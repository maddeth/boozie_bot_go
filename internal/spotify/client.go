package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const apiBase = "https://api.spotify.com/v1"

// Track is the minimal track representation broadcast to the frontend and shown in chat.
type Track struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Artists    []string `json:"artists"`
	Album      string   `json:"album"`
	AlbumArt   string   `json:"albumArt"` // largest image URL, may be ""
	DurationMS int      `json:"durationMs"`
	URL        string   `json:"url"`           // open.spotify.com URL
	URI        string   `json:"uri,omitempty"` // spotify:track:... (used internally for queue)
	Explicit   bool     `json:"explicit"`
}

// NowPlaying represents the broadcaster's current playback state.
type NowPlaying struct {
	IsPlaying  bool   `json:"isPlaying"`
	ProgressMS int    `json:"progressMs"`
	Track      *Track `json:"track,omitempty"` // nil when nothing is playing
}

// ErrNothingPlaying is returned by GetCurrentlyPlaying when Spotify returns 204
// (no active device or playback paused for a long time).
var ErrNothingPlaying = errors.New("nothing playing")

// ErrNoActiveDevice is returned by AddToQueue when there's no active Spotify device.
var ErrNoActiveDevice = errors.New("no active spotify device")

// ErrTrackNotFound is returned by SearchTrack when no matches exist.
var ErrTrackNotFound = errors.New("track not found")

// Client wraps the Spotify Web API. All requests use the broadcaster's user token.
type Client struct {
	tokens     *TokenManager
	httpClient *http.Client
}

