package services

import (
	"encoding/json"
	"log/slog"
	"os"
	"sync"
)

const spotifySettingsPath = "./spotify_runtime.json"

// SpotifyRuntimeSettings holds toggles that the broadcaster can flip from the
// moderator UI without restarting the bot. Persisted to disk so state survives
// rebuilds.
type SpotifyRuntimeSettings struct {
	mu                  sync.RWMutex
	songRequestsEnabled bool
	overlayEnabled      bool
}

// settingsFile is the on-disk representation. Defaults to both enabled if the
// file doesn't exist yet (i.e. fresh install).
type settingsFile struct {
	SongRequestsEnabled bool `json:"songRequestsEnabled"`
	OverlayEnabled      bool `json:"overlayEnabled"`
}

// NewSpotifyRuntimeSettings loads settings from disk; on first run it writes
// defaults (both toggles on).
func NewSpotifyRuntimeSettings() *SpotifyRuntimeSettings {
	s := &SpotifyRuntimeSettings{
		songRequestsEnabled: true,
		overlayEnabled:      true,
	}

	data, err := os.ReadFile(spotifySettingsPath)
	if os.IsNotExist(err) {
		// Persist defaults so the file exists for next time.
		_ = s.persist()
		return s
	}
	if err != nil {
		slog.Warn("failed to read spotify runtime settings, using defaults", "error", err)
		return s
	}

	var f settingsFile
	if err := json.Unmarshal(data, &f); err != nil {
		slog.Warn("failed to parse spotify runtime settings, using defaults", "error", err)
		return s
	}
	s.songRequestsEnabled = f.SongRequestsEnabled
	s.overlayEnabled = f.OverlayEnabled
	slog.Info("spotify runtime settings loaded",
		"songRequests", s.songRequestsEnabled, "overlay", s.overlayEnabled)
	return s
}

// SongRequestsEnabled reports whether !sr is currently accepting requests.
func (s *SpotifyRuntimeSettings) SongRequestsEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.songRequestsEnabled
}

// OverlayEnabled reports whether the now-playing overlay is being kept in sync.
func (s *SpotifyRuntimeSettings) OverlayEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.overlayEnabled
}

// SetSongRequestsEnabled updates the toggle and persists to disk.
func (s *SpotifyRuntimeSettings) SetSongRequestsEnabled(v bool) error {
	s.mu.Lock()
	s.songRequestsEnabled = v
	s.mu.Unlock()
	return s.persist()
}

// SetOverlayEnabled updates the toggle and persists to disk.
func (s *SpotifyRuntimeSettings) SetOverlayEnabled(v bool) error {
	s.mu.Lock()
	s.overlayEnabled = v
	s.mu.Unlock()
	return s.persist()
}

// Snapshot returns both flags as a JSON-serialisable struct.
func (s *SpotifyRuntimeSettings) Snapshot() settingsFile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return settingsFile{
		SongRequestsEnabled: s.songRequestsEnabled,
		OverlayEnabled:      s.overlayEnabled,
	}
}

func (s *SpotifyRuntimeSettings) persist() error {
	snap := s.Snapshot()
	data, err := json.MarshalIndent(snap, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(spotifySettingsPath, data, 0644)
}
