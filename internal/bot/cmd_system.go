package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/maddeth/boozie-bot/internal/twitch"
)

// cmdListCommands handles !commands — list all available commands.
func (b *Bot) cmdListCommands(ctx context.Context, msg *twitch.ChatMessage) {
	commands := b.commands.GetAllCommands(ctx)

	// Separate public vs restricted
	var publicCmds, restrictedCmds []string
	for _, cmd := range commands {
		if cmd.Permission == "everyone" {
			publicCmds = append(publicCmds, cmd.Trigger)
		} else {
			restrictedCmds = append(restrictedCmds, fmt.Sprintf("%s (%s)", cmd.Trigger, cmd.Permission))
		}
	}
	sort.Strings(publicCmds)
	sort.Strings(restrictedCmds)

	message := fmt.Sprintf("%s - Available commands: ", msg.User.DisplayName)

	if len(publicCmds) > 0 {
		message += strings.Join(publicCmds, ", ")
	}
	if len(restrictedCmds) > 0 {
		if len(publicCmds) > 0 {
			message += " | "
		}
		message += strings.Join(restrictedCmds, ", ")
	}

	// Built-in commands
	message += fmt.Sprintf(" | Built-in: %s, %s, !quote, !pool, !donate, !pools, "+
		"!so (moderator), !shoutout (moderator), !createpool (moderator), "+
		"!deletepool (moderator), %s (moderator)",
		b.cmdPoints, b.cmdTopPoints, b.cmdMergePoints)

	// Truncate for Twitch's 500 char limit
	if len(message) > 450 {
		message = message[:447] + "..."
	}

	b.say(message)
}

// cmdReloadCommands handles !reloadcommands (moderator only).
func (b *Bot) cmdReloadCommands(ctx context.Context, msg *twitch.ChatMessage) {
	if !b.isMod(ctx, msg) {
		b.sayf("%s - Only moderators can reload commands", msg.User.DisplayName)
		return
	}

	if err := b.commands.ReloadCommands(ctx); err != nil {
		slog.Error("failed to reload commands", "error", err)
		b.sayf("%s - Failed to reload commands: %s", msg.User.DisplayName, err.Error())
		return
	}

	b.sayf("%s - Custom commands reloaded successfully!", msg.User.DisplayName)
}
