package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/maddeth/boozie-bot/internal/auth"
	"github.com/maddeth/boozie-bot/internal/services"
)

// ColourHandler handles /api/colours/* endpoints.
type ColourHandler struct {
	colours *services.ColourService
	auth    *auth.Middleware
}

// NewColourHandler creates a new colour handler.
func NewColourHandler(colours *services.ColourService, authMW *auth.Middleware) *ColourHandler {
	return &ColourHandler{colours: colours, auth: authMW}
}

// Register registers colour routes on the given mux.
func (h *ColourHandler) Register(mux *http.ServeMux) {
	// All colour endpoints require auth
	mux.Handle("GET /api/colours", h.auth.AuthenticateToken(http.HandlerFunc(h.getAll)))
	mux.Handle("POST /api/colours", h.auth.AuthenticateToken(http.HandlerFunc(h.addColour)))
	mux.Handle("GET /api/colours/username", h.auth.AuthenticateToken(http.HandlerFunc(h.myColours)))
	mux.Handle("POST /api/colours/username", h.auth.AuthenticateToken(http.HandlerFunc(h.coloursByUsername)))
	mux.Handle("POST /api/colours/hex", h.auth.AuthenticateToken(http.HandlerFunc(h.colourByHex)))
	mux.Handle("POST /api/colours/colourName", h.auth.AuthenticateToken(http.HandlerFunc(h.hexByColourName)))
	mux.Handle("POST /api/colours/search", h.auth.AuthenticateToken(http.HandlerFunc(h.searchByName)))
	mux.Handle("GET /api/colours/getLastColour", h.auth.AuthenticateToken(http.HandlerFunc(h.getLastColour)))

	// Moderator-only endpoints (rename + delete colours)
	mux.Handle("PUT /api/colours/{id}", h.auth.AuthenticateToken(h.auth.RequireModeratorRole(http.HandlerFunc(h.renameColour))))
	mux.Handle("DELETE /api/colours/{id}", h.auth.AuthenticateToken(h.auth.RequireModeratorRole(http.HandlerFunc(h.deleteColour))))

	// Test endpoint
	mux.HandleFunc("POST /api/", h.testEndpoint)
}

func (h *ColourHandler) testEndpoint(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": body.Text})
}

func (h *ColourHandler) getAll(w http.ResponseWriter, r *http.Request) {
	colours, err := h.colours.GetAll(r.Context())
	if err != nil {
		logAndError(w, "Failed to fetch colours", err)
		return
	}
	writeJSON(w, http.StatusOK, colours)
}

func (h *ColourHandler) addColour(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetClaims(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	var body struct {
		Colour string `json:"colour"`
		Hex    string `json:"hex"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	colour := sanitizeColourInput(body.Colour)
	hex := sanitizeHexInput(body.Hex)

	if colour == "" || len(hex) != 6 {
		writeError(w, http.StatusBadRequest, "Valid colour name and 6-digit hex value are required")
		return
	}

	// Use the nickname from JWT metadata or email as username
	username := claims.Email
	if username == "" {
		username = "unknown"
	}

	err := h.colours.Add(r.Context(), colour, hex, username)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		logAndError(w, "Failed to add colour", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"message": "Colour added successfully"})
}

func (h *ColourHandler) myColours(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetClaims(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	username := claims.Email
	if username == "" {
		username = "unknown"
	}

	colours, err := h.colours.GetByUsername(r.Context(), username)
	if err != nil {
		logAndError(w, "Failed to fetch colours", err)
		return
	}
	writeJSON(w, http.StatusOK, colours)
}

func (h *ColourHandler) coloursByUsername(w http.ResponseWriter, r *http.Request) {
	claims := auth.GetClaims(r.Context())
	if claims == nil {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	var body struct {
		Username string `json:"username"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	username := body.Username
	if username == "" {
		username = claims.Email
	}

	colours, err := h.colours.GetByUsername(r.Context(), username)
	if err != nil {
		logAndError(w, "Failed to fetch colours", err)
		return
	}
	writeJSON(w, http.StatusOK, colours)
}

func (h *ColourHandler) colourByHex(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hex string `json:"hex"`
	}
	if err := readJSON(r, &body); err != nil || body.Hex == "" {
		writeError(w, http.StatusBadRequest, "hex not supplied")
		return
	}

	hex := sanitizeHexInput(body.Hex)
	if len(hex) != 6 {
		writeError(w, http.StatusBadRequest, "invalid hex value")
		return
	}

	names, err := h.colours.GetByHex(r.Context(), hex)
	if err != nil {
		logAndError(w, "Failed to fetch colour", err)
		return
	}
	writeJSON(w, http.StatusOK, names)
}

func (h *ColourHandler) hexByColourName(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Colour string `json:"colour"`
	}
	if err := readJSON(r, &body); err != nil || body.Colour == "" {
		writeError(w, http.StatusBadRequest, "colour name not supplied")
		return
	}

	colour := sanitizeColourInput(body.Colour)
	hex, err := h.colours.GetHexByName(r.Context(), colour)
	if err != nil {
		logAndError(w, "Failed to fetch colour", err)
		return
	}
	if hex == "" {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, hex)
}

func (h *ColourHandler) searchByName(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Colour string `json:"colour"`
	}
	if err := readJSON(r, &body); err != nil || body.Colour == "" {
		writeJSON(w, http.StatusOK, []services.Colour{})
		return
	}

	colour := sanitizeColourInput(body.Colour)
	colours, err := h.colours.SearchByName(r.Context(), colour)
	if err != nil {
		logAndError(w, "Failed to search colours", err)
		return
	}
	if colours == nil {
		colours = []services.Colour{}
	}
	writeJSON(w, http.StatusOK, colours)
}

func (h *ColourHandler) getLastColour(w http.ResponseWriter, r *http.Request) {
	colour, err := h.colours.GetLast(r.Context())
	if err != nil {
		logAndError(w, "Failed to fetch colour", err)
		return
	}
	writeJSON(w, http.StatusOK, colour)
}
func (h *ColourHandler) renameColour(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid colour ID")
		return
	}

	var body struct {
		Colour string `json:"colour"`
		Hex    string `json:"hex"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	newName := sanitizeColourInput(body.Colour)
	newHex := sanitizeHexInput(body.Hex)

	if newName == "" && len(newHex) != 6 {
		writeError(w, http.StatusBadRequest, "Provide at least a valid colour name or 6-digit hex value to update")
		return
	}

	if len(newHex) > 0 && len(newHex) != 6 {
		writeError(w, http.StatusBadRequest, "Hex value must be 6 characters")
		return
	}

	err = h.colours.Rename(r.Context(), id, newName, newHex)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, "Colour not found")
			return
		}
		logAndError(w, "Failed to rename colour", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Colour updated successfully"})
}

func (h *ColourHandler) deleteColour(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid colour ID")
		return
	}

	err = h.colours.Delete(r.Context(), id)
	if err != nil {
		logAndError(w, "Failed to delete colour", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Colour deleted successfully"})
}
