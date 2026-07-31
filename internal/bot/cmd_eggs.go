package bot

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/maddeth/boozie-bot/internal/twitch"
)

var twitchUsernameRE = regexp.MustCompile(`^[a-zA-Z0-9_]{4,25}$`)

// cmdAddEggs handles !add<points> <username> <amount> (moderator only).
func (b *Bot) cmdAddEggs(ctx context.Context, msg *twitch.ChatMessage) {
	if !b.isMod(ctx, msg) {
		b.sayf("Get fucked %s, you're not a mod cmonBruh", msg.User.DisplayName)
		return
	}

	args := strings.Fields(msg.Text[len(b.cmdAddPoints):])
	if len(args) != 2 {
		b.sayf("Incorrect arguments, please use %s username amount", b.cmdAddPoints)
		return
	}

	username := stripInvisibleChars(args[0])
	if !twitchUsernameRE.MatchString(username) {
		b.sayf("Invalid username format: %s", username)
		return
	}

	amount, err := strconv.Atoi(args[1])
	if err != nil {
		b.sayf("Invalid number of %s: %s", b.cfg.PointsName, args[1])
		return
	}

	targetID := b.resolveTwitchUserID(ctx, username)
	if targetID == "" {
		b.sayf("%s - Could not find Twitch user %s", msg.User.DisplayName, username)
		return
	}

	result, err := b.eggs.EggUpdateCommand(ctx, username, amount, targetID, b.cfg.PointsName, b.cfg.PointsNameSingular)
	if err != nil {
		slog.Error("addeggs failed", "error", err, "user", username)
		b.sayf("%s - Failed to update %s", msg.User.DisplayName, b.cfg.PointsName)
		return
	}
	b.say(result)
}

// cmdEggs handles !<points> [username] - check your own or another user's points.
func (b *Bot) cmdEggs(ctx context.Context, msg *twitch.ChatMessage) {
	args := stripInvisibleChars(strings.TrimSpace(msg.Text[len(b.cmdPoints):]))

	if args != "" {
		// Check someone else's points
		targetUsername := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(args)), "@")

		targetID := b.resolveTwitchUserID(ctx, targetUsername)
		identifier := targetID
		if identifier == "" {
			identifier = targetUsername
		}

		eggs, err := b.eggs.GetUserEggs(ctx, identifier)
		if err != nil {
			slog.Error("failed to get eggs for user", "target", targetUsername, "error", err)
			b.sayf("%s - Could not find user %s", msg.User.DisplayName, targetUsername)
			return
		}
		if eggs == nil {
			b.sayf("%s - %s has no %s yet!", msg.User.DisplayName, targetUsername, b.cfg.PointsName)
			return
		}
		b.sayf("%s - %s has %s %s %s", msg.User.DisplayName, eggs.Username, formatNumber(eggs.EggsAmount), b.cfg.PointsName, b.cfg.PointsEmoji)
		return
	}

	// Check own points
	identifier := msg.User.ID
	if identifier == "" {
		identifier = msg.User.Name
	}

	eggs, err := b.eggs.GetUserEggs(ctx, identifier)
	if err != nil {
		slog.Error("failed to get eggs", "user", msg.User.Name, "error", err)
		b.sayf("%s - Could not check your %s", msg.User.DisplayName, b.cfg.PointsName)
		return
	}
	if eggs == nil {
		b.sayf("%s - You have no %s yet! Keep chatting to earn some %s", msg.User.DisplayName, b.cfg.PointsName, b.cfg.PointsEmoji)
		return
	}
	b.sayf("%s - You have %s %s %s", msg.User.DisplayName, formatNumber(eggs.EggsAmount), b.cfg.PointsName, b.cfg.PointsEmoji)
}

// cmdTopEggs handles !top<points> - show the points leaderboard.
func (b *Bot) cmdTopEggs(ctx context.Context, msg *twitch.ChatMessage) {
	leaderboard, err := b.eggs.GetLeaderboard(ctx, 5)
	if err != nil {
		slog.Error("failed to get leaderboard", "error", err)
		b.sayf("%s - Could not load %s leaderboard", msg.User.DisplayName, b.cfg.PointsName)
		return
	}
	if len(leaderboard) == 0 {
		b.sayf("%s - No %s data available yet!", msg.User.DisplayName, b.cfg.PointsName)
		return
	}

	parts := make([]string, len(leaderboard))
	for i, e := range leaderboard {
		parts[i] = fmt.Sprintf("%d. %s (%s)", i+1, e.Username, formatNumber(e.EggsAmount))
	}
	b.sayf("%s - Top %s Collectors: %s %s", msg.User.DisplayName, strings.Title(b.cfg.PointsNameSingular), strings.Join(parts, ", "), b.cfg.PointsEmoji)
}

// stripInvisibleChars removes zero-width/invisible characters from text.
func stripInvisibleChars(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == 0x034F,
			r >= 0x200B && r <= 0x200D,
			r == 0xFEFF,
			r == 0x00A0,
			r == 0x180E,
			r >= 0x2000 && r <= 0x200F,
			r >= 0x2028 && r <= 0x202F,
			r >= 0x205F && r <= 0x206F,
			r == 0x3000,
			r == 0xF3A0:
			return -1
		}
		return r
	}, s)
}

// formatNumber formats an integer with commas (e.g. 1234 -> "1,234").
func formatNumber(n int) string {
	if n < 0 {
		return "-" + formatNumber(-n)
	}
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}
