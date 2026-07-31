package websocket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

// TestIntegration_FullBotFlow simulates a real bot session:
// emote load → clients connect → chat messages → alert redemption → new client gets cached emotes.
func TestIntegration_FullBotFlow(t *testing.T) {
	srv, url, cleanup := startTestServer(t)
	defer cleanup()

	t.Log("=== Phase 1: Bot loads emotes on startup ===")
	emotes := []any{
		map[string]any{"id": "1", "code": "Kappa", "source": "twitch"},
		map[string]any{"id": "2", "code": "PogChamp", "source": "twitch"},
		map[string]any{"id": "3", "code": "catJAM", "source": "bttv"},
		map[string]any{"id": "4", "code": "Chatting", "source": "7tv"},
	}
	srv.BroadcastImmediate(map[string]any{
		"type":   "emotes",
		"emotes": emotes,
	})
	t.Logf("  Loaded %d emotes into cache", len(emotes))

	t.Log("=== Phase 2: Chat overlay (client 1) and alerts page (client 2) connect ===")
	chatOverlay := dial(t, url)
	defer chatOverlay.Close(websocket.StatusNormalClosure, "")
	alertsPage := dial(t, url)
	defer alertsPage.Close(websocket.StatusNormalClosure, "")
	time.Sleep(50 * time.Millisecond)

	// Both should receive cached emotes on connect.
	for _, name := range []string{"chatOverlay", "alertsPage"} {
		var conn *websocket.Conn
		if name == "chatOverlay" {
			conn = chatOverlay
		} else {
			conn = alertsPage
		}
		msg := readJSON(t, conn)
		if msg["type"] != "emotes" {
			t.Fatalf("%s: expected emotes on connect, got %v", name, msg["type"])
		}
		gotEmotes := msg["emotes"].([]any)
		t.Logf("  %s received %d cached emotes", name, len(gotEmotes))
	}

	stats := srv.Stats()
	t.Logf("  Connected clients: %d, Unique IPs: %d", stats.TotalConnections, stats.UniqueIPs)

	t.Log("=== Phase 3: Chat messages arrive (batched) ===")
	// Simulate 4 chat messages arriving quickly (like a busy chat).
	chatMessages := []map[string]any{
		{"type": "chat", "user": "boozie_", "message": "hello everyone!", "isMod": true, "isSubscriber": true, "color": "#FF4500", "timestamp": "2026-02-08T17:30:00.000Z"},
		{"type": "chat", "user": "viewer42", "message": "PogChamp PogChamp", "isMod": false, "isSubscriber": false, "color": "#1E90FF", "timestamp": "2026-02-08T17:30:00.100Z"},
		{"type": "chat", "user": "nightbot_fan", "message": "!eggs", "isMod": false, "isSubscriber": true, "color": "#00FF7F", "timestamp": "2026-02-08T17:30:00.200Z"},
		{"type": "chat", "user": "mod_person", "message": "!so viewer42", "isMod": true, "isSubscriber": false, "color": "#FFD700", "timestamp": "2026-02-08T17:30:00.300Z"},
	}
	for _, cm := range chatMessages {
		srv.Broadcast(cm)
	}

	// Both clients should receive a single batch with all 4 messages.
	for _, name := range []string{"chatOverlay", "alertsPage"} {
		var conn *websocket.Conn
		if name == "chatOverlay" {
			conn = chatOverlay
		} else {
			conn = alertsPage
		}
		batch := readJSON(t, conn)
		if batch["type"] != "batch" {
			t.Fatalf("%s: expected batch, got %v", name, batch["type"])
		}
		count := int(batch["count"].(float64))
		messages := batch["messages"].([]any)
		t.Logf("  %s received batch of %d chat messages", name, count)
		for _, m := range messages {
			cm := m.(map[string]any)
			t.Logf("    [%s] %s: %s", cm["color"], cm["user"], cm["message"])
		}
	}

	t.Log("=== Phase 4: Channel point redemption triggers alert ===")
	// This is what EventSub sends via BroadcastImmediate.
	srv.BroadcastImmediate(map[string]any{
		"type":     "redeem",
		"audioUrl": "/alerts/sounds/airhorn.mp3",
		"gifUrl":   "/alerts/gifs/airhorn.gif",
		"duration": 3000,
	})

	for _, name := range []string{"chatOverlay", "alertsPage"} {
		var conn *websocket.Conn
		if name == "chatOverlay" {
			conn = chatOverlay
		} else {
			conn = alertsPage
		}
		msg := readJSON(t, conn)
		if msg["type"] != "redeem" {
			t.Fatalf("%s: expected redeem, got %v", name, msg["type"])
		}
		t.Logf("  %s received alert: %s (gif: %s, %vms)", name, msg["audioUrl"], msg["gifUrl"], msg["duration"])
	}

	t.Log("=== Phase 5: Custom command triggers audio ===")
	// This is what cmd_custom.go sends when a command has audio.
	srv.Broadcast(map[string]any{
		"type":     "redeem",
		"audioUrl": "/alerts/sounds/bruh.mp3",
	})
	time.Sleep(100 * time.Millisecond) // wait for batch

	for _, name := range []string{"chatOverlay", "alertsPage"} {
		var conn *websocket.Conn
		if name == "chatOverlay" {
			conn = chatOverlay
		} else {
			conn = alertsPage
		}
		batch := readJSON(t, conn)
		if batch["type"] != "batch" {
			t.Fatalf("%s: expected batch, got %v", name, batch["type"])
		}
		msgs := batch["messages"].([]any)
		audio := msgs[0].(map[string]any)
		t.Logf("  %s received batched audio: %s", name, audio["audioUrl"])
	}

	t.Log("=== Phase 6: New client connects late, gets cached emotes ===")
	lateClient := dial(t, url)
	defer lateClient.Close(websocket.StatusNormalClosure, "")

	msg := readJSON(t, lateClient)
	if msg["type"] != "emotes" {
		t.Fatalf("late client: expected emotes, got %v", msg["type"])
	}
	gotEmotes := msg["emotes"].([]any)
	t.Logf("  Late client received %d cached emotes on connect", len(gotEmotes))

	t.Log("=== Phase 7: Connection stats ===")
	stats = srv.Stats()
	t.Logf("  Total connections: %d", stats.TotalConnections)
	t.Logf("  Unique IPs: %d", stats.UniqueIPs)
	t.Logf("  Max per IP: %d", stats.MaxConnectionsPerIP)
	t.Logf("  Max total: %d", stats.MaxTotalConnections)

	t.Log("=== All phases passed! ===")
}

