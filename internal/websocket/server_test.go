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

// startTestServer creates a Server wired to an httptest.Server for testing.
// Returns the Server and a cleanup function.
func startTestServer(t *testing.T) (*Server, string, func()) {
	t.Helper()

	s := New("0") // port unused — httptest provides its own listener

	// Wire the upgrade handler into an httptest server.
	ts := httptest.NewServer(http.HandlerFunc(s.handleUpgrade))

	// Start background goroutines.
	s.shutdownCh = make(chan struct{})
	s.wg.Add(3)
	go s.heartbeatLoop()
	go s.batchLoop()
	go s.rateLimitCleanupLoop()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	cleanup := func() {
		close(s.shutdownCh)
		ts.Close()
		s.wg.Wait()
	}
	return s, wsURL, cleanup
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	return conn
}

func readJSON(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	return msg
}

func TestBroadcast_Batching(t *testing.T) {
	s, url, cleanup := startTestServer(t)
	defer cleanup()

	conn := dial(t, url)
	defer conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(50 * time.Millisecond) // let connection register

	// Send 3 messages (below maxBatchSize).
	s.Broadcast(map[string]any{"type": "chat", "msg": "hello"})
	s.Broadcast(map[string]any{"type": "chat", "msg": "world"})
	s.Broadcast(map[string]any{"type": "chat", "msg": "test"})

	// Wait for batch timer to fire.
	msg := readJSON(t, conn)
	if msg["type"] != "batch" {
		t.Fatalf("expected batch, got %v", msg["type"])
	}
	count := int(msg["count"].(float64))
	if count != 3 {
		t.Fatalf("expected 3 messages in batch, got %d", count)
	}
	messages := msg["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(messages))
	}
}

func TestBroadcast_FlushOnFull(t *testing.T) {
	s, url, cleanup := startTestServer(t)
	defer cleanup()

	conn := dial(t, url)
	defer conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(50 * time.Millisecond)

	// Send exactly maxBatchSize messages to trigger immediate flush.
	for i := 0; i < maxBatchSize; i++ {
		s.Broadcast(map[string]any{"type": "chat", "n": i})
	}

	msg := readJSON(t, conn)
	if msg["type"] != "batch" {
		t.Fatalf("expected batch, got %v", msg["type"])
	}
	if int(msg["count"].(float64)) != maxBatchSize {
		t.Fatalf("expected %d, got %v", maxBatchSize, msg["count"])
	}
}

func TestBroadcastImmediate(t *testing.T) {
	s, url, cleanup := startTestServer(t)
	defer cleanup()

	conn := dial(t, url)
	defer conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(50 * time.Millisecond)

	s.BroadcastImmediate(map[string]any{"type": "alert", "audioUrl": "/test.mp3"})

	msg := readJSON(t, conn)
	if msg["type"] != "alert" {
		t.Fatalf("expected alert, got %v", msg["type"])
	}
	if msg["audioUrl"] != "/test.mp3" {
		t.Fatalf("expected /test.mp3, got %v", msg["audioUrl"])
	}
}

func TestEmoteCaching(t *testing.T) {
	s, url, cleanup := startTestServer(t)
	defer cleanup()

	// Broadcast emotes before any client connects.
	emotes := []any{
		map[string]any{"id": "1", "code": "Kappa"},
		map[string]any{"id": "2", "code": "PogChamp"},
	}
	s.BroadcastImmediate(map[string]any{"type": "emotes", "emotes": emotes})

	// Now connect — new client should receive cached emotes.
	conn := dial(t, url)
	defer conn.Close(websocket.StatusNormalClosure, "")

	msg := readJSON(t, conn)
	if msg["type"] != "emotes" {
		t.Fatalf("expected emotes, got %v", msg["type"])
	}
	gotEmotes := msg["emotes"].([]any)
	if len(gotEmotes) != 2 {
		t.Fatalf("expected 2 emotes, got %d", len(gotEmotes))
	}
}

