package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/maddeth/boozie-bot/internal/services"
	"github.com/maddeth/boozie-bot/internal/spotify"
	"github.com/maddeth/boozie-bot/internal/twitch"
)

// cooldownMap tracks per-key timestamps for cooldowns. Used for both the
// per-user !sr rate limit and the per-track duplicate-prevention window.
type cooldownMap struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newCooldownMap() *cooldownMap {
	return &cooldownMap{last: make(map[string]time.Time)}
}

// check atomically tests the cooldown and, if not blocked, records the current
// time. Used for the per-user cooldown where we want to "consume" the action
// even if it later fails (so users can't retry instantly).
func (c *cooldownMap) check(key string, dur time.Duration) (remaining time.Duration, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if last, exists := c.last[key]; exists && now.Sub(last) < dur {
		return dur - now.Sub(last), false
	}
	c.last[key] = now
	return 0, true
}

// peek tests the cooldown without updating. Used for the per-track dupe check
// so that a queue-add failure doesn't lock the track out for the whole window.
func (c *cooldownMap) peek(key string, dur time.Duration) (remaining time.Duration, blocked bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if last, exists := c.last[key]; exists {
		if elapsed := time.Since(last); elapsed < dur {
			return dur - elapsed, true
		}
	}
	return 0, false
}

// mark records the current time for key.
func (c *cooldownMap) mark(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last[key] = time.Now()
}

// cmdSongRequest handles !sr <query> - adds a track to the broadcaster's Spotify queue.
func (b *Bot) cmdSongRequest(ctx context.Context, msg *twitch.ChatMessage) {
	if b.spotifySvc == nil || b.cfg.Spotify == nil || !b.cfg.Spotify.Enabled {
		return // silently ignore if Spotify isn't configured
	}

	if !b.spotifySvc.Settings().SongRequestsEnabled() {
		b.sayf("%s - song requests are currently disabled", msg.User.DisplayName)
		return
	}

	args := strings.TrimSpace(msg.Text[len("!sr"):])
	if args == "" {
		b.sayf("%s - Usage: !sr <Spotify URL or search query>", msg.User.DisplayName)
		return
	}

	cost := b.cfg.Spotify.SongRequestCost
	cooldown := time.Duration(b.cfg.Spotify.SongRequestCooldownSeconds) * time.Second
	maxDur := time.Duration(b.cfg.Spotify.MaxTrackDurationSeconds) * time.Second
	dupeCD := time.Duration(b.cfg.Spotify.DuplicateCooldownSeconds) * time.Second

	// Per-user cooldown (skipped for mods/broadcaster - they tend to test).
	if !b.isMod(ctx, msg) {
		if remaining, ok := b.srUserCooldown.check(msg.User.ID, cooldown); !ok {
			b.sayf("%s - !sr is on cooldown, try again in %ds", msg.User.DisplayName, int(remaining.Seconds())+1)
			return
		}
	}

	if msg.User.ID == "" {
		b.sayf("%s - couldn't identify your account for !sr", msg.User.DisplayName)
		return
	}

	// Resolve the track FIRST so we never charge eggs for an invalid query.
	track, err := b.spotifySvc.Client().ResolveTrack(ctx, args)
	if err != nil {
		if errors.Is(err, spotify.ErrTrackNotFound) {
			b.sayf("%s - couldn't find a Spotify track matching %q", msg.User.DisplayName, args)
			return
		}
		slog.Error("sr: resolve track failed", "error", err, "query", args)
		b.sayf("%s - Spotify search failed, try again later", msg.User.DisplayName)
		return
	}

	// Duration cap - applies to everyone (mods included).
	if maxDur > 0 && time.Duration(track.DurationMS)*time.Millisecond > maxDur {
		b.sayf("%s - %q is too long (%s) - max for !sr is %s",
			msg.User.DisplayName, track.Name,
			formatDuration(time.Duration(track.DurationMS)*time.Millisecond),
			formatDuration(maxDur))
		return
	}

	// Duplicate-track cooldown - applies to everyone. Peek only; we'll mark
	// after a successful queue add so a failed add doesn't lock the track out.
	if dupeCD > 0 {
		if remaining, blocked := b.srTrackCooldown.peek(track.ID, dupeCD); blocked {
			b.sayf("%s - %q was just queued, try again in %s",
				msg.User.DisplayName, track.Name, formatDuration(remaining))
			return
		}
	}

	// Charge eggs.
	if cost > 0 {
		if _, err := b.eggs.UpdateUserEggs(ctx, msg.User.ID, msg.User.Name, -cost); err != nil {
			if errors.Is(err, services.ErrInsufficientEggs) {
				b.sayf("%s - !sr costs %d %s %s - you don't have enough", msg.User.DisplayName, cost, b.cfg.PointsName, b.cfg.PointsEmoji)
				return
			}
			slog.Error("sr: charge eggs failed", "error", err, "user", msg.User.Name)
			b.sayf("%s - Failed to charge %s, try again", msg.User.DisplayName, b.cfg.PointsName)
			return
		}
	}

	// Queue it on Spotify.
	if err := b.spotifySvc.Client().AddToQueue(ctx, track.URI); err != nil {
		// Refund eggs since the queue add failed.
		if cost > 0 {
			if _, refundErr := b.eggs.UpdateUserEggs(ctx, msg.User.ID, msg.User.Name, cost); refundErr != nil {
				slog.Error("sr: refund failed after queue error", "error", refundErr, "user", msg.User.Name)
			}
		}
		if errors.Is(err, spotify.ErrNoActiveDevice) {
			b.sayf("%s - Spotify isn't playing on any device right now (refunded)", msg.User.DisplayName)
			return
		}
		slog.Error("sr: queue add failed", "error", err)
		b.sayf("%s - Failed to queue track (refunded), try again later", msg.User.DisplayName)
		return
	}

	// Mark the track so it can't be re-requested for the dupe window.
	if dupeCD > 0 {
		b.srTrackCooldown.mark(track.ID)
	}

	b.sayf("🎵 Queued for %s: %s - %s", msg.User.DisplayName, track.Name, strings.Join(track.Artists, ", "))
}