// TestIntegration_ConnectionRejection demonstrates all rejection scenarios.
func TestIntegration_ConnectionRejection(t *testing.T) {
	srv, url, cleanup := startTestServer(t)
	defer cleanup()

	t.Log("=== Scenario 1: Per-IP limit (max 5) ===")
	conns := make([]*websocket.Conn, maxConnectionsPerIP)
	for i := 0; i < maxConnectionsPerIP; i++ {
		conns[i] = dial(t, url)
		defer conns[i].Close(websocket.StatusNormalClosure, "")
	}
	time.Sleep(50 * time.Millisecond)
	t.Logf("  Opened %d connections from same IP", maxConnectionsPerIP)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, _, err := websocket.Dial(ctx, url, nil)
	cancel()
	if err != nil {
		t.Logf("  6th connection correctly rejected: %v", err)
	} else {
		t.Fatal("  6th connection should have been rejected")
	}

	t.Log("=== Scenario 2: Rate limit (disconnect and reconnect rapidly) ===")
	// Close all connections first.
	for _, c := range conns {
		c.Close(websocket.StatusNormalClosure, "")
	}
	time.Sleep(100 * time.Millisecond)

	// The previous 5 connects + 1 rejected = 6 attempts already consumed.
	// Burn through the remaining 4 attempts.
	for i := 0; i < maxAttemptsPerWindow-6; i++ {
		c := dial(t, url)
		c.Close(websocket.StatusNormalClosure, "")
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("  Burned through %d rate limit attempts", maxAttemptsPerWindow)

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	_, _, err = websocket.Dial(ctx2, url, nil)
	cancel2()
	if err != nil {
		t.Logf("  Rate-limited connection correctly rejected: %v", err)
	} else {
		t.Fatal("  Rate-limited connection should have been rejected")
	}

	stats := srv.Stats()
	t.Logf("  Final stats: %d connections, %d unique IPs", stats.TotalConnections, stats.UniqueIPs)
	t.Log("=== All rejection scenarios passed! ===")
}

// TestIntegration_ConcurrentBroadcast simulates high-throughput chat with concurrent broadcasts.
func TestIntegration_ConcurrentBroadcast(t *testing.T) {
	srv, url, cleanup := startTestServer(t)
	defer cleanup()

	conn := dial(t, url)
	defer conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(50 * time.Millisecond)

	t.Log("=== Sending 50 messages from 10 goroutines concurrently ===")
	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				srv.Broadcast(map[string]any{
					"type":    "chat",
					"user":    fmt.Sprintf("user_%d", goroutineID),
					"message": fmt.Sprintf("msg %d from goroutine %d", i, goroutineID),
				})
			}
		}(g)
	}
	wg.Wait()

	// Read all batches until we've received 50 messages total.
	totalReceived := 0
	deadline := time.After(3 * time.Second)
	for totalReceived < 50 {
		select {
		case <-deadline:
			t.Fatalf("timeout: received only %d/50 messages", totalReceived)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, data, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read error after %d messages: %v", totalReceived, err)
		}
		var batch map[string]any
		json.Unmarshal(data, &batch)
		if batch["type"] == "batch" {
			count := int(batch["count"].(float64))
			totalReceived += count
			t.Logf("  Received batch of %d (total: %d/50)", count, totalReceived)
		}
	}
	t.Logf("=== All 50 messages received across batches ===")
}

