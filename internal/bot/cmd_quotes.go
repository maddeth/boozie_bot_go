package bot

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/maddeth/boozie-bot/internal/services"
	"github.com/maddeth/boozie-bot/internal/twitch"
)

// quoteDate returns the display date for a quote, preferring date_said over created_at.
func quoteDate(q *services.Quote) string {
	if q.DateSaid != nil {
		return q.DateSaid.Format("Jan 2, 2006")
	}
	return q.CreatedAt.Format("Jan 2, 2006")
}

// cmdQuote handles !quote [id] - get a random or specific quote.
func (b *Bot) cmdQuote(ctx context.Context, msg *twitch.ChatMessage) {
	args := strings.TrimSpace(stripInvisibleChars(msg.Text[len("!quote"):]))

	if args != "" {
		id, err := strconv.Atoi(args)
		if err != nil {
			// Not a number - ignore (might be a partial match like "!quotestuff")
			return
		}

		quote, err := b.quotes.GetQuoteByID(ctx, id)
		if err != nil {
			slog.Error("failed to get quote", "id", id, "error", err)
			b.sayf("%s - Error retrieving quote", msg.User.DisplayName)
			return
		}
		if quote == nil {
			b.sayf("%s - Quote #%d not found", msg.User.DisplayName, id)
			return
		}
		b.sayf("Quote #%d: \"%s\" - %s, %s",
			quote.ID, quote.QuoteText, quote.QuotedBy,
			quoteDate(quote))
		return
	}

	// Random quote
	quote, err := b.quotes.GetRandomQuote(ctx)
	if err != nil {
		slog.Error("failed to get random quote", "error", err)
		b.sayf("%s - Error retrieving quote", msg.User.DisplayName)
		return
	}
	if quote == nil {
		b.sayf("%s - No quotes available yet!", msg.User.DisplayName)
		return
	}
	b.sayf("Quote #%d: \"%s\" - %s, %s",
		quote.ID, quote.QuoteText, quote.QuotedBy,
		quoteDate(quote))
}

// cmdAddQuote handles !addquote <text> and !quoteadd <text> (moderator only).
func (b *Bot) cmdAddQuote(ctx context.Context, msg *twitch.ChatMessage) {
	perms := b.getPermissions(msg)
	if !perms.IsModerator {
		b.sayf("%s - Only moderators can add quotes", msg.User.DisplayName)
		return
	}

	// Both "!addquote " and "!quoteadd " are 10 characters
	quoteText := strings.TrimSpace(msg.Text[10:])
	if quoteText == "" {
		b.sayf("%s - Please provide quote text: !addquote <quote>", msg.User.DisplayName)
		return
	}

	var addedByID *string
	if msg.User.ID != "" {
		addedByID = &msg.User.ID
	}

	quote, err := b.quotes.AddQuote(ctx, quoteText, b.cfg.MyChannel, msg.User.DisplayName, addedByID)
	if err != nil {
		slog.Error("failed to add quote", "error", err)
		b.sayf("%s - Failed to add quote", msg.User.DisplayName)
		return
	}

	b.sayf("%s - Quote #%d added successfully!", msg.User.DisplayName, quote.ID)
	slog.Info("quote added", "id", quote.ID, "by", msg.User.DisplayName)
}

// cmdDelQuote handles !delquote <id> (moderator only).
func (b *Bot) cmdDelQuote(ctx context.Context, msg *twitch.ChatMessage) {
	perms := b.getPermissions(msg)
	if !perms.IsModerator {
		b.sayf("%s - Only moderators can delete quotes", msg.User.DisplayName)
		return
	}

	idStr := strings.TrimSpace(msg.Text[len("!delquote"):])
	id, err := strconv.Atoi(idStr)
	if err != nil || idStr == "" {
		b.sayf("%s - Please provide a valid quote ID: !delquote <id>", msg.User.DisplayName)
		return
	}

	quote, err := b.quotes.DeleteQuote(ctx, id)
	if err != nil {
		slog.Error("failed to delete quote", "error", err, "id", id)
		b.sayf("%s - Failed to delete quote", msg.User.DisplayName)
		return
	}
	if quote == nil {
		b.sayf("%s - Quote #%d not found", msg.User.DisplayName, id)
		return
	}

	b.sayf("%s - Quote #%d deleted successfully", msg.User.DisplayName, id)
	slog.Info("quote deleted", "id", id, "by", msg.User.DisplayName)
}
