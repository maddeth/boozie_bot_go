package bot

import (
	"context"
	"log/slog"
	"strings"

	"github.com/maddeth/boozie-bot/internal/twitch"
)

// cmdMergeEggs handles !mergeeggs <fromUser> <toUser> [reason] (moderator only).
func (b *Bot) cmdMergeEggs(ctx context.Context, msg *twitch.ChatMessage) {
	perms := b.getPermissions(msg)
	if !perms.IsModerator {
		b.sayf("%s - Only moderators can merge user eggs", msg.User.DisplayName)
		return
	}

	args := strings.Fields(strings.TrimSpace(msg.Text[len("!mergeeggs"):]))
	if len(args) < 2 {
		b.sayf("%s - Usage: !mergeeggs <fromUser> <toUser> [reason]", msg.User.DisplayName)
		return
	}

	fromUser := args[0]
	toUser := args[1]
	reason := "Moderator merge via chat"
	if len(args) > 2 {
		reason = strings.Join(args[2:], " ")
	}

	if strings.EqualFold(fromUser, toUser) {
		b.sayf("%s - Cannot merge user with themselves", msg.User.DisplayName)
		return
	}

	// Preview first
	preview, err := b.merge.PreviewMerge(ctx, fromUser, toUser)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			b.sayf("%s - User not found: %s", msg.User.DisplayName, err.Error())
		} else {
			slog.Error("merge preview failed", "error", err)
			b.sayf("%s - Could not merge eggs: %s", msg.User.DisplayName, err.Error())
		}
		return
	}

	if preview.Source == nil || preview.Source.EggsAmount == 0 {
		b.sayf("%s - %s has no eggs to transfer", msg.User.DisplayName, fromUser)
		return
	}

	// Execute merge
	adminTwitchID := msg.User.ID
	if adminTwitchID == "" {
		adminTwitchID = msg.User.Name
	}

	result, err := b.merge.MergeUserEggs(ctx, fromUser, toUser, adminTwitchID, msg.User.DisplayName, &reason, false)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			b.sayf("%s - User not found: %s", msg.User.DisplayName, err.Error())
		} else {
			slog.Error("merge failed", "error", err, "from", fromUser, "to", toUser)
			b.sayf("%s - Could not merge eggs: %s", msg.User.DisplayName, err.Error())
		}
		return
	}

	// Calculate new total
	newTotal := result.EggsTransferred
	if preview.Target != nil {
		newTotal += preview.Target.EggsAmount
	}

	b.sayf("%s successfully merged %s eggs from %s to %s. New total: %s eggs",
		msg.User.DisplayName,
		formatNumber(result.EggsTransferred),
		result.SourceUsername,
		result.TargetUsername,
		formatNumber(newTotal))
}
