package handlers

import (
	"net/http"

	"github.com/maddeth/boozie-bot/internal/auth"
	"github.com/maddeth/boozie-bot/internal/services"
)

// QuoteHandler handles /api/quotes/* endpoints.
type QuoteHandler struct {
	quotes *services.QuoteService
	auth   *auth.Middleware
}

// NewQuoteHandler creates a new quote handler.
func NewQuoteHandler(quotes *services.QuoteService, authMW *auth.Middleware) *QuoteHandler {
	return &QuoteHandler{quotes: quotes, auth: authMW}
}

// Register registers quote routes on the given mux.
func (h *QuoteHandler) Register(mux *http.ServeMux) {
	// Public
	mux.HandleFunc("GET /api/quotes/", h.getAll)
	mux.HandleFunc("GET /api/quotes/random", h.random)
	mux.HandleFunc("GET /api/quotes/stats/count", h.count)
	mux.HandleFunc("GET /api/quotes/user/{username}", h.byUser)
	mux.HandleFunc("GET /api/quotes/{id}", h.getByID)

	// Moderator-protected
	mux.Handle("POST /api/quotes/", h.auth.AuthenticateToken(h.auth.RequireModeratorRole(http.HandlerFunc(h.create))))
	mux.Handle("PUT /api/quotes/{id}", h.auth.AuthenticateToken(h.auth.RequireModeratorRole(http.HandlerFunc(h.update))))
	mux.Handle("DELETE /api/quotes/{id}", h.auth.AuthenticateToken(h.auth.RequireModeratorRole(http.HandlerFunc(h.delete))))
}

func (h *QuoteHandler) getAll(w http.ResponseWriter, r *http.Request) {
	page := parseIntParam(r, "page", 1, 0)
	limit := parseIntParam(r, "limit", 50, 0)
	search := r.URL.Query().Get("search")

	var result *services.QuotePage
	var err error

	if search != "" {
		result, err = h.quotes.SearchQuotes(r.Context(), search, page, limit)
	} else {
		result, err = h.quotes.GetAllQuotes(r.Context(), page, limit)
	}
	if err != nil {
		logAndError(w, "Failed to fetch quotes", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *QuoteHandler) random(w http.ResponseWriter, r *http.Request) {
	quote, err := h.quotes.GetRandomQuote(r.Context())
	if err != nil {
		logAndError(w, "Failed to fetch random quote", err)
		return
	}
	if quote == nil {
		writeError(w, http.StatusNotFound, "No quotes found")
		return
	}
	writeJSON(w, http.StatusOK, quote)
}

func (h *QuoteHandler) getByID(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "Invalid quote ID")
		return
	}

	quote, err := h.quotes.GetQuoteByID(r.Context(), id)
	if err != nil {
		logAndError(w, "Failed to fetch quote", err)
		return
	}
	if quote == nil {
		writeError(w, http.StatusNotFound, "Quote not found")
		return
	}
	writeJSON(w, http.StatusOK, quote)
}

func (h *QuoteHandler) byUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	page := parseIntParam(r, "page", 1, 0)
	limit := parseIntParam(r, "limit", 50, 0)

	result, err := h.quotes.GetQuotesByUser(r.Context(), username, page, limit)
	if err != nil {
		logAndError(w, "Failed to fetch user quotes", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *QuoteHandler) count(w http.ResponseWriter, r *http.Request) {
	count, err := h.quotes.GetQuoteCount(r.Context())
	if err != nil {
		logAndError(w, "Failed to fetch quote count", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"count": count})
}

func (h *QuoteHandler) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		QuoteText string `json:"quote_text"`
		QuotedBy  string `json:"quoted_by"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if body.QuoteText == "" || body.QuotedBy == "" {
		writeError(w, http.StatusBadRequest, "Quote text and author are required")
		return
	}

	user := auth.GetUser(r.Context())
	var addedBy string
	var addedByID *string
	if user != nil {
		addedBy = user.Username
		addedByID = &user.TwitchUserID
	}

	quote, err := h.quotes.AddQuote(r.Context(), body.QuoteText, body.QuotedBy, addedBy, addedByID)
	if err != nil {
		logAndError(w, "Failed to add quote", err)
		return
	}
	writeJSON(w, http.StatusCreated, quote)
}

func (h *QuoteHandler) update(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "Invalid quote ID")
		return
	}

	var body struct {
		QuoteText string `json:"quote_text"`
		QuotedBy  string `json:"quoted_by"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if body.QuoteText == "" || body.QuotedBy == "" {
		writeError(w, http.StatusBadRequest, "Quote text and author are required")
		return
	}

	quote, err := h.quotes.UpdateQuote(r.Context(), id, body.QuoteText, body.QuotedBy)
	if err != nil {
		logAndError(w, "Failed to update quote", err)
		return
	}
	if quote == nil {
		writeError(w, http.StatusNotFound, "Quote not found")
		return
	}
	writeJSON(w, http.StatusOK, quote)
}

func (h *QuoteHandler) delete(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathInt(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "Invalid quote ID")
		return
	}

	quote, err := h.quotes.DeleteQuote(r.Context(), id)
	if err != nil {
		logAndError(w, "Failed to delete quote", err)
		return
	}
	if quote == nil {
		writeError(w, http.StatusNotFound, "Quote not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Quote deleted successfully"})
}
