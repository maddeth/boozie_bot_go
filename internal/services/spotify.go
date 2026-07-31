package services

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/maddeth/boozie-bot/internal/spotify"
)

// SpotifyBroadcaster is the minimal interface needed by the polling loop.
// Satisfied by *websocket.Server.
type SpotifyBroadcaster interface {
	BroadcastImmediate(msg any)
}

// SpotifyService polls the broadcaster's now-playing state and broadcasts
// changes over WebSocket. The latest state is cached so the HTTP handler can
// serve it without an extra Spotify round-trip.
type SpotifyService struct {
	client   *spotify.Client
	ws       SpotifyBroadcaster
	settings *SpotifyRuntimeSettings

	mu           sync.RWMutex
	latest       *spotify.NowPlaying
	lastErr      error
	overlayWasOn bool // tracks previous overlay state so we can clear on transition

	wakeCh chan struct{} // non-blocking signal that the polling loop should run immediately
}

// NewSpotifyService creates a service. The polling loop is started by Run.
func NewSpotifyService(client *spotify.Client, ws SpotifyBroadcaster, settings *SpotifyRuntimeSettings) *SpotifyService {
	return &SpotifyService{
		client:       client,
		ws:           ws,
		settings:     settings,
		overlayWasOn: settings.OverlayEnabled(),
		wakeCh:       make(chan struct{}, 1),
	}
}

// Wake signals the polling loop to run a cycle immediately, bypassing the
// current sleep. Used by the settings handler after a toggle change so the
// overlay reflects the new state without waiting for the next tick.
func (s *SpotifyService) Wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default: // already pending
	}
}

// Settings exposes the runtime settings so handlers can read/write toggles.
func (s *SpotifyService) Settings() *SpotifyRuntimeSettings {
	return s.settings
}

// Latest returns the most recently observed now-playing state (may be nil).
func (s *SpotifyService) Latest() *spotify.NowPlaying {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest
}

// Client exposes the underlying Spotify client for chat command use (!sr).
func (s *SpotifyService) Client() *spotify.Client {
	return s.client
}

// Run starts the polling loop and blocks until ctx is cancelled.
// Cadence: 5s while playing, 30s while idle/paused — to stay well under
// Spotify's rate limit while still feeling responsive when music is on.
func (s *SpotifyService) Run(ctx context.Context) {
	const (
		playingInterval = 5 * time.Second
		idleInterval    = 30 * time.Second
	)

	slog.Info("spotify polling started")
	defer slog.Info("spotify polling stopped")

	timer := time.NewTimer(playingInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.wakeCh:
			// Stop the timer and drain so the next Reset is safe.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}

		// Respect the overlay toggle. When transitioning from on→off, push a
		// final "cleared" state so the overlay drops the last track.
		if !s.settings.OverlayEnabled() {
			s.mu.Lock()
			wasOn := s.overlayWasOn
			s.overlayWasOn = false
			s.mu.Unlock()
			if wasOn {
				s.clearOverlay()
			}
			timer.Reset(idleInterval)
			continue
		}
		s.mu.Lock()
		s.overlayWasOn = true
		s.mu.Unlock()

		next := idleInterval
		if s.poll(ctx) {
			next = playingInterval
		}
		timer.Reset(next)
	}
}

// clearOverlay pushes a null now-playing message so the overlay shows nothing.
func (s *SpotifyService) clearOverlay() {
	s.mu.Lock()
	s.latest = nil
	s.mu.Unlock()
	if s.ws != nil {
		s.ws.BroadcastImmediate(map[string]any{
			"type":       "spotify_now_playing",
			"nowPlaying": nil,
		})
	}
}

// poll fetches the current track, broadcasts on change, and returns true if
// something is playing (so the caller picks the faster cadence).
func (s *SpotifyService) poll(ctx context.Context) bool {
	np, err := s.client.GetCurrentlyPlaying(ctx)
	if err != nil {
		if errors.Is(err, spotify.ErrNothingPlaying) {
			s.handleChange(nil)
			return false
		}
		s.mu.Lock()
		first := s.lastErr == nil
		s.lastErr = err
		s.mu.Unlock()
		if first {
			slog.Warn("spotify polling error", "error", err)
		}
		return false
	}

	s.mu.Lock()
	s.lastErr = nil
	s.mu.Unlock()

	s.handleChange(np)
	return np != nil && np.IsPlaying
}

// handleChange caches the new state and broadcasts when the track ID or play state changes.
func (s *SpotifyService) handleChange(np *spotify.NowPlaying) {
	s.mu.Lock()
	prev := s.latest
	s.latest = np
	s.mu.Unlock()

	if !shouldBroadcast(prev, np) {
		return
	}

	if s.ws == nil {
		return
	}
	s.ws.BroadcastImmediate(map[string]any{
		"type":       "spotify_now_playing",
		"nowPlaying": np,
	})
}

// shouldBroadcast returns true when the visible track or playing-state changes.
// Progress ticks every poll don't count.
func shouldBroadcast(prev, cur *spotify.NowPlaying) bool {
	if prev == nil && cur == nil {
		return false
	}
	if prev == nil || cur == nil {
		return true
	}
	if prev.IsPlaying != cur.IsPlaying {
		return true
	}
	prevID := trackID(prev)
	curID := trackID(cur)
	return prevID != curID
}

func trackID(np *spotify.NowPlaying) string {
	if np == nil || np.Track == nil {
		return ""
	}
	return np.Track.ID
}
