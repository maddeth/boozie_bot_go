package services

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Command represents a row in the custom_commands table.
type Command struct {
	ID          int    `json:"id"`
	Trigger     string `json:"trigger"`
	Response    string `json:"response"`
	Cooldown    int    `json:"cooldown"`    // seconds
	Permission  string `json:"permission"`  // everyone, subscriber, vip, moderator
	Enabled     bool   `json:"enabled"`
	UsageCount  int    `json:"usage_count"`
	TriggerType string `json:"trigger_type"` // exact, contains, regex
	AudioURL    string `json:"audio_url"`
	EggCost     int    `json:"egg_cost"`
}

// ChatUserInfo holds the info about a chat user needed for permission/command checks.
type ChatUserInfo struct {
	Username     string
	DisplayName  string
	TwitchUserID string
	IsModerator  bool
	IsSubscriber bool
	IsVIP        bool
	Channel      string
}

// CommandMatch is returned when a command matches a chat message.
type CommandMatch struct {
	Trigger string
	Command *Command
}

type containsEntry struct {
	pattern string
	command *Command
}

type regexEntry struct {
	regex   *regexp.Regexp
	command *Command
}

// CommandService manages custom chat commands with in-memory caching and cooldowns.
type CommandService struct {
	db         *pgxpool.Pool
	eggSvc     *EggService
	pointsName string // user-facing points name for cost messages

	mu              sync.RWMutex
	commands        map[string]*Command // all commands keyed by lowercase trigger
	exactCommands   map[string]*Command
	containsCommands []containsEntry
	regexCommands   []regexEntry
	lastLoad        time.Time
	cacheTimeout    time.Duration

	cooldownMu sync.RWMutex
	cooldowns  map[string]time.Time // "commandID_username" -> last used
}

// NewCommandService creates a new command service.
func NewCommandService(db *pgxpool.Pool, eggSvc *EggService, pointsName string) *CommandService {
	cs := &CommandService{
		db:            db,
		eggSvc:        eggSvc,
		pointsName:    pointsName,
		commands:      make(map[string]*Command),
		exactCommands: make(map[string]*Command),
		cooldowns:     make(map[string]time.Time),
		cacheTimeout:  60 * time.Second,
	}
	return cs
}

