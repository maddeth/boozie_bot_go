package services

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

// OBSService controls OBS Studio via the OBS WebSocket v5 protocol.
// It connects on demand, sends commands, and disconnects — matching the JS behaviour.
type OBSService struct {
	address  string // e.g. "ws://192.168.1.69:4455"
	password string
}

// NewOBSService creates a new OBS WebSocket client.
func NewOBSService(address, password string) *OBSService {
	return &OBSService{
		address:  address,
		password: password,
	}
}

// ChangeColour connects to OBS and updates the colour filter on the configured sources.
// hexColour should be a 6-character hex string (no # prefix), e.g. "FF00AA".
func (s *OBSService) ChangeColour(ctx context.Context, hexColour string) error {
	if s.address == "" {
		return fmt.Errorf("OBS address not configured")
	}

	conn, err := s.connect(ctx)
	if err != nil {
		return fmt.Errorf("OBS connect: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	// Convert hex to OBS decimal colour (ABGR format).
	obsColour, err := hexToOBSColour(hexColour)
	if err != nil {
		return fmt.Errorf("invalid hex colour %q: %w", hexColour, err)
	}

	// Update both source filters (matching JS behaviour).
	sources := []struct{ source, filter string }{
		{"Webcam shadow", "colour"},
		{"Muse Shadow", "colour"},
	}

	for _, src := range sources {
		if err := s.setSourceFilterSettings(ctx, conn, src.source, src.filter, map[string]any{
			"color": obsColour,
		}); err != nil {
			slog.Error("OBS filter update failed", "source", src.source, "error", err)
			return err
		}
	}

	slog.Info("OBS colour updated", "hex", hexColour, "obsDecimal", obsColour)
	return nil
}

// IsConnected tests whether OBS is reachable.
func (s *OBSService) IsConnected(ctx context.Context) bool {
	if s.address == "" {
		return false
	}
	conn, err := s.connect(ctx)
	if err != nil {
		return false
	}
	conn.Close(websocket.StatusNormalClosure, "")
	return true
}

// --- OBS WebSocket v5 protocol ---

// connect dials OBS and performs the authentication handshake.
func (s *OBSService) connect(ctx context.Context) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, s.address, nil)
	if err != nil {
		return nil, err
	}

	// Read Hello (OpCode 0).
	var hello obsMessage
	if err := s.readJSON(ctx, conn, &hello); err != nil {
		conn.Close(websocket.StatusAbnormalClosure, "")
		return nil, fmt.Errorf("reading Hello: %w", err)
	}
	if hello.Op != 0 {
		conn.Close(websocket.StatusAbnormalClosure, "")
		return nil, fmt.Errorf("expected OpCode 0 (Hello), got %d", hello.Op)
	}

	// Build Identify (OpCode 1).
	identify := obsMessage{
		Op: 1,
		D: map[string]any{
			"rpcVersion": 1,
		},
	}

	// Authenticate if required.
	if auth, ok := hello.D["authentication"].(map[string]any); ok {
		challenge, _ := auth["challenge"].(string)
		salt, _ := auth["salt"].(string)
		if challenge != "" && salt != "" {
			authStr := generateOBSAuth(s.password, salt, challenge)
			identify.D["authentication"] = authStr
		}
	}

	if err := s.writeJSON(ctx, conn, identify); err != nil {
		conn.Close(websocket.StatusAbnormalClosure, "")
		return nil, fmt.Errorf("sending Identify: %w", err)
	}

	// Read Identified (OpCode 2).
	var identified obsMessage
	if err := s.readJSON(ctx, conn, &identified); err != nil {
		conn.Close(websocket.StatusAbnormalClosure, "")
		return nil, fmt.Errorf("reading Identified: %w", err)
	}
	if identified.Op != 2 {
		conn.Close(websocket.StatusAbnormalClosure, "")
		return nil, fmt.Errorf("expected OpCode 2 (Identified), got %d", identified.Op)
	}

	slog.Debug("OBS WebSocket connected and authenticated")
	return conn, nil
}

// setSourceFilterSettings sends a SetSourceFilterSettings request.
func (s *OBSService) setSourceFilterSettings(ctx context.Context, conn *websocket.Conn, sourceName, filterName string, settings map[string]any) error {
	req := obsMessage{
		Op: 6, // Request
		D: map[string]any{
			"requestType": "SetSourceFilterSettings",
			"requestId":   "filter-" + sourceName,
			"requestData": map[string]any{
				"sourceName":     sourceName,
				"filterName":     filterName,
				"filterSettings": settings,
			},
		},
	}

	if err := s.writeJSON(ctx, conn, req); err != nil {
		return err
	}

	// Read RequestResponse (OpCode 7).
	var resp obsMessage
	if err := s.readJSON(ctx, conn, &resp); err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	if resp.Op != 7 {
		return fmt.Errorf("expected OpCode 7 (RequestResponse), got %d", resp.Op)
	}

	// Check for errors in the response.
	if status, ok := resp.D["requestStatus"].(map[string]any); ok {
		if result, ok := status["result"].(bool); ok && !result {
			code, _ := status["code"].(float64)
			comment, _ := status["comment"].(string)
			return fmt.Errorf("OBS request failed (code %.0f): %s", code, comment)
		}
	}

	return nil
}

type obsMessage struct {
	Op int                    `json:"op"`
	D  map[string]any         `json:"d"`
}

func (s *OBSService) readJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (s *OBSService) writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}

// --- Auth helpers ---

// generateOBSAuth computes the OBS WebSocket v5 authentication string.
// Algorithm: base64(SHA256(base64(SHA256(password + salt)) + challenge))
func generateOBSAuth(password, salt, challenge string) string {
	// Step 1: SHA256(password + salt) -> base64
	h1 := sha256.Sum256([]byte(password + salt))
	secret := base64.StdEncoding.EncodeToString(h1[:])

	// Step 2: SHA256(secret + challenge) -> base64
	h2 := sha256.Sum256([]byte(secret + challenge))
	return base64.StdEncoding.EncodeToString(h2[:])
}

// hexToOBSColour converts a 6-char hex string (e.g. "FF00AA") to OBS decimal colour (ABGR).
// Matches the JS logic: reverse byte order, prepend "ff" for full alpha.
func hexToOBSColour(hex string) (int64, error) {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) != 6 {
		return 0, fmt.Errorf("expected 6 hex characters, got %d", len(hex))
	}

	// Reverse byte pairs: "RRGGBB" -> "BBGGRR", then prepend "ff" for alpha.
	reversed := hex[4:6] + hex[2:4] + hex[0:2]
	obsHex := "ff" + reversed

	return strconv.ParseInt(obsHex, 16, 64)
}