// cmdSong handles !song - prints the currently playing track to chat.
func (b *Bot) cmdSong(ctx context.Context, msg *twitch.ChatMessage) {
	if b.spotifySvc == nil || b.cfg.Spotify == nil || !b.cfg.Spotify.Enabled {
		return
	}

	np := b.spotifySvc.Latest()
	if np == nil || np.Track == nil || !np.IsPlaying {
		b.sayf("No song is currently playing")
		return
	}

	artists := strings.Join(np.Track.Artists, ", ")
	b.sayf("🎵 Now playing: %s - %s", np.Track.Name, artists)
}

// cmdSongQueue handles !songqueue - prints the current queue to chat.
func (b *Bot) cmdSongQueue(ctx context.Context, msg *twitch.ChatMessage) {
	if b.spotifySvc == nil || b.cfg.Spotify == nil || !b.cfg.Spotify.Enabled {
		return
	}

	currentlyPlaying, queue, err := b.spotifySvc.Client().GetQueue(ctx)
	if err != nil {
		if errors.Is(err, spotify.ErrNothingPlaying) {
			b.sayf("Spotify queue is empty")
			return
		}
		slog.Error("songqueue: failed to get queue", "error", err)
		b.sayf("Failed to fetch Spotify queue")
		return
	}

	if currentlyPlaying == nil && len(queue) == 0 {
		b.sayf("Spotify queue is empty")
		return
	}

	// Build the queue message. Spotify chat messages have a 500 char limit
	// so we cap the number of tracks shown.
	const maxTracks = 5

	var sb strings.Builder
	sb.WriteString("🎵 Queue: ")
	if currentlyPlaying != nil {
		sb.WriteString("NOW: ")
		sb.WriteString(currentlyPlaying.Name)
		sb.WriteString(" - ")
		sb.WriteString(strings.Join(currentlyPlaying.Artists, ", "))
	}

	shown := 0
	for _, t := range queue {
		if shown >= maxTracks {
			remaining := len(queue) - shown
			sb.WriteString(fmt.Sprintf(" | +%d more", remaining))
			break
		}
		if currentlyPlaying != nil || shown > 0 {
			sb.WriteString(" | ")
		}
		sb.WriteString(fmt.Sprintf("NEXT%d: %s - %s", shown+1, t.Name, strings.Join(t.Artists, ", ")))
		shown++
	}

	if currentlyPlaying == nil && len(queue) == 0 {
		b.sayf("Spotify queue is empty")
		return
	}

	b.sayf(sb.String())
}

// formatDuration renders a duration as "Mm" or "Mm Ss" (e.g. "10m", "3m 24s").
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	minutes := int(d / time.Minute)
	seconds := int((d % time.Minute) / time.Second)
	if minutes == 0 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds == 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%dm %ds", minutes, seconds)
}