func TestClientPing(t *testing.T) {
	_, url, cleanup := startTestServer(t)
	defer cleanup()

	conn := dial(t, url)
	defer conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(50 * time.Millisecond)

	// Send a JSON ping.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ping, _ := json.Marshal(map[string]any{"type": "ping"})
	if err := conn.Write(ctx, websocket.MessageText, ping); err != nil {
		t.Fatalf("write ping failed: %v", err)
	}

	msg := readJSON(t, conn)
	if msg["type"] != "pong" {
		t.Fatalf("expected pong, got %v", msg["type"])
	}
	if msg["timestamp"] == nil {
		t.Fatal("pong missing timestamp")
	}
}

func TestConnectionLimit_PerIP(t *testing.T) {
	_, url, cleanup := startTestServer(t)
	defer cleanup()

	conns := make([]*websocket.Conn, maxConnectionsPerIP)
	for i := 0; i < maxConnectionsPerIP; i++ {
		conns[i] = dial(t, url)
		defer conns[i].Close(websocket.StatusNormalClosure, "")
	}
	time.Sleep(50 * time.Millisecond)

	// 6th connection should be rejected.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := websocket.Dial(ctx, url, nil)
	if err == nil {
		t.Fatal("expected 6th connection to be rejected")
	}
}

func TestConnectionLimit_Total(t *testing.T) {
	// This test only checks that Stats reflects the correct count.
	s, url, cleanup := startTestServer(t)
	defer cleanup()

	conn := dial(t, url)
	defer conn.Close(websocket.StatusNormalClosure, "")
	time.Sleep(50 * time.Millisecond)

	stats := s.Stats()
	if stats.TotalConnections != 1 {
		t.Fatalf("expected 1 connection, got %d", stats.TotalConnections)
	}
	if stats.UniqueIPs != 1 {
		t.Fatalf("expected 1 unique IP, got %d", stats.UniqueIPs)
	}
}

func TestRateLimit(t *testing.T) {
	s, url, cleanup := startTestServer(t)
	defer cleanup()

	// Burn through the rate limit window.
	conns := make([]*websocket.Conn, 0)
	for i := 0; i < maxAttemptsPerWindow; i++ {
		c := dial(t, url)
		conns = append(conns, c)
		// Close immediately so per-IP limit doesn't block us.
		c.Close(websocket.StatusNormalClosure, "")
		time.Sleep(5 * time.Millisecond) // let server process disconnect
	}

	// Next attempt should be rate limited.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := websocket.Dial(ctx, url, nil)
	if err == nil {
		t.Fatal("expected rate-limited connection to be rejected")
	}

	// Verify the rate limit entry exists.
	s.rateLimitMu.Lock()
	if len(s.connectionAttempts) == 0 {
		t.Fatal("expected rate limit entries")
	}
	s.rateLimitMu.Unlock()
}

func TestMultipleClients(t *testing.T) {
	s, url, cleanup := startTestServer(t)
	defer cleanup()

	// Connect 3 clients.
	var clients [3]*websocket.Conn
	for i := range clients {
		clients[i] = dial(t, url)
		defer clients[i].Close(websocket.StatusNormalClosure, "")
	}
	time.Sleep(50 * time.Millisecond)

	// Broadcast a message.
	s.BroadcastImmediate(map[string]any{"type": "test", "value": 42})

	// All 3 should receive it.
	var wg sync.WaitGroup
	wg.Add(3)
	for i := range clients {
		go func(idx int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, data, err := clients[idx].Read(ctx)
			if err != nil {
				t.Errorf("client %d read failed: %v", idx, err)
				return
			}
			var msg map[string]any
			json.Unmarshal(data, &msg)
			if msg["type"] != "test" {
				t.Errorf("client %d: expected test, got %v", idx, msg["type"])
			}
		}(i)
	}
	wg.Wait()
}

