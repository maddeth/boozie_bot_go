package bot

import (
	"context"
	"log/slog"
	"strings"

	"github.com/maddeth/boozie-bot/internal/twitch"
)

// cmdShoutout handles !so <username> and !shoutout <username> (moderator only).
func (b *Bot) cmdShoutout(ctx context.Context, msg *twitch.ChatMessage) {
	if !b.isMod(ctx, msg) {
		b.sayf("%s - Only moderators can use the shoutout command", msg.User.DisplayName)
		return
	}

	parts := strings.Fields(msg.Text)
	if len(parts) < 2 {
		b.sayf("%s - Usage: !so @username or !shoutout @username", msg.User.DisplayName)
		return
	}

	targetUsername := strings.TrimPrefix(strings.ToLower(parts[1]), "@")

	user, err := b.helix.GetUserByName(ctx, targetUsername)
	if err != nil {
		slog.Error("shoutout user lookup failed", "target", targetUsername, "error", err)
		b.sayf("%s - Failed to send shoutout", msg.User.DisplayName)
		return
	}
	if user == nil {
		b.sayf("%s - User \"%s\" not found", msg.User.DisplayName, targetUsername)
		return
	}

	if user.ID == msg.User.ID {
		b.sayf("%s - You can't shoutout yourself! 😅", msg.User.DisplayName)
		return
	}

	if err := b.helix.SendShoutout(ctx, user.ID); err != nil {
		slog.Error("shoutout API call failed", "target", targetUsername, "error", err)
	} else {
		slog.Info("shoutout sent", "by", msg.User.DisplayName, "target", targetUsername)
	}
	b.shoutouts.MarkShoutedOut(user.ID)

	desc := user.BroadcasterType
	if desc == "" {
		desc = "something awesome"
	}
	b.sayf("Check out %s! They were last streaming %s! Follow them at twitch.tv/%s",
		user.DisplayName, desc, user.Login)
}
