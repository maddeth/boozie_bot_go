package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// Config mirrors the structure of the existing config.json used by the JS bot.
type Config struct {
	Channels          []string    `json:"channels"`
	Username          string      `json:"username"`
	ClientID          string      `json:"clientId"`
	ClientSecret      string      `json:"clientSecret"`
	RedirectURI       string      `json:"redirectUri"`
	Scopes            []string    `json:"scopes"`
	Port              string      `json:"-"` // parsed from portRaw
	WebSocketPort     string      `json:"-"` // parsed from wsPortRaw
	MyChannel         string      `json:"myChannel"`
	MyChannelUserID   string      `json:"myChannelUserId"`
	BoozieBotUserID   string      `json:"boozieBotUserID"`
	EggUpdateInterval int         `json:"eggUpdateInterval"` // milliseconds
	LogLevel          string      `json:"logLevel"`
	CommandPrefix     string      `json:"commandPrefix"`
	DefaultCooldown   int         `json:"defaultCooldown"` // milliseconds
	OBSIP             string      `json:"obsIP"`
	OBSPassword       string      `json:"obsPassword"`
	TTSDirectory      string      `json:"ttsDirectory"`
	Secret             string      `json:"secret"`             // EventSub webhook HMAC secret
	WebAddress         string      `json:"webAddress"`         // Public URL for webhook callbacks
	PointsName         string      `json:"pointsName"`         // Plural name (default: "points", e.g. "eggs", "coins")
	PointsNameSingular string      `json:"pointsNameSingular"` // Singular name (default: "point", e.g. "egg", "coin")
	PointsEmoji        string      `json:"pointsEmoji"`        // Emoji (default: "🪙", e.g. "🥚", "💰")
	OpenAI             *OpenAIConf `json:"openai,omitempty"`
	Spotify            *SpotifyConf `json:"spotify,omitempty"`
}

type OpenAIConf struct {
	APIKey string `json:"apiKey"`
}

// SpotifyConf holds Spotify Web API integration settings.
type SpotifyConf struct {
	Enabled                    bool   `json:"enabled"`
	ClientID                   string `json:"clientId"`
	ClientSecret               string `json:"clientSecret"`
	RedirectURI                string `json:"redirectUri"`
	SongRequestCost            int    `json:"songRequestCost"`            // eggs charged per !sr (default: 100)
	SongRequestCooldownSeconds int    `json:"songRequestCooldownSeconds"` // per-user cooldown (default: 60)
	MaxTrackDurationSeconds    int    `json:"maxTrackDurationSeconds"`    // reject longer tracks (default: 600 = 10 min, 0 = no limit)
	DuplicateCooldownSeconds   int    `json:"duplicateCooldownSeconds"`   // reject same track if queued in last N seconds (default: 3600 = 60 min, 0 = no limit)
}

// rawConfig is the intermediate struct for JSON unmarshalling.
// Handles port/webSocketPort being either string or number in config.json.
type rawConfig struct {
	Config
	PortRaw   json.RawMessage `json:"port"`
	WSPortRaw json.RawMessage `json:"webSocketPort"`
}

// parsePort extracts a string from a JSON value that may be a string or number.
func parsePort(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try number
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return strconv.Itoa(n)
	}
	return ""
}

// Load reads and parses a config JSON file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	cfg := raw.Config
	cfg.Port = parsePort(raw.PortRaw)
	cfg.WebSocketPort = parsePort(raw.WSPortRaw)

	// Defaults
	if cfg.Port == "" {
		cfg.Port = "3000"
	}
	if cfg.WebSocketPort == "" {
		cfg.WebSocketPort = "3001"
	}
	if cfg.CommandPrefix == "" {
		cfg.CommandPrefix = "!"
	}
	if cfg.TTSDirectory == "" {
		cfg.TTSDirectory = "/home/html/tts"
	}
	if cfg.PointsName == "" {
		cfg.PointsName = "points"
	}
	if cfg.PointsNameSingular == "" {
		cfg.PointsNameSingular = "point"
	}
	if cfg.PointsEmoji == "" {
		cfg.PointsEmoji = "🪙"
	}
	if cfg.Spotify != nil && cfg.Spotify.Enabled {
		if cfg.Spotify.SongRequestCost == 0 {
			cfg.Spotify.SongRequestCost = 100
		}
		if cfg.Spotify.SongRequestCooldownSeconds == 0 {
			cfg.Spotify.SongRequestCooldownSeconds = 60
		}
		if cfg.Spotify.MaxTrackDurationSeconds == 0 {
			cfg.Spotify.MaxTrackDurationSeconds = 600
		}
		if cfg.Spotify.DuplicateCooldownSeconds == 0 {
			cfg.Spotify.DuplicateCooldownSeconds = 3600
		}
	}

	return &cfg, nil
}