// NewClient creates a new Spotify API client.
func NewClient(tokens *TokenManager) *Client {
	return &Client{
		tokens:     tokens,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// GetCurrentlyPlaying returns the broadcaster's current playback state.
// Returns (nil, ErrNothingPlaying) when Spotify reports 204 No Content.
func (c *Client) GetCurrentlyPlaying(ctx context.Context) (*NowPlaying, error) {
	body, status, err := c.do(ctx, http.MethodGet, apiBase+"/me/player/currently-playing", nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, ErrNothingPlaying
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("currently-playing failed (%d): %s", status, body)
	}

	var raw struct {
		IsPlaying  bool `json:"is_playing"`
		ProgressMS int  `json:"progress_ms"`
		Item       *struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			DurationMS   int    `json:"duration_ms"`
			Explicit     bool   `json:"explicit"`
			URI          string `json:"uri"`
			ExternalURLs struct {
				Spotify string `json:"spotify"`
			} `json:"external_urls"`
			Artists []struct {
				Name string `json:"name"`
			} `json:"artists"`
			Album struct {
				Name   string `json:"name"`
				Images []struct {
					URL    string `json:"url"`
					Width  int    `json:"width"`
					Height int    `json:"height"`
				} `json:"images"`
			} `json:"album"`
		} `json:"item"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse currently-playing: %w", err)
	}

	np := &NowPlaying{IsPlaying: raw.IsPlaying, ProgressMS: raw.ProgressMS}
	if raw.Item == nil {
		return np, nil
	}

	artists := make([]string, len(raw.Item.Artists))
	for i, a := range raw.Item.Artists {
		artists[i] = a.Name
	}

	np.Track = &Track{
		ID:         raw.Item.ID,
		Name:       raw.Item.Name,
		Artists:    artists,
		Album:      raw.Item.Album.Name,
		AlbumArt:   pickLargestImage(raw.Item.Album.Images),
		DurationMS: raw.Item.DurationMS,
		URL:        raw.Item.ExternalURLs.Spotify,
		URI:        raw.Item.URI,
		Explicit:   raw.Item.Explicit,
	}
	return np, nil
}

// trackURIRE matches Spotify track URIs/URLs and captures the ID.
var trackURIRE = regexp.MustCompile(`(?:spotify:track:|open\.spotify\.com/track/)([A-Za-z0-9]+)`)

// ResolveTrack accepts a Spotify URL, URI, raw ID, or free-text search query and
// returns the resulting Track. Used by the !sr command.
func (c *Client) ResolveTrack(ctx context.Context, input string) (*Track, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("empty query")
	}

	// Strip query string from Spotify share links (?si=...)
	if idx := strings.Index(input, "?"); idx > 0 && strings.Contains(input[:idx], "spotify") {
		input = input[:idx]
	}

	if m := trackURIRE.FindStringSubmatch(input); len(m) == 2 {
		return c.GetTrack(ctx, m[1])
	}
	// 22-char alphanumeric = bare Spotify ID
	if len(input) == 22 && isAlphaNum(input) {
		return c.GetTrack(ctx, input)
	}
	return c.SearchTrack(ctx, input)
}

// GetTrack fetches track metadata by ID.
func (c *Client) GetTrack(ctx context.Context, trackID string) (*Track, error) {
	body, status, err := c.do(ctx, http.MethodGet, apiBase+"/tracks/"+url.PathEscape(trackID), nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, ErrTrackNotFound
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("get track failed (%d): %s", status, body)
	}
	return parseTrack(body)
}

// searchLimit is how many candidates we fetch and locally re-rank. Spotify's
// raw search relevance leans on popularity for tie-breaks, so the best literal
// title match isn't always #1 (e.g. "Nessa Barrett - S.L.U.T." returning her
// more popular "la di die"). We pull a handful and re-rank by query coverage.
const searchLimit = 10

// SearchTrack returns the best match for a free-text query. It fetches several
// candidates and picks the one whose title and artists best cover the query
// tokens (order- and punctuation-independent), falling back to Spotify's own
// ordering when candidates tie.
func (c *Client) SearchTrack(ctx context.Context, query string) (*Track, error) {
	q := url.Values{
		"q":     {query},
		"type":  {"track"},
		"limit": {strconv.Itoa(searchLimit)},
	}
	body, status, err := c.do(ctx, http.MethodGet, apiBase+"/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("search failed (%d): %s", status, body)
	}

	var raw struct {
		Tracks struct {
			Items []json.RawMessage `json:"items"`
		} `json:"tracks"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse search response: %w", err)
	}
	if len(raw.Tracks.Items) == 0 {
		return nil, ErrTrackNotFound
	}

	want := tokenize(query)
	var best *Track
	bestScore := -1.0
	for _, item := range raw.Tracks.Items {
		t, err := parseTrack(item)
		if err != nil {
			continue // skip malformed candidates rather than fail the whole search
		}
		// Strictly-greater keeps Spotify's original order on ties, so generic
		// queries (e.g. just an artist name) still return the popular pick.
		if score := queryCoverage(want, t); score > bestScore {
			best, bestScore = t, score
		}
	}
	if best == nil {
		return nil, ErrTrackNotFound
	}
	return best, nil
}

// tokenSplitRE splits on any run of non-alphanumeric characters.
var tokenSplitRE = regexp.MustCompile(`[^a-z0-9]+`)

// tokenize lowercases s and splits it into alphanumeric words. Both sides of a
// comparison go through this, so "S.L.U.T." -> [s l u t] on the query and the
// title alike, letting them match despite the punctuation.
func tokenize(s string) []string {
	var out []string
	for _, f := range tokenSplitRE.Split(strings.ToLower(s), -1) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// queryCoverage returns the fraction of query tokens that appear among a
// track's title and artist tokens - how well the candidate accounts for what
// the user typed, irrespective of word order.
func queryCoverage(want []string, t *Track) float64 {
	if len(want) == 0 {
		return 0
	}
	have := make(map[string]bool)
	for _, tok := range tokenize(t.Name) {
		have[tok] = true
	}
	for _, a := range t.Artists {
		for _, tok := range tokenize(a) {
			have[tok] = true
		}
	}
	matched := 0
	for _, tok := range want {
		if have[tok] {
			matched++
		}
	}
	return float64(matched) / float64(len(want))
}

// AddToQueue adds a track URI to the broadcaster's playback queue.
// Returns ErrNoActiveDevice when Spotify reports 404 (no device active).
func (c *Client) AddToQueue(ctx context.Context, trackURI string) error {
	q := url.Values{"uri": {trackURI}}
	body, status, err := c.do(ctx, http.MethodPost, apiBase+"/me/player/queue?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrNoActiveDevice
	default:
		return fmt.Errorf("add to queue failed (%d): %s", status, body)
	}
}

// GetQueue returns the user's current queue: the currently playing track
// followed by the upcoming tracks in the queue.
func (c *Client) GetQueue(ctx context.Context) (currentlyPlaying *Track, queue []*Track, err error) {
	body, status, err := c.do(ctx, http.MethodGet, apiBase+"/me/player/queue", nil)
	if err != nil {
		return nil, nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil, ErrNothingPlaying
	}
	if status != http.StatusOK {
		return nil, nil, fmt.Errorf("get queue failed (%d): %s", status, body)
	}

	var raw struct {
		CurrentlyPlaying *struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			DurationMS   int    `json:"duration_ms"`
			Explicit     bool   `json:"explicit"`
			URI          string `json:"uri"`
			ExternalURLs struct {
				Spotify string `json:"spotify"`
			} `json:"external_urls"`
			Artists []struct {
				Name string `json:"name"`
			} `json:"artists"`
			Album struct {
				Name   string `json:"name"`
				Images []struct {
					URL    string `json:"url"`
					Width  int    `json:"width"`
					Height int    `json:"height"`
				} `json:"images"`
			} `json:"album"`
		} `json:"currently_playing"`
		Queue []json.RawMessage `json:"queue"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, fmt.Errorf("parse queue response: %w", err)
	}

	if raw.CurrentlyPlaying != nil {
		artists := make([]string, len(raw.CurrentlyPlaying.Artists))
		for i, a := range raw.CurrentlyPlaying.Artists {
			artists[i] = a.Name
		}
		currentlyPlaying = &Track{
			ID:         raw.CurrentlyPlaying.ID,
			Name:       raw.CurrentlyPlaying.Name,
			Artists:    artists,
			Album:      raw.CurrentlyPlaying.Album.Name,
			AlbumArt:   pickLargestImage(raw.CurrentlyPlaying.Album.Images),
			DurationMS: raw.CurrentlyPlaying.DurationMS,
			URL:        raw.CurrentlyPlaying.ExternalURLs.Spotify,
			URI:        raw.CurrentlyPlaying.URI,
			Explicit:   raw.CurrentlyPlaying.Explicit,
		}
	}

	for _, item := range raw.Queue {
		t, err := parseTrack(item)
		if err != nil {
			continue
		}
		queue = append(queue, t)
	}

	return currentlyPlaying, queue, nil
}

// do is the common HTTP wrapper - handles auth, body read, and status return.
func (c *Client) do(ctx context.Context, method, urlStr string, payload io.Reader) ([]byte, int, error) {
	token, err := c.tokens.GetAccessToken()
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, payload)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

func parseTrack(data []byte) (*Track, error) {
	var raw struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		DurationMS   int    `json:"duration_ms"`
		Explicit     bool   `json:"explicit"`
		URI          string `json:"uri"`
		ExternalURLs struct {
			Spotify string `json:"spotify"`
		} `json:"external_urls"`
		Artists []struct {
			Name string `json:"name"`
		} `json:"artists"`
		Album struct {
			Name   string `json:"name"`
			Images []struct {
				URL    string `json:"url"`
				Width  int    `json:"width"`
				Height int    `json:"height"`
			} `json:"images"`
		} `json:"album"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse track: %w", err)
	}

	artists := make([]string, len(raw.Artists))
	for i, a := range raw.Artists {
		artists[i] = a.Name
	}

	return &Track{
		ID:         raw.ID,
		Name:       raw.Name,
		Artists:    artists,
		Album:      raw.Album.Name,
		AlbumArt:   pickLargestImage(raw.Album.Images),
		DurationMS: raw.DurationMS,
		URL:        raw.ExternalURLs.Spotify,
		URI:        raw.URI,
		Explicit:   raw.Explicit,
	}, nil
}

func pickLargestImage(images []struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}) string {
	var best string
	var bestSize int
	for _, img := range images {
		if img.Width*img.Height > bestSize {
			bestSize = img.Width * img.Height
			best = img.URL
		}
	}
	return best
}

func isAlphaNum(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
