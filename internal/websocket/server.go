package websocket

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"nhooyr.io/websocket"
)

// Server manages WebSocket connections and implements the bot.Broadcaster interface.
type Server struct {
	port       string
	httpServer *http.Server

	// Client management.
	clientsMu sync.RWMutex
	clients   map[string]*client

	// Per-IP connection tracking (IP -> *int32 count).
	connectionsByIP sync.Map

	// Rate limiting.
	rateLimitMu        sync.Mutex
	connectionAttempts map[string]*rateLimitEntry

	// Message batching.
	batchMu      sync.Mutex
	messageQueue []any
	batchNotify  chan struct{} // signals that batch is full and needs immediate flush

	// Emote caching.
	emoteCacheMu sync.RWMutex
	cachedEmotes json.RawMessage

	// Shutdown coordination.
	shutdownCh chan struct{}
	wg         sync.WaitGroup
}

type client struct {
	id      string
	conn    *websocket.Conn
	ip      string
	writeMu sync.Mutex
}

type rateLimitEntry struct {
	count        int
	firstAttempt time.Time
}

// ConnectionStats holds metrics for the health endpoint.
type ConnectionStats struct {
	TotalConnections    int            `json:"totalConnections"`
	ConnectionsByIP     map[string]int `json:"connectionsByIP"`
	UniqueIPs           int            `json:"uniqueIPs"`
	MaxConnectionsPerIP int            `json:"maxConnectionsPerIP"`
	MaxTotalConnections int            `json:"maxTotalConnections"`
}

// Configuration constants matching the JS implementation.
const (
	maxConnectionsPerIP  = 5
	maxTotalConnections  = 100
	heartbeatInterval    = 30 * time.Second
	heartbeatTimeout     = 5 * time.Second
	batchInterval        = 50 * time.Millisecond
	maxBatchSize         = 10
	rateLimitWindow      = 60 * time.Second
	maxAttemptsPerWindow = 10
	rateLimitCleanup     = 5 * time.Minute
	writeTimeout         = 5 * time.Second
)

// New creates a new WebSocket server on the given port.
func New(port string) *Server {
	return &Server{
		port:               port,
		clients:            make(map[string]*client),
		connectionAttempts: make(map[string]*rateLimitEntry),
		messageQueue:       make([]any, 0, maxBatchSize),
		batchNotify:        make(chan struct{}, 1),
		shutdownCh:         make(chan struct{}),
	}
}

// Start listens for WebSocket connections and blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleUpgrade)

	s.httpServer = &http.Server{
		Addr:    ":" + s.port,
		Handler: mux,
	}

	// Start background goroutines.
	s.wg.Add(3)
	go s.heartbeatLoop()
	go s.batchLoop()
	go s.rateLimitCleanupLoop()

	// Start HTTP server in background.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("WebSocket server listening", "port", s.port)
		if err := s.httpServer.ListenAndServe(); err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Wait for shutdown signal or fatal error.
	select {
	case <-ctx.Done():
		return s.shutdown()
	case err := <-errCh:
		return err
	}
}

// Broadcast queues a message for batched delivery to all connected clients.
func (s *Server) Broadcast(msg any) {
	s.batchMu.Lock()
	s.messageQueue = append(s.messageQueue, msg)
	full := len(s.messageQueue) >= maxBatchSize
	s.batchMu.Unlock()

	if full {
		// Non-blocking signal to flush immediately.
		select {
		case s.batchNotify <- struct{}{}:
		default:
		}
	}
}

// BroadcastImmediate sends a message to all clients immediately, bypassing batching.
// Caches emote messages so new clients receive them on connect.
func (s *Server) BroadcastImmediate(msg any) {
	// Cache emotes for new client onboarding.
	if m, ok := msg.(map[string]any); ok {
		if m["type"] == "emotes" {
			if emotes, ok := m["emotes"]; ok {
				if data, err := json.Marshal(emotes); err == nil {
					s.emoteCacheMu.Lock()
					s.cachedEmotes = data
					s.emoteCacheMu.Unlock()
					slog.Info("cached emotes for new clients")
				}
			}
		}
	}

	data, err := json.Marshal(msg)
	if err != nil {
		slog.Error("BroadcastImmediate marshal failed", "error", err)
		return
	}
	s.sendToAll(data)
}

