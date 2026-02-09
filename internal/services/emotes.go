package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Emote represents a single emote from any provider.
type Emote struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	URL      string `json:"url"`
	Provider string `json:"provider"`
}

// EmoteService fetches and caches emotes from Twitch, BTTV, and 7TV.
type EmoteService struct {
	channelID  string
	clientID   string
	httpClient *http.Client

	// tokenFunc returns a current access token for Twitch API calls.
	tokenFunc func() (string, error)

	mu            sync.RWMutex
	emotesMap     map[string]Emote // name -> emote (fast lookup)
	allEmotes     []Emote          // ordered list for getAllEmotes
	lastFetch     time.Time
	cacheDuration time.Duration
}

// NewEmoteService creates an emote service for the given channel.
// tokenFunc should return a valid Twitch access token (e.g. from TokenManager).
func NewEmoteService(channelID, clientID string, tokenFunc func() (string, error)) *EmoteService {
	return &EmoteService{
		channelID:     channelID,
		clientID:      clientID,
		tokenFunc:     tokenFunc,
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		emotesMap:     make(map[string]Emote),
		cacheDuration: 1 * time.Hour,
	}
}

// LoadAllEmotes fetches emotes from all three providers (Twitch, BTTV, 7TV) in parallel.
// Results are cached for 1 hour.
func (s *EmoteService) LoadAllEmotes(ctx context.Context) error {
	s.mu.RLock()
	if time.Since(s.lastFetch) < s.cacheDuration {
		s.mu.RUnlock()
		slog.Info("using cached emotes")
		return nil
	}
	s.mu.RUnlock()

	slog.Info("fetching emotes from all providers...")

	type result struct {
		emotes []Emote
		err    error
		name   string
	}

	ch := make(chan result, 3)

	go func() {
		emotes, err := s.fetchTwitchEmotes(ctx)
		ch <- result{emotes, err, "twitch"}
	}()
	go func() {
		emotes, err := s.fetchBTTVEmotes(ctx)
		ch <- result{emotes, err, "bttv"}
	}()
	go func() {
		emotes, err := s.fetch7TVEmotes(ctx)
		ch <- result{emotes, err, "7tv"}
	}()

	// Collect results: globals first, then channel (so channel overrides global).
	var allEmotes []Emote
	for i := 0; i < 3; i++ {
		r := <-ch
		if r.err != nil {
			slog.Error("error fetching emotes", "provider", r.name, "error", r.err)
			continue
		}
		allEmotes = append(allEmotes, r.emotes...)
	}

	// Build lookup map (later entries override earlier, so channel > global).
	emotesMap := make(map[string]Emote, len(allEmotes))
	for _, e := range allEmotes {
		emotesMap[e.Name] = e
	}

	// Build deduplicated list from map.
	deduped := make([]Emote, 0, len(emotesMap))
	for _, e := range emotesMap {
		deduped = append(deduped, e)
	}

	s.mu.Lock()
	s.emotesMap = emotesMap
	s.allEmotes = deduped
	s.lastFetch = time.Now()
	s.mu.Unlock()

	slog.Info("emotes loaded", "total", len(emotesMap))
	return nil
}

// GetAllEmotes returns all cached emotes.
func (s *EmoteService) GetAllEmotes() []Emote {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.allEmotes
}

// GetEmoteByName looks up an emote by its name.
func (s *EmoteService) GetEmoteByName(name string) (Emote, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.emotesMap[name]
	return e, ok
}

// ParseMessage splits a message into emote and text segments.
func (s *EmoteService) ParseMessage(message string) []MessageSegment {
	words := strings.Fields(message)
	segments := make([]MessageSegment, 0, len(words))

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, word := range words {
		if emote, ok := s.emotesMap[word]; ok {
			segments = append(segments, MessageSegment{Type: "emote", Content: emote})
		} else {
			segments = append(segments, MessageSegment{Type: "text", Content: word})
		}
	}
	return segments
}

// MessageSegment represents either a text word or an emote in a parsed message.
type MessageSegment struct {
	Type    string `json:"type"`
	Content any    `json:"content"`
}

// --- Twitch emotes ---

func (s *EmoteService) fetchTwitchEmotes(ctx context.Context) ([]Emote, error) {
	token, err := s.tokenFunc()
	if err != nil {
		return nil, fmt.Errorf("getting access token: %w", err)
	}

	var emotes []Emote

	// Global emotes
	globalBody, err := s.twitchGet(ctx, token, "https://api.twitch.tv/helix/chat/emotes/global")
	if err != nil {
		slog.Error("error fetching global Twitch emotes", "error", err)
	} else {
		var resp struct {
			Data []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"data"`
		}
		if json.Unmarshal(globalBody, &resp) == nil {
			for _, e := range resp.Data {
				emotes = append(emotes, Emote{
					Name:     e.Name,
					ID:       e.ID,
					URL:      fmt.Sprintf("https://static-cdn.jtvnw.net/emoticons/v2/%s/default/dark/2.0", e.ID),
					Provider: "twitch",
				})
			}
			slog.Info("loaded Twitch global emotes", "count", len(resp.Data))
		}
	}

	// Channel emotes
	channelBody, err := s.twitchGet(ctx, token,
		fmt.Sprintf("https://api.twitch.tv/helix/chat/emotes?broadcaster_id=%s", s.channelID))
	if err != nil {
		slog.Error("error fetching channel Twitch emotes", "error", err)
	} else {
		var resp struct {
			Data []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"data"`
		}
		if json.Unmarshal(channelBody, &resp) == nil {
			for _, e := range resp.Data {
				emotes = append(emotes, Emote{
					Name:     e.Name,
					ID:       e.ID,
					URL:      fmt.Sprintf("https://static-cdn.jtvnw.net/emoticons/v2/%s/default/dark/2.0", e.ID),
					Provider: "twitch",
				})
			}
			slog.Info("loaded Twitch channel emotes", "count", len(resp.Data))
		}
	}

	return emotes, nil
}

