package bot

import (
	"context"
	"log/slog"
	"strings"

	"github.com/maddeth/boozie-bot/internal/twitch"
)

// cmdCustom handles dynamic database-driven commands (fallthrough handler).
// Called when no built-in command matches.
func (b *Bot) cmdCustom(ctx context.Context, msg *twitch.ChatMessage) {
	match := b.commands.FindMatchingCommand(ctx, strings.ToLower(msg.Text))
	if match == nil {
		return
	}

	perms := b.getPermissions(msg)
	response, audioURL, executed := b.commands.ExecuteCommand(ctx, match.Trigger, perms)
	if !executed {
		return
	}

	if response != "" {
		b.say(response)
	}

	// Send audio to WebSocket clients if present
	if audioURL != "" {
		b.ws.Broadcast(map[string]any{
			"type":     "redeem",
			"audioUrl": audioURL,
		})
	}

	slog.Debug("custom command executed", "trigger", match.Trigger, "user", msg.User.DisplayName)
}
