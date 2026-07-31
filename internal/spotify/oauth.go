// Package spotify implements Spotify Web API access for the broadcaster's account.
// Tokens are persisted to ./tokens.spotify.json (gitignored) and refreshed automatically.
package spotify

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Required scopes for now-playing widget + song request queueing.
var Scopes = []string{
	"user-read-currently-playing",
	"user-read-playback-state",
	"user-modify-playback-state",
}

const tokenFilePath = "./tokens.spotify.json"

// TokenData is the persisted Spotify OAuth token state.
type TokenData struct {
	AccessToken         string   `json:"accessToken"`
	RefreshToken        string   `json:"refreshToken"`
	ExpiresIn           int      `json:"expiresIn"`
	ObtainmentTimestamp int64    `json:"obtainmentTimestamp"`
	Scope               []string `json:"scope,omitempty"`
}

// IsExpired returns true if the token is expired or within 60s of expiring.
func (td *TokenData) IsExpired() bool {
	if td.ExpiresIn == 0 || td.ObtainmentTimestamp == 0 {
		return true
	}
	expiry := time.UnixMilli(td.ObtainmentTimestamp).Add(time.Duration(td.ExpiresIn) * time.Second)
	return time.Now().After(expiry.Add(-60 * time.Second))
}

// TokenManager handles Spotify OAuth state and refresh.
type TokenManager struct {
	clientID     string
	clientSecret string
	redirectURI  string
	httpClient   *http.Client

	mu    sync.RWMutex
	token *TokenData
}

// NewTokenManager creates a Spotify token manager. Call LoadTokenFile to restore
// state from disk; if no token exists, the manager is "unauthorized" until the
// broadcaster completes the OAuth flow via HandleCallback.
func NewTokenManager(clientID, clientSecret, redirectURI string) *TokenManager {
	return &TokenManager{
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURI:  redirectURI,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
}

// LoadTokenFile attempts to load tokens.spotify.json. Returns nil if the file
// doesn't exist (broadcaster hasn't authorized yet).
func (tm *TokenManager) LoadTokenFile() error {
	data, err := os.ReadFile(tokenFilePath)
	if os.IsNotExist(err) {
		slog.Info("no Spotify token file yet - broadcaster must authorize via /api/spotify/auth")
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading spotify token file: %w", err)
	}

	var td TokenData
	if err := json.Unmarshal(data, &td); err != nil {
		return fmt.Errorf("parsing spotify token file: %w", err)
	}

	tm.mu.Lock()
	tm.token = &td
	tm.mu.Unlock()

	slog.Info("spotify token loaded")
	return nil
}

// IsAuthorized reports whether a refresh token is available.
func (tm *TokenManager) IsAuthorized() bool {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.token != nil && tm.token.RefreshToken != ""
}

// AuthorizeURL returns the Spotify OAuth consent URL for the broadcaster.
// state is echoed back to the callback for CSRF protection.
func (tm *TokenManager) AuthorizeURL(state string) string {
	q := url.Values{
		"client_id":     {tm.clientID},
		"response_type": {"code"},
		"redirect_uri":  {tm.redirectURI},
		"scope":         {strings.Join(Scopes, " ")},
		"state":         {state},
	}
	return "https://accounts.spotify.com/authorize?" + q.Encode()
}

// HandleCallback exchanges an authorization code for tokens and persists them.
func (tm *TokenManager) HandleCallback(code string) error {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {tm.redirectURI},
	}

	req, err := http.NewRequest(http.MethodPost, "https://accounts.spotify.com/api/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+basicAuth(tm.clientID, tm.clientSecret))

	resp, err := tm.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, body)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parsing token response: %w", err)
	}

	td := &TokenData{
		AccessToken:         result.AccessToken,
		RefreshToken:        result.RefreshToken,
		ExpiresIn:           result.ExpiresIn,
		ObtainmentTimestamp: time.Now().UnixMilli(),
		Scope:               strings.Fields(result.Scope),
	}

	tm.mu.Lock()
	tm.token = td
	tm.mu.Unlock()

	if err := tm.saveToFile(td); err != nil {
		return fmt.Errorf("persist spotify token: %w", err)
	}
	slog.Info("spotify authorization complete", "scopes", td.Scope)
	return nil
}

// GetAccessToken returns a valid access token, refreshing if necessary.
func (tm *TokenManager) GetAccessToken() (string, error) {
	tm.mu.RLock()
	td := tm.token
	tm.mu.RUnlock()

	if td == nil || td.RefreshToken == "" {
		return "", fmt.Errorf("spotify not authorized - visit /api/spotify/auth")
	}
	if !td.IsExpired() {
		return td.AccessToken, nil
	}
	return tm.refresh(td.RefreshToken)
}

func (tm *TokenManager) refresh(refreshToken string) (string, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}

	req, err := http.NewRequest(http.MethodPost, "https://accounts.spotify.com/api/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+basicAuth(tm.clientID, tm.clientSecret))

	resp, err := tm.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("refresh: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refresh failed (%d): %s", resp.StatusCode, body)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"` // optional - Spotify may not rotate it
		ExpiresIn    int    `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing refresh response: %w", err)
	}

	newRefresh := result.RefreshToken
	if newRefresh == "" {
		newRefresh = refreshToken
	}

	td := &TokenData{
		AccessToken:         result.AccessToken,
		RefreshToken:        newRefresh,
		ExpiresIn:           result.ExpiresIn,
		ObtainmentTimestamp: time.Now().UnixMilli(),
		Scope:               strings.Fields(result.Scope),
	}

	tm.mu.Lock()
	tm.token = td
	tm.mu.Unlock()

	if err := tm.saveToFile(td); err != nil {
		slog.Error("failed to persist refreshed spotify token", "error", err)
	}
	slog.Info("spotify token refreshed")
	return td.AccessToken, nil
}

func (tm *TokenManager) saveToFile(td *TokenData) error {
	data, err := json.MarshalIndent(td, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(tokenFilePath, data, 0600)
}

func basicAuth(id, secret string) string {
	return base64.StdEncoding.EncodeToString([]byte(id + ":" + secret))
}
