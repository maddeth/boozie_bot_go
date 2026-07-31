package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/maddeth/boozie-bot/internal/twitch"
)

// cmdPool handles !pool <poolname> - check a pool's status.
func (b *Bot) cmdPool(ctx context.Context, msg *twitch.ChatMessage) {
	args := strings.Fields(strings.TrimSpace(msg.Text[len("!pool"):]))
	if len(args) == 0 {
		b.sayf("%s - Usage: !pool <poolname>", msg.User.DisplayName)
		return
	}

	pool, err := b.pools.GetPool(ctx, args[0])
	if err != nil {
		slog.Error("failed to get pool", "pool", args[0], "error", err)
		b.sayf("%s - Could not check pool", msg.User.DisplayName)
		return
	}
	if pool == nil {
		b.sayf("%s - Pool \"%s\" not found", msg.User.DisplayName, args[0])
		return
	}
	b.sayf("%s - Pool \"%s\" has %s %s from %d donors %s",
		msg.User.DisplayName, pool.PoolName, formatNumber(pool.EggsAmount), b.cfg.PointsName, pool.UniqueDonors, b.cfg.PointsEmoji)
}

// cmdDonate handles !donate <poolname> <amount>.
func (b *Bot) cmdDonate(ctx context.Context, msg *twitch.ChatMessage) {
	args := strings.Fields(strings.TrimSpace(msg.Text[len("!donate"):]))
	if len(args) < 2 {
		b.sayf("%s - Usage: !donate <poolname> <amount>", msg.User.DisplayName)
		return
	}

	poolName := args[0]
	amount, err := strconv.Atoi(args[1])
	if err != nil || amount < 1 {
		b.sayf("%s - Usage: !donate <poolname> <amount>", msg.User.DisplayName)
		return
	}

	twitchUserID := msg.User.ID
	if twitchUserID == "" {
		twitchUserID = b.resolveTwitchUserID(ctx, msg.User.Name)
	}
	if twitchUserID == "" {
		b.sayf("%s - Could not verify your Twitch account. Please try again.", msg.User.DisplayName)
		return
	}

	pool, err := b.pools.DonateToPool(ctx, poolName, twitchUserID, msg.User.DisplayName, amount)
	if err != nil {
		errMsg := err.Error()
		switch {
		case strings.Contains(errMsg, "not found"):
			b.sayf("%s - Pool \"%s\" not found", msg.User.DisplayName, poolName)
		case strings.Contains(errMsg, "insufficient") || strings.Contains(errMsg, "Insufficient"):
			b.sayf("%s - You don't have enough %s to donate %d", msg.User.DisplayName, b.cfg.PointsName, amount)
		case strings.Contains(errMsg, "not active"):
			b.sayf("%s - Pool \"%s\" is not active", msg.User.DisplayName, poolName)
		default:
			slog.Error("donate failed", "error", err, "pool", poolName)
			b.sayf("%s - Could not process donation", msg.User.DisplayName)
		}
		return
	}

	b.sayf("%s donated %d %s to pool \"%s\"! Pool total: %s %s",
		msg.User.DisplayName, amount, b.cfg.PointsName, pool.PoolName, formatNumber(pool.EggsAmount), b.cfg.PointsEmoji)
}

// cmdListPools handles !pools - list active pools.
func (b *Bot) cmdListPools(ctx context.Context, msg *twitch.ChatMessage) {
	pools, err := b.pools.GetAllPools(ctx)
	if err != nil {
		slog.Error("failed to list pools", "error", err)
		b.sayf("%s - Could not list pools", msg.User.DisplayName)
		return
	}
	if len(pools) == 0 {
		b.sayf("%s - No active pools available", msg.User.DisplayName)
		return
	}

	limit := 3
	if len(pools) < limit {
		limit = len(pools)
	}
	parts := make([]string, limit)
	for i := 0; i < limit; i++ {
		parts[i] = fmt.Sprintf("%s (%s)", pools[i].PoolName, formatNumber(pools[i].EggsAmount))
	}
	extra := ""
	if len(pools) > 3 {
		extra = fmt.Sprintf(" and %d more", len(pools)-3)
	}
	b.sayf("%s - Active pools: %s%s", msg.User.DisplayName, strings.Join(parts, ", "), extra)
}

// cmdCreatePool handles !createpool <name> [description] (moderator only).
func (b *Bot) cmdCreatePool(ctx context.Context, msg *twitch.ChatMessage) {
	perms := b.getPermissions(msg)
	if !perms.IsModerator {
		b.sayf("%s - Only moderators can create pools", msg.User.DisplayName)
		return
	}

	twitchUserID := msg.User.ID
	if twitchUserID == "" {
		twitchUserID = b.resolveTwitchUserID(ctx, msg.User.Name)
	}
	if twitchUserID == "" {
		b.sayf("%s - Could not verify your Twitch account. Please try again.", msg.User.DisplayName)
		return
	}

	rest := strings.TrimSpace(msg.Text[len("!createpool"):])
	parts := strings.SplitN(rest, " ", 2)
	poolName := parts[0]
	description := ""
	if len(parts) > 1 {
		description = parts[1]
	}

	if poolName == "" {
		b.sayf("%s - Usage: !createpool <poolname> [description]", msg.User.DisplayName)
		return
	}

	pool, err := b.pools.CreatePool(ctx, poolName, description, twitchUserID, msg.User.DisplayName)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			b.sayf("%s - Pool name already exists", msg.User.DisplayName)
		} else {
			slog.Error("create pool failed", "error", err)
			b.sayf("%s - Could not create pool", msg.User.DisplayName)
		}
		return
	}

	b.sayf("%s created pool \"%s\"! Start donating with !donate %s <amount>",
		msg.User.DisplayName, pool.PoolName, pool.PoolName)
}

// cmdDeletePool handles !deletepool <poolname> (moderator only).
func (b *Bot) cmdDeletePool(ctx context.Context, msg *twitch.ChatMessage) {
	perms := b.getPermissions(msg)
	if !perms.IsModerator {
		b.sayf("%s - Only moderators can delete pools", msg.User.DisplayName)
		return
	}

	fields := strings.Fields(strings.TrimSpace(msg.Text[len("!deletepool"):]))
	if len(fields) == 0 {
		b.sayf("%s - Usage: !deletepool <poolname>", msg.User.DisplayName)
		return
	}
	poolName := fields[0]

	// Get pool info before deleting for the response message
	pool, err := b.pools.GetPool(ctx, poolName)
	if err != nil || pool == nil {
		b.sayf("%s - Pool \"%s\" not found", msg.User.DisplayName, poolName)
		return
	}
	if !pool.IsActive {
		b.sayf("%s - Pool \"%s\" is already deleted", msg.User.DisplayName, poolName)
		return
	}

	if err := b.pools.DeletePool(ctx, poolName); err != nil {
		slog.Error("delete pool failed", "error", err, "pool", poolName)
		b.sayf("%s - Could not delete pool", msg.User.DisplayName)
		return
	}

	b.sayf("%s deleted pool \"%s\" (had %s %s)", msg.User.DisplayName, pool.PoolName, formatNumber(pool.EggsAmount), b.cfg.PointsName)
	slog.Info("pool deleted via chat", "pool", pool.PoolNameSanitised, "by", msg.User.DisplayName)
}
