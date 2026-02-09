package twitch

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// TokenData matches Twurple's JSON token format for cross-compatibility
// with the existing JS bot token files.
type TokenData struct {
	AccessToken         string   `json:"accessToken"`
	RefreshToken        string   `json:"refreshToken"`
	ExpiresIn           int      `json:"expiresIn"`
	ObtainmentTimestamp int64    `json:"obtainmentTimestamp"`
	Scope               []string `json:"scope,omitempty"`
}

// IsExpired returns true if the token is expired or will expire within 5 minutes.
func (td *TokenData) IsExpired() bool {
	if td.ExpiresIn == 0 || td.ObtainmentTimestamp == 0 {
		return false
	}
	expiry := time.UnixMilli(td.ObtainmentTimestamp).Add(time.Duration(td.ExpiresIn) * time.Second)
	return time.Now().After(expiry.Add(-5 * time.Minute))
}

// TokenManager handles loading, refreshing, and storing OAuth tokens.
type TokenManager struct {
	mu           sync.RWMutex
	tokens       map[string]*TokenData
	clientID     string
	clientSecret string
	httpClient   *http.Client

	appMu       sync.RWMutex
	appToken    string
	appTokenExp time.Time
}

// NewTokenManager creates a new token manager.
func NewTokenManager(clientID, clientSecret string) *TokenManager {
	return &TokenManager{
		tokens:       make(map[string]*TokenData),
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
}

// LoadTokenFile loads a Twurple-format token file for a user ID.
// Expects files at ./tokens.{userID}.json (same location as the JS bot).
func (tm *TokenManager) LoadTokenFile(userID string) error {
	path := fmt.Sprintf("./tokens.%s.json", userID)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading token file %s: %w", path, err)
	}

	var td TokenData
	if err := json.Unmarshal(data, &td); err != nil {
		return fmt.Errorf("parsing token file %s: %w", path, err)
	}

	tm.mu.Lock()
	tm.tokens[userID] = &td
	tm.mu.Unlock()

	slog.Info("token loaded", "user_id", userID)
	return nil
}

// GetAccessToken returns a valid access token for the given user, refreshing if necessary.
func (tm *TokenManager) GetAccessToken(userID string) (string, error) {
	tm.mu.RLock()
	td, ok := tm.tokens[userID]
	tm.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("no token loaded for user %s", userID)
	}

	if !td.IsExpired() {
		return td.AccessToken, nil
	}

	slog.Info("refreshing expired token", "user_id", userID)
	return tm.refreshToken(userID, td)
}

// refreshToken calls Twitch's OAuth2 token refresh endpoint.
func (tm *TokenManager) refreshToken(userID string, td *TokenData) (string, error) {
	form := url.Values{
		"client_id":     {tm.clientID},
		"client_secret": {tm.clientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {td.RefreshToken},
	}

	resp, err := tm.httpClient.PostForm("https://id.twitch.tv/oauth2/token", form)
	if err != nil {
		return "", fmt.Errorf("token refresh request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token refresh failed (status %d): %s", resp.StatusCode, body)
	}

	// Twitch API returns snake_case
	var result struct {
		AccessToken  string   `json:"access_token"`
		RefreshToken string   `json:"refresh_token"`
		ExpiresIn    int      `json:"expires_in"`
		Scope        []string `json:"scope"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing refresh response: %w", err)
	}

	// Update token data in Twurple format for file compatibility
	newTD := &TokenData{
		AccessToken:         result.AccessToken,
		RefreshToken:        result.RefreshToken,
		ExpiresIn:           result.ExpiresIn,
		ObtainmentTimestamp: time.Now().UnixMilli(),
		Scope:               result.Scope,
	}

	tm.mu.Lock()
	tm.tokens[userID] = newTD
	tm.mu.Unlock()

	// Save to file so JS bot can also use the refreshed token
	if err := tm.saveTokenFile(userID, newTD); err != nil {
		slog.Error("failed to save refreshed token", "error", err, "user_id", userID)
	}

	slog.Info("token refreshed", "user_id", userID)
	return newTD.AccessToken, nil
}

// saveTokenFile writes a token back to disk in Twurple format.
func (tm *TokenManager) saveTokenFile(userID string, td *TokenData) error {
	path := fmt.Sprintf("./tokens.%s.json", userID)
	data, err := json.MarshalIndent(td, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// GetAppAccessToken returns a valid app access token via client credentials flow.
// Required for EventSub webhook subscription creation.
func (tm *TokenManager) GetAppAccessToken() (string, error) {
	tm.appMu.RLock()
	if tm.appToken != "" && time.Now().Before(tm.appTokenExp) {
		token := tm.appToken
		tm.appMu.RUnlock()
		return token, nil
	}
	tm.appMu.RUnlock()

	form := url.Values{
		"client_id":     {tm.clientID},
		"client_secret": {tm.clientSecret},
		"grant_type":    {"client_credentials"},
	}

	resp, err := tm.httpClient.PostForm("https://id.twitch.tv/oauth2/token", form)
	if err != nil {
		return "", fmt.Errorf("app token request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("app token request failed (status %d): %s", resp.StatusCode, body)
	}

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing app token response: %w", err)
	}

	tm.appMu.Lock()
	tm.appToken = result.AccessToken
	tm.appTokenExp = time.Now().Add(time.Duration(result.ExpiresIn)*time.Second - 5*time.Minute)
	tm.appMu.Unlock()

	slog.Info("app access token obtained")
	return result.AccessToken, nil
}

// GetClientID returns the OAuth client ID.
func (tm *TokenManager) GetClientID() string {
	return tm.clientID
}
