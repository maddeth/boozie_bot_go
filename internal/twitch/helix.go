package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

const helixBaseURL = "https://api.twitch.tv/helix"

// TwitchUser represents a Twitch user from the Helix API.
type TwitchUser struct {
	ID              string `json:"id"`
	Login           string `json:"login"`
	DisplayName     string `json:"display_name"`
	BroadcasterType string `json:"broadcaster_type"`
	Description     string `json:"description"`
	ProfileImageURL string `json:"profile_image_url"`
}

// TwitchStream represents an active stream from the Helix API.
type TwitchStream struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	UserLogin   string `json:"user_login"`
	GameName    string `json:"game_name"`
	Type        string `json:"type"` // "live" or ""
	Title       string `json:"title"`
	ViewerCount int    `json:"viewer_count"`
}

// HelixClient makes raw HTTP calls to the Twitch Helix API.
type HelixClient struct {
	tokenMgr   *TokenManager
	streamerID string
	botUserID  string
	httpClient *http.Client
}

// NewHelixClient creates a new Helix API client.
func NewHelixClient(tokenMgr *TokenManager, streamerID, botUserID string) *HelixClient {
	return &HelixClient{
		tokenMgr:   tokenMgr,
		streamerID: streamerID,
		botUserID:  botUserID,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// helixGet performs an authenticated GET to the Helix API using the given user's token.
func (h *HelixClient) helixGet(ctx context.Context, tokenUserID, path string, params url.Values) ([]byte, error) {
	token, err := h.tokenMgr.GetAccessToken(tokenUserID)
	if err != nil {
		return nil, fmt.Errorf("getting access token: %w", err)
	}

	u := helixBaseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Client-Id", h.tokenMgr.GetClientID())

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("helix %s returned %d: %s", path, resp.StatusCode, body)
	}
	return body, nil
}

// GetUserByName looks up a Twitch user by login name.
func (h *HelixClient) GetUserByName(ctx context.Context, login string) (*TwitchUser, error) {
	body, err := h.helixGet(ctx, h.botUserID, "/users", url.Values{"login": {login}})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data []TwitchUser `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, nil
	}
	return &resp.Data[0], nil
}

// GetUserByID looks up a Twitch user by user ID.
func (h *HelixClient) GetUserByID(ctx context.Context, userID string) (*TwitchUser, error) {
	body, err := h.helixGet(ctx, h.botUserID, "/users", url.Values{"id": {userID}})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data []TwitchUser `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, nil
	}
	return &resp.Data[0], nil
}

// GetStream checks if a user is currently streaming.
func (h *HelixClient) GetStream(ctx context.Context, userID string) (*TwitchStream, error) {
	body, err := h.helixGet(ctx, h.botUserID, "/streams", url.Values{"user_id": {userID}})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Data []TwitchStream `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, nil
	}
	return &resp.Data[0], nil
}

// GetChatters fetches the current chatters list. Returns a map of displayName -> userID.
func (h *HelixClient) GetChatters(ctx context.Context) (map[string]string, error) {
	chatters := make(map[string]string)
	cursor := ""

	for {
		params := url.Values{
			"broadcaster_id": {h.streamerID},
			"moderator_id":   {h.botUserID},
			"first":          {"1000"},
		}
		if cursor != "" {
			params.Set("after", cursor)
		}

		body, err := h.helixGet(ctx, h.botUserID, "/chat/chatters", params)
		if err != nil {
			return chatters, err
		}

		var resp struct {
			Data []struct {
				UserID    string `json:"user_id"`
				UserLogin string `json:"user_login"`
				UserName  string `json:"user_name"`
			} `json:"data"`
			Pagination struct {
				Cursor string `json:"cursor"`
			} `json:"pagination"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return chatters, err
		}

		for _, c := range resp.Data {
			chatters[c.UserName] = c.UserID
			chatters[c.UserLogin] = c.UserID
		}

		if resp.Pagination.Cursor == "" || len(resp.Data) == 0 {
			break
		}
		cursor = resp.Pagination.Cursor
	}

	slog.Info("chatters fetched", "count", len(chatters))
	return chatters, nil
}

// GetSubscription checks if a user is subscribed. Returns the tier ("1", "2", "3") or "0".
// Returns an error if the API call or response parsing fails (previously swallowed silently).
func (h *HelixClient) GetSubscription(ctx context.Context, userID string) (string, error) {
	body, err := h.helixGet(ctx, h.streamerID, "/subscriptions", url.Values{
		"broadcaster_id": {h.streamerID},
		"user_id":        {userID},
	})
	if err != nil {
		return "0", fmt.Errorf("subscription lookup for %s: %w", userID, err)
	}

	var resp struct {
		Data []struct {
			Tier string `json:"tier"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "0", fmt.Errorf("parsing subscription response for %s: %w", userID, err)
	}
	if len(resp.Data) == 0 {
		return "0", nil // Not subscribed — this is expected
	}

	// Twitch returns "1000", "2000", "3000" — convert to "1", "2", "3"
	tier := resp.Data[0].Tier
	if len(tier) >= 1 {
		return tier[:1], nil
	}
	return "0", nil
}

// SendShoutout sends a Twitch shoutout API call.
func (h *HelixClient) SendShoutout(ctx context.Context, toUserID string) error {
	token, err := h.tokenMgr.GetAccessToken(h.streamerID)
	if err != nil {
		return err
	}

	params := url.Values{
		"from_broadcaster_id": {h.streamerID},
		"to_broadcaster_id":   {toUserID},
		"moderator_id":        {h.botUserID},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, helixBaseURL+"/chat/shoutouts?"+params.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Client-Id", h.tokenMgr.GetClientID())

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("shoutout API returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

// TwitchModerator represents a moderator from the Helix API.
type TwitchModerator struct {
	UserID      string `json:"user_id"`
	UserLogin   string `json:"user_login"`
	UserName    string `json:"user_name"`
}

// GetModerators fetches all moderators for the channel (paginated).
func (h *HelixClient) GetModerators(ctx context.Context) ([]TwitchModerator, error) {
	var mods []TwitchModerator
	cursor := ""

	for {
		params := url.Values{
			"broadcaster_id": {h.streamerID},
			"first":          {"100"},
		}
		if cursor != "" {
			params.Set("after", cursor)
		}

		body, err := h.helixGet(ctx, h.streamerID, "/moderation/moderators", params)
		if err != nil {
			return mods, err
		}

		var resp struct {
			Data       []TwitchModerator `json:"data"`
			Pagination struct {
				Cursor string `json:"cursor"`
			} `json:"pagination"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return mods, err
		}

		mods = append(mods, resp.Data...)

		if resp.Pagination.Cursor == "" || len(resp.Data) == 0 {
			break
		}
		cursor = resp.Pagination.Cursor
	}

	slog.Info("moderators fetched from Twitch", "count", len(mods))
	return mods, nil
}

// IsStreamLive returns true if the streamer is currently live.
func (h *HelixClient) IsStreamLive(ctx context.Context) (bool, error) {
	stream, err := h.GetStream(ctx, h.streamerID)
	if err != nil {
		return false, err
	}
	return stream != nil && stream.Type == "live", nil
}
