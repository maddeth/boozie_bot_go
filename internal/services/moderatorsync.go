package services

import (
	"context"
	"log/slog"
)

// TwitchModInfo represents a moderator fetched from the Twitch API.
type TwitchModInfo struct {
	UserID    string
	UserLogin string
	UserName  string
}

// TwitchSyncClient abstracts the Twitch API calls needed for mod/sub sync.
// Implemented by twitch.HelixClient via an adapter in main.go.
type TwitchSyncClient interface {
	GetModeratorList(ctx context.Context) ([]TwitchModInfo, error)
	LookupUserByID(ctx context.Context, userID string) (login, displayName string, err error)
	GetSubTier(ctx context.Context, userID string) (tier string, err error)
}

// ModSyncResult holds the outcome of a moderator sync operation.
type ModSyncResult struct {
	Success         bool   `json:"success"`
	Error           string `json:"error,omitempty"`
	TotalTwitchMods int    `json:"totalTwitchMods"`
	TotalDBMods     int    `json:"totalDbMods"`
	Added           int    `json:"added"`
	Removed         int    `json:"removed"`
	Updated         int    `json:"updated"`
}

// SubSyncResult holds the outcome of a subscriber sync operation.
type SubSyncResult struct {
	Success       bool   `json:"success"`
	Error         string `json:"error,omitempty"`
	TotalChatters int    `json:"totalChatters"`
	Synced        int    `json:"synced"`
	Errors        int    `json:"errors"`
}

// ModeratorSyncService syncs Twitch moderator and subscriber status with the database.
type ModeratorSyncService struct {
	users         *UserService
	twitch        TwitchSyncClient
	broadcasterID string
}

// NewModeratorSyncService creates a new moderator sync service.
func NewModeratorSyncService(users *UserService, twitch TwitchSyncClient, broadcasterID string) *ModeratorSyncService {
	return &ModeratorSyncService{
		users:         users,
		twitch:        twitch,
		broadcasterID: broadcasterID,
	}
}

// SyncModerators fetches moderators from Twitch API and syncs with the database.
// Adds the broadcaster as a permanent moderator.
func (s *ModeratorSyncService) SyncModerators(ctx context.Context) ModSyncResult {
	slog.Info("starting moderator sync", "channelId", s.broadcasterID)

	// Get current moderators from Twitch API
	twitchMods, err := s.twitch.GetModeratorList(ctx)
	if err != nil {
		slog.Error("failed to fetch moderators from Twitch API", "error", err)
		return ModSyncResult{Success: false, Error: "failed to fetch from Twitch API: " + err.Error()}
	}

	// Ensure broadcaster is always in the moderator list
	broadcasterInList := false
	for _, mod := range twitchMods {
		if mod.UserID == s.broadcasterID {
			broadcasterInList = true
			break
		}
	}
	if !broadcasterInList {
		login, displayName, err := s.twitch.LookupUserByID(ctx, s.broadcasterID)
		if err == nil && login != "" {
			twitchMods = append(twitchMods, TwitchModInfo{
				UserID:    s.broadcasterID,
				UserLogin: login,
				UserName:  displayName,
			})
			slog.Info("added broadcaster to moderators list", "broadcaster", login)
		}
	}

	// Get current moderators from database
	dbMods, err := s.users.GetModerators(ctx)
	if err != nil {
		slog.Error("failed to get moderators from database", "error", err)
		return ModSyncResult{Success: false, Error: "failed to query database: " + err.Error()}
	}

	// Build lookup sets
	dbModIDs := make(map[string]bool, len(dbMods))
	for _, mod := range dbMods {
		if mod.TwitchUserID != nil {
			dbModIDs[*mod.TwitchUserID] = true
		}
	}

	twitchModIDs := make(map[string]bool, len(twitchMods))
	for _, mod := range twitchMods {
		twitchModIDs[mod.UserID] = true
	}

	var added, removed, updated int

	// Add new moderators and update existing ones
	for _, twitchMod := range twitchMods {
		dn := twitchMod.UserName
		_, err := s.users.GetOrCreateUser(ctx, twitchMod.UserID, twitchMod.UserLogin, &dn)
		if err != nil {
			slog.Error("error processing moderator", "username", twitchMod.UserLogin, "error", err)
			continue
		}

		if !dbModIDs[twitchMod.UserID] {
			if err := s.users.UpdateModeratorStatus(ctx, twitchMod.UserID, true); err != nil {
				slog.Error("error adding moderator", "username", twitchMod.UserLogin, "error", err)
				continue
			}
			added++
			slog.Info("added new moderator", "username", twitchMod.UserLogin, "twitchUserId", twitchMod.UserID)
		} else {
			updated++
		}
	}

	// Remove users who are no longer moderators (except broadcaster)
	for _, dbMod := range dbMods {
		if dbMod.TwitchUserID == nil {
			continue
		}
		twitchID := *dbMod.TwitchUserID

		if !twitchModIDs[twitchID] {
			// Never remove broadcaster from moderators
			if twitchID == s.broadcasterID {
				slog.Debug("skipping broadcaster removal from moderators", "username", dbMod.Username)
				continue
			}

			if err := s.users.UpdateModeratorStatus(ctx, twitchID, false); err != nil {
				slog.Error("error removing moderator status", "username", dbMod.Username, "error", err)
				continue
			}
			removed++
			slog.Info("removed moderator status", "username", dbMod.Username, "twitchUserId", twitchID)
		}
	}

	result := ModSyncResult{
		Success:         true,
		TotalTwitchMods: len(twitchMods),
		TotalDBMods:     len(dbMods),
		Added:           added,
		Removed:         removed,
		Updated:         updated,
	}

	slog.Info("moderator sync completed",
		"totalTwitch", result.TotalTwitchMods,
		"totalDB", result.TotalDBMods,
		"added", added, "removed", removed, "updated", updated)

	return result
}

// SyncSubscribers checks subscription status for all current chatters.
// chatters is a map of displayName -> twitchUserID (from HelixClient.GetChatters).
func (s *ModeratorSyncService) SyncSubscribers(ctx context.Context, chatters map[string]string) SubSyncResult {
	slog.Info("starting subscriber sync")

	var synced, errCount int

	// Deduplicate by userID (GetChatters maps both displayName and login to the same ID)
	seen := make(map[string]bool, len(chatters))

	for displayName, userID := range chatters {
		if seen[userID] {
			continue
		}
		seen[userID] = true

		// Ensure user exists in database
		dn := displayName
		_, err := s.users.GetOrCreateUser(ctx, userID, displayName, &dn)
		if err != nil {
			slog.Warn("error syncing subscriber user", "displayName", displayName, "error", err)
			errCount++
			continue
		}

		// Check subscription status
		tier, err := s.twitch.GetSubTier(ctx, userID)
		if err != nil {
			slog.Warn("error checking subscription", "displayName", displayName, "error", err)
			errCount++
			continue
		}

		isSubscriber := tier != "0"
		var tierPtr *string
		if isSubscriber {
			tierPtr = &tier
		}

		if err := s.users.UpdateSubscriptionStatus(ctx, userID, isSubscriber, tierPtr); err != nil {
			slog.Warn("error updating subscription status", "displayName", displayName, "error", err)
			errCount++
			continue
		}

		synced++
	}

	result := SubSyncResult{
		Success:       true,
		TotalChatters: len(seen),
		Synced:        synced,
		Errors:        errCount,
	}

	slog.Info("subscriber sync completed",
		"totalChatters", result.TotalChatters,
		"synced", synced, "errors", errCount)

	return result
}