func TestStats(t *testing.T) {
	s, url, cleanup := startTestServer(t)
	defer cleanup()

	// No connections yet.
	stats := s.Stats()
	if stats.TotalConnections != 0 {
		t.Fatalf("expected 0 connections, got %d", stats.TotalConnections)
	}

	conn1 := dial(t, url)
	defer conn1.Close(websocket.StatusNormalClosure, "")
	conn2 := dial(t, url)
	defer conn2.Close(websocket.StatusNormalClosure, "")
	time.Sleep(50 * time.Millisecond)

	stats = s.Stats()
	if stats.TotalConnections != 2 {
		t.Fatalf("expected 2 connections, got %d", stats.TotalConnections)
	}
	if stats.MaxConnectionsPerIP != maxConnectionsPerIP {
		t.Fatalf("expected maxPerIP=%d, got %d", maxConnectionsPerIP, stats.MaxConnectionsPerIP)
	}
}

func TestDisconnectCleanup(t *testing.T) {
	s, url, cleanup := startTestServer(t)
	defer cleanup()

	conn := dial(t, url)
	time.Sleep(50 * time.Millisecond)

	stats := s.Stats()
	if stats.TotalConnections != 1 {
		t.Fatalf("expected 1 connection, got %d", stats.TotalConnections)
	}

	// Client disconnects.
	conn.Close(websocket.StatusNormalClosure, "bye")
	time.Sleep(100 * time.Millisecond)

	stats = s.Stats()
	if stats.TotalConnections != 0 {
		t.Fatalf("expected 0 connections after disconnect, got %d", stats.TotalConnections)
	}
}

func TestGetClientIP(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		remote string
		want   string
	}{
		{
			name:   "X-Forwarded-For",
			header: http.Header{"X-Forwarded-For": {"1.2.3.4, 5.6.7.8"}},
			remote: "9.0.0.1:12345",
			want:   "1.2.3.4",
		},
		{
			name:   "X-Real-IP",
			header: http.Header{"X-Real-Ip": {"10.0.0.1"}},
			remote: "9.0.0.1:12345",
			want:   "10.0.0.1",
		},
		{
			name:   "RemoteAddr",
			header: http.Header{},
			remote: "192.168.1.1:54321",
			want:   "192.168.1.1",
		},
		{
			name:   "RemoteAddr no port",
			header: http.Header{},
			remote: "192.168.1.1",
			want:   "192.168.1.1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{
				Header:     tc.header,
				RemoteAddr: tc.remote,
			}
			got := getClientIP(r)
			if got != tc.want {
				t.Errorf("getClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRateLimitCleanup(t *testing.T) {
	s := New("0")

	// Insert an expired entry.
	s.rateLimitMu.Lock()
	s.connectionAttempts["1.2.3.4"] = &rateLimitEntry{
		count:        5,
		firstAttempt: time.Now().Add(-2 * rateLimitWindow),
	}
	// Insert a fresh entry.
	s.connectionAttempts["5.6.7.8"] = &rateLimitEntry{
		count:        1,
		firstAttempt: time.Now(),
	}
	s.rateLimitMu.Unlock()

	s.cleanRateLimits()

	s.rateLimitMu.Lock()
	defer s.rateLimitMu.Unlock()
	if _, exists := s.connectionAttempts["1.2.3.4"]; exists {
		t.Fatal("expired entry should have been cleaned")
	}
	if _, exists := s.connectionAttempts["5.6.7.8"]; !exists {
		t.Fatal("fresh entry should still exist")
	}
}

func TestBroadcast_EmptyQueue(t *testing.T) {
	s := New("0")
	// Should not panic on empty queue.
	s.flushBatch()
}

func TestCheckRateLimit_WindowReset(t *testing.T) {
	s := New("0")
	ip := "10.0.0.1"

	// Insert an expired entry.
	s.rateLimitMu.Lock()
	s.connectionAttempts[ip] = &rateLimitEntry{
		count:        maxAttemptsPerWindow + 5,
		firstAttempt: time.Now().Add(-2 * rateLimitWindow),
	}
	s.rateLimitMu.Unlock()

	// Should be allowed because window expired.
	if !s.checkRateLimit(ip) {
		t.Fatal("expected rate limit to reset after window expires")
	}
}

func TestBroadcasterInterface(t *testing.T) {
	// Verify Server satisfies the Broadcaster-like interface.
	s := New("0")
	var _ interface {
		Broadcast(any)
		BroadcastImmediate(any)
	} = s
	_ = fmt.Sprintf("server: %p", s) // use s
}