// LoadCommands fetches all enabled commands from the database and rebuilds the in-memory cache.
func (cs *CommandService) LoadCommands(ctx context.Context) error {
	rows, err := cs.db.Query(ctx,
		`SELECT id, trigger, COALESCE(response, ''), cooldown, permission, enabled, usage_count, trigger_type,
		 COALESCE(audio_url, ''), COALESCE(egg_cost, 0)
		 FROM custom_commands WHERE enabled = true ORDER BY trigger ASC`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()

	commands := make(map[string]*Command)
	exact := make(map[string]*Command)
	var contains []containsEntry
	var regexes []regexEntry

	for rows.Next() {
		var cmd Command
		if err := rows.Scan(&cmd.ID, &cmd.Trigger, &cmd.Response, &cmd.Cooldown, &cmd.Permission,
			&cmd.Enabled, &cmd.UsageCount, &cmd.TriggerType, &cmd.AudioURL, &cmd.EggCost); err != nil {
			return err
		}

		key := strings.ToLower(cmd.Trigger)
		cmdCopy := cmd // avoid closure issues
		commands[key] = &cmdCopy

		switch cmd.TriggerType {
		case "exact":
			exact[key] = &cmdCopy
		case "contains":
			contains = append(contains, containsEntry{pattern: key, command: &cmdCopy})
		case "regex":
			re, err := regexp.Compile(cmd.Trigger)
			if err != nil {
				slog.Warn("invalid regex command", "trigger", cmd.Trigger, "error", err)
				continue
			}
			regexes = append(regexes, regexEntry{regex: re, command: &cmdCopy})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	cs.mu.Lock()
	cs.commands = commands
	cs.exactCommands = exact
	cs.containsCommands = contains
	cs.regexCommands = regexes
	cs.lastLoad = time.Now()
	cs.mu.Unlock()

	slog.Info("commands loaded", "count", len(commands))
	return nil
}

// shouldRefreshCache returns true if the cache is stale.
func (cs *CommandService) shouldRefreshCache() bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return time.Since(cs.lastLoad) > cs.cacheTimeout
}

// refreshIfNeeded reloads commands if the cache is stale.
func (cs *CommandService) refreshIfNeeded(ctx context.Context) {
	if cs.shouldRefreshCache() {
		if err := cs.LoadCommands(ctx); err != nil {
			slog.Error("failed to refresh commands cache", "error", err)
		}
	}
}

// GetCommand returns a command by trigger name.
func (cs *CommandService) GetCommand(ctx context.Context, trigger string) *Command {
	cs.refreshIfNeeded(ctx)

	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.commands[strings.ToLower(trigger)]
}

// HasPermission checks if a user has permission to execute a command.
func (cs *CommandService) HasPermission(cmd *Command, user *ChatUserInfo) bool {
	switch cmd.Permission {
	case "everyone":
		return true
	case "subscriber":
		return user.IsSubscriber || user.IsModerator
	case "vip":
		return user.IsVIP || user.IsModerator
	case "moderator":
		return user.IsModerator
	}
	return true
}

// IsOnCooldown checks if a command is on cooldown for a specific user.
func (cs *CommandService) IsOnCooldown(commandID int, username string) bool {
	key := fmt.Sprintf("%d_%s", commandID, username)

	cs.cooldownMu.RLock()
	lastUsed, ok := cs.cooldowns[key]
	cs.cooldownMu.RUnlock()

	if !ok {
		return false
	}

	// Look up the command's cooldown from cache
	cs.mu.RLock()
	var cooldownSecs int
	for _, cmd := range cs.commands {
		if cmd.ID == commandID {
			cooldownSecs = cmd.Cooldown
			break
		}
	}
	cs.mu.RUnlock()

	return time.Since(lastUsed) < time.Duration(cooldownSecs)*time.Second
}

// SetCooldown records a cooldown for a command/user pair.
func (cs *CommandService) SetCooldown(commandID int, username string) {
	key := fmt.Sprintf("%d_%s", commandID, username)

	cs.cooldownMu.Lock()
	cs.cooldowns[key] = time.Now()
	cs.cooldownMu.Unlock()

	// 1% chance to clean up old cooldowns
	if rand.IntN(100) == 0 {
		go cs.cleanupCooldowns()
	}
}

// cleanupCooldowns removes cooldown entries older than 5 minutes.
func (cs *CommandService) cleanupCooldowns() {
	cutoff := time.Now().Add(-5 * time.Minute)

	cs.cooldownMu.Lock()
	for key, t := range cs.cooldowns {
		if t.Before(cutoff) {
			delete(cs.cooldowns, key)
		}
	}
	cs.cooldownMu.Unlock()
}

// ProcessResponse replaces placeholders in a command response.
func (cs *CommandService) ProcessResponse(response string, user *ChatUserInfo) string {
	nick := user.Username
	if len(nick) > 4 {
		nick = nick[:4]
	}
	r := strings.NewReplacer(
		"{user}", "@"+user.Username,
		"{nick}", nick,
		"{username}", user.Username,
		"{displayname}", user.DisplayName,
		"{channel}", user.Channel,
	)
	return r.Replace(response)
}

// FindMatchingCommand finds a command matching the given chat message.
// Priority: exact > contains > regex.
func (cs *CommandService) FindMatchingCommand(ctx context.Context, message string) *CommandMatch {
	cs.refreshIfNeeded(ctx)

	cs.mu.RLock()
	defer cs.mu.RUnlock()

	msgLower := strings.ToLower(message)

	// 1. Exact match (startsWith + boundary check)
	for trigger, cmd := range cs.exactCommands {
		if strings.HasPrefix(msgLower, trigger) {
			// Check word boundary: message is exactly the trigger or trigger is followed by space
			if len(msgLower) == len(trigger) || msgLower[len(trigger)] == ' ' {
				return &CommandMatch{Trigger: trigger, Command: cmd}
			}
		}
	}

	// 2. Contains match
	for _, entry := range cs.containsCommands {
		if strings.Contains(msgLower, entry.pattern) {
			return &CommandMatch{Trigger: entry.pattern, Command: entry.command}
		}
	}

	// 3. Regex match
	for _, entry := range cs.regexCommands {
		if entry.regex.MatchString(message) {
			return &CommandMatch{Trigger: entry.command.Trigger, Command: entry.command}
		}
	}

	return nil
}

// ExecuteCommand runs the full command execution pipeline.
// Returns the response message to send to chat, or empty string if command was not executed.
func (cs *CommandService) ExecuteCommand(ctx context.Context, trigger string, user *ChatUserInfo) (response string, audioURL string, executed bool) {
	cmd := cs.GetCommand(ctx, trigger)
	if cmd == nil {
		return "", "", false
	}

	if !cs.HasPermission(cmd, user) {
		return "", "", false
	}

	if cs.IsOnCooldown(cmd.ID, user.Username) {
		return "", "", false
	}

	// Check and deduct egg cost
	if cmd.EggCost > 0 && cs.eggSvc != nil {
		_, err := cs.eggSvc.UpdateUserEggs(ctx, user.TwitchUserID, user.Username, -cmd.EggCost)
		if err != nil {
			if err == ErrInsufficientEggs {
				return fmt.Sprintf("@%s You need %d %s to use this command!", user.Username, cmd.EggCost, cs.pointsName), "", false
			}
			slog.Error("egg cost deduction failed", "error", err, "command", trigger)
			return "", "", false
		}
	}

	response = cs.ProcessResponse(cmd.Response, user)
	cs.SetCooldown(cmd.ID, user.Username)

	// Update usage count (fire-and-forget)
	go cs.updateUsageCount(ctx, cmd.ID)

	return response, cmd.AudioURL, true
}

// updateUsageCount increments the usage counter for a command.
func (cs *CommandService) updateUsageCount(ctx context.Context, commandID int) {
	_, err := cs.db.Exec(ctx,
		`UPDATE custom_commands SET usage_count = usage_count + 1, last_used_at = CURRENT_TIMESTAMP WHERE id = $1`,
		commandID,
	)
	if err != nil {
		slog.Error("failed to update command usage count", "error", err, "command_id", commandID)
	}
}

// GetAllCommands returns all cached commands.
func (cs *CommandService) GetAllCommands(ctx context.Context) []*Command {
	cs.refreshIfNeeded(ctx)

	cs.mu.RLock()
	defer cs.mu.RUnlock()

	cmds := make([]*Command, 0, len(cs.commands))
	for _, cmd := range cs.commands {
		cmds = append(cmds, cmd)
	}
	return cmds
}

// ReloadCommands forces a cache refresh.
func (cs *CommandService) ReloadCommands(ctx context.Context) error {
	return cs.LoadCommands(ctx)
}