// Stats returns current connection statistics.
func (s *Server) Stats() ConnectionStats {
	s.clientsMu.RLock()
	total := len(s.clients)
	s.clientsMu.RUnlock()

	byIP := make(map[string]int)
	s.connectionsByIP.Range(func(key, val any) bool {
		byIP[key.(string)] = int(atomic.LoadInt32(val.(*int32)))
		return true
	})

	return ConnectionStats{
		TotalConnections:    total,
		ConnectionsByIP:     byIP,
		UniqueIPs:           len(byIP),
		MaxConnectionsPerIP: maxConnectionsPerIP,
		MaxTotalConnections: maxTotalConnections,
	}
}

// --- Connection handling ---

func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	clientIP := getClientIP(r)

	if !s.checkRateLimit(clientIP) {
		slog.Warn("WebSocket rejected: rate limit", "ip", clientIP)
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	s.clientsMu.RLock()
	total := len(s.clients)
	s.clientsMu.RUnlock()
	if total >= maxTotalConnections {
		slog.Warn("WebSocket rejected: server at capacity", "ip", clientIP, "total", total)
		http.Error(w, "Server at capacity", http.StatusServiceUnavailable)
		return
	}

	if s.getIPCount(clientIP) >= maxConnectionsPerIP {
		slog.Warn("WebSocket rejected: per-IP limit", "ip", clientIP)
		http.Error(w, "Too many connections from this IP", http.StatusTooManyRequests)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // allow any origin (bot frontend may be on different domain)
	})
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err, "ip", clientIP)
		return
	}

	c := &client{
		id:   uuid.New().String(),
		conn: conn,
		ip:   clientIP,
	}

	s.clientsMu.Lock()
	s.clients[c.id] = c
	s.clientsMu.Unlock()
	s.incrementIP(clientIP)

	slog.Debug("WebSocket client connected", "id", c.id, "ip", clientIP, "total", total+1)

	// Send cached emotes to new client.
	s.emoteCacheMu.RLock()
	cached := s.cachedEmotes
	s.emoteCacheMu.RUnlock()
	if cached != nil {
		envelope, _ := json.Marshal(map[string]any{
			"type":   "emotes",
			"emotes": json.RawMessage(cached),
		})
		s.sendToClient(c, envelope)
	}

	// Client read loop (blocks until disconnect).
	s.wg.Add(1)
	go s.readLoop(c)
}

func (s *Server) readLoop(c *client) {
	defer s.wg.Done()
	defer s.removeClient(c)

	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			return // connection closed or error
		}

		var msg struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}

		if msg.Type == "ping" {
			resp, _ := json.Marshal(map[string]any{
				"type":      "pong",
				"timestamp": time.Now().UnixMilli(),
			})
			s.sendToClient(c, resp)
		}
	}
}

// --- Batching ---

func (s *Server) batchLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdownCh:
			return
		case <-s.batchNotify:
			s.flushBatch()
		case <-ticker.C:
			s.flushBatch()
		}
	}
}

func (s *Server) flushBatch() {
	s.batchMu.Lock()
	if len(s.messageQueue) == 0 {
		s.batchMu.Unlock()
		return
	}

	// Take up to maxBatchSize messages.
	n := len(s.messageQueue)
	if n > maxBatchSize {
		n = maxBatchSize
	}
	batch := make([]any, n)
	copy(batch, s.messageQueue[:n])
	s.messageQueue = s.messageQueue[n:]
	s.batchMu.Unlock()

	envelope, err := json.Marshal(map[string]any{
		"type":     "batch",
		"messages": batch,
		"count":    len(batch),
	})
	if err != nil {
		slog.Error("batch marshal failed", "error", err)
		return
	}

	s.sendToAll(envelope)

	// If there are still messages queued, signal another flush.
	s.batchMu.Lock()
	more := len(s.messageQueue) > 0
	s.batchMu.Unlock()
	if more {
		select {
		case s.batchNotify <- struct{}{}:
		default:
		}
	}
}

// --- Heartbeat ---

func (s *Server) heartbeatLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			s.pingAllClients()
		}
	}
}