func (s *EmoteService) twitchGet(ctx context.Context, token, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Client-Id", s.clientID)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("twitch emotes API returned %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

// --- BTTV emotes ---

func (s *EmoteService) fetchBTTVEmotes(ctx context.Context) ([]Emote, error) {
	var emotes []Emote

	// Global BTTV emotes
	globalBody, err := s.httpGet(ctx, "https://api.betterttv.net/3/cached/emotes/global")
	if err != nil {
		slog.Error("error fetching global BTTV emotes", "error", err)
	} else {
		var data []struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		}
		if json.Unmarshal(globalBody, &data) == nil {
			for _, e := range data {
				emotes = append(emotes, Emote{
					Name:     e.Code,
					ID:       e.ID,
					URL:      fmt.Sprintf("https://cdn.betterttv.net/emote/%s/2x", e.ID),
					Provider: "bttv",
				})
			}
			slog.Info("loaded BTTV global emotes", "count", len(data))
		}
	}

	// Channel BTTV emotes
	channelBody, err := s.httpGet(ctx,
		fmt.Sprintf("https://api.betterttv.net/3/cached/users/twitch/%s", s.channelID))
	if err != nil {
		slog.Error("error fetching channel BTTV emotes", "error", err)
	} else {
		var resp struct {
			ChannelEmotes []struct {
				ID   string `json:"id"`
				Code string `json:"code"`
			} `json:"channelEmotes"`
			SharedEmotes []struct {
				ID   string `json:"id"`
				Code string `json:"code"`
			} `json:"sharedEmotes"`
		}
		if json.Unmarshal(channelBody, &resp) == nil {
			channelCount := 0
			for _, e := range resp.ChannelEmotes {
				emotes = append(emotes, Emote{
					Name:     e.Code,
					ID:       e.ID,
					URL:      fmt.Sprintf("https://cdn.betterttv.net/emote/%s/2x", e.ID),
					Provider: "bttv",
				})
				channelCount++
			}
			for _, e := range resp.SharedEmotes {
				emotes = append(emotes, Emote{
					Name:     e.Code,
					ID:       e.ID,
					URL:      fmt.Sprintf("https://cdn.betterttv.net/emote/%s/2x", e.ID),
					Provider: "bttv",
				})
				channelCount++
			}
			slog.Info("loaded BTTV channel emotes", "count", channelCount)
		}
	}

	return emotes, nil
}

// --- 7TV emotes ---

func (s *EmoteService) fetch7TVEmotes(ctx context.Context) ([]Emote, error) {
	var emotes []Emote

	// Global 7TV emotes
	globalBody, err := s.httpGet(ctx, "https://7tv.io/v3/emote-sets/global")
	if err != nil {
		slog.Error("error fetching global 7TV emotes", "error", err)
	} else {
		var resp struct {
			Emotes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"emotes"`
		}
		if json.Unmarshal(globalBody, &resp) == nil {
			for _, e := range resp.Emotes {
				emotes = append(emotes, Emote{
					Name:     e.Name,
					ID:       e.ID,
					URL:      fmt.Sprintf("https://cdn.7tv.app/emote/%s/2x.webp", e.ID),
					Provider: "7tv",
				})
			}
			slog.Info("loaded 7TV global emotes", "count", len(resp.Emotes))
		}
	}

	// Channel 7TV emotes
	channelBody, err := s.httpGet(ctx,
		fmt.Sprintf("https://7tv.io/v3/users/twitch/%s", s.channelID))
	if err != nil {
		slog.Error("error fetching channel 7TV emotes", "error", err)
	} else {
		var resp struct {
			EmoteSet struct {
				Emotes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"emotes"`
			} `json:"emote_set"`
		}
		if json.Unmarshal(channelBody, &resp) == nil {
			for _, e := range resp.EmoteSet.Emotes {
				emotes = append(emotes, Emote{
					Name:     e.Name,
					ID:       e.ID,
					URL:      fmt.Sprintf("https://cdn.7tv.app/emote/%s/2x.webp", e.ID),
					Provider: "7tv",
				})
			}
			slog.Info("loaded 7TV channel emotes", "count", len(resp.EmoteSet.Emotes))
		}
	}

	return emotes, nil
}

// --- HTTP helper ---

func (s *EmoteService) httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, body)
	}
	return body, nil
}