// TestIntegration_ClientPingPong verifies the application-level ping/pong flow.
func TestIntegration_ClientPingPong(t *testing.T) {
	_, url, cleanup := startTestServer(t)
	defer cleanup()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()

	conn := dial(t, url)
	defer conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(50 * time.Millisecond)

	t.Log("=== Client sends JSON ping ===")
	ctx := context.Background()
	ping, _ := json.Marshal(map[string]any{"type": "ping"})
	conn.Write(ctx, websocket.MessageText, ping)

	msg := readJSON(t, conn)
	if msg["type"] != "pong" {
		t.Fatalf("expected pong, got %v", msg["type"])
	}
	ts_val := int64(msg["timestamp"].(float64))
	t.Logf("  Server responded with pong (timestamp: %d)", ts_val)

	t.Log("=== Client sends non-ping message (ignored) ===")
	other, _ := json.Marshal(map[string]any{"type": "hello"})
	conn.Write(ctx, websocket.MessageText, other)

	// Should not get a response - verify by sending a real ping after and checking that's what we get.
	conn.Write(ctx, websocket.MessageText, ping)
	msg = readJSON(t, conn)
	if msg["type"] != "pong" {
		t.Fatalf("expected pong (not echo of hello), got %v", msg["type"])
	}
	t.Log("  Non-ping message correctly ignored")

	t.Log("=== Client sends invalid JSON (ignored) ===")
	conn.Write(ctx, websocket.MessageText, []byte("not json"))
	conn.Write(ctx, websocket.MessageText, ping)
	msg = readJSON(t, conn)
	if msg["type"] != "pong" {
		t.Fatalf("expected pong after invalid JSON, got %v", msg["type"])
	}
	t.Log("  Invalid JSON correctly ignored")

	_ = strings.TrimSpace("") // avoid import error
	t.Log("=== All ping/pong scenarios passed! ===")
}