func (s *Server) pingAllClients() {
	s.clientsMu.RLock()
	snapshot := make([]*client, 0, len(s.clients))
	for _, c := range s.clients {
		snapshot = append(snapshot, c)
	}
	s.clientsMu.RUnlock()

	var removed int
	for _, c := range snapshot {
		ctx, cancel := context.WithTimeout(context.Background(), heartbeatTimeout)
		err := c.conn.Ping(ctx)
		cancel()
		if err != nil {
			slog.Debug("client unresponsive, removing", "id", c.id)
			s.removeClient(c)
			removed++
		}
	}

	if removed > 0 {
		s.clientsMu.RLock()
		remaining := len(s.clients)
		s.clientsMu.RUnlock()
		slog.Info("cleaned unresponsive clients", "removed", removed, "remaining", remaining)
	}
}

// --- Rate limiting ---

func (s *Server) checkRateLimit(ip string) bool {
	s.rateLimitMu.Lock()
	defer s.rateLimitMu.Unlock()

	now := time.Now()
	entry, ok := s.connectionAttempts[ip]

	if !ok {
		s.connectionAttempts[ip] = &rateLimitEntry{count: 1, firstAttempt: now}
		return true
	}

	if now.Sub(entry.firstAttempt) > rateLimitWindow {
		s.connectionAttempts[ip] = &rateLimitEntry{count: 1, firstAttempt: now}
		return true
	}

	entry.count++
	return entry.count <= maxAttemptsPerWindow
}

func (s *Server) rateLimitCleanupLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(rateLimitCleanup)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			s.cleanRateLimits()
		}
	}
}

func (s *Server) cleanRateLimits() {
	s.rateLimitMu.Lock()
	defer s.rateLimitMu.Unlock()

	now := time.Now()
	var cleaned int
	for ip, entry := range s.connectionAttempts {
		if now.Sub(entry.firstAttempt) > rateLimitWindow {
			delete(s.connectionAttempts, ip)
			cleaned++
		}
	}
	if cleaned > 0 {
		slog.Debug("cleaned rate limit entries", "count", cleaned)
	}
}

// --- IP tracking ---

func (s *Server) getIPCount(ip string) int {
	val, ok := s.connectionsByIP.Load(ip)
	if !ok {
		return 0
	}
	return int(atomic.LoadInt32(val.(*int32)))
}

func (s *Server) incrementIP(ip string) {
	val, _ := s.connectionsByIP.LoadOrStore(ip, new(int32))
	atomic.AddInt32(val.(*int32), 1)
}

func (s *Server) decrementIP(ip string) {
	val, ok := s.connectionsByIP.Load(ip)
	if !ok {
		return
	}
	if atomic.AddInt32(val.(*int32), -1) <= 0 {
		s.connectionsByIP.Delete(ip)
	}
}

// --- Helpers ---

func (s *Server) sendToAll(data []byte) {
	s.clientsMu.RLock()
	snapshot := make([]*client, 0, len(s.clients))
	for _, c := range s.clients {
		snapshot = append(snapshot, c)
	}
	s.clientsMu.RUnlock()

	for _, c := range snapshot {
		s.sendToClient(c, data)
	}
}

func (s *Server) sendToClient(c *client, data []byte) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()

	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		slog.Error("write to client failed", "id", c.id, "error", err)
		go s.removeClient(c)
	}
}

func (s *Server) removeClient(c *client) {
	s.clientsMu.Lock()
	_, exists := s.clients[c.id]
	if !exists {
		s.clientsMu.Unlock()
		return
	}
	delete(s.clients, c.id)
	s.clientsMu.Unlock()

	s.decrementIP(c.ip)
	c.conn.Close(websocket.StatusNormalClosure, "")
	slog.Debug("client removed", "id", c.id)
}

func (s *Server) shutdown() error {
	slog.Info("shutting down WebSocket server")

	close(s.shutdownCh)

	// Flush pending messages.
	s.flushBatch()

	// Close all client connections.
	s.clientsMu.Lock()
	for id, c := range s.clients {
		c.conn.Close(websocket.StatusGoingAway, "server shutting down")
		delete(s.clients, id)
	}
	s.clientsMu.Unlock()

	// Shutdown HTTP listener.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("WebSocket HTTP shutdown error", "error", err)
	}

	// Wait for all goroutines to exit.
	s.wg.Wait()
	slog.Info("WebSocket server stopped")
	return nil
}

func getClientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		return strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
		return realIP
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}
