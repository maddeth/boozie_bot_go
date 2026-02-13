package twitch

import (
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	twitch "github.com/gempir/go-twitch-irc/v4"
)

// invisibleCharRE matches invisible/zero-width characters used for message deduplication.
var invisibleCharRE = regexp.MustCompile(`[\x{034F}\x{200B}-\x{200D}\x{FEFF}\x{00A0}\x{180E}\x{2000}-\x{200F}\x{2028}-\x{202F}\x{205F}-\x{206F}\x{3000}\x{F3A0}]`)

// ChatUser holds user information extracted from a Twitch chat message.
type ChatUser struct {
	ID            string
	Name          string // login name (lowercase)
	DisplayName   string
	Color         string
	IsMod         bool
	IsVIP         bool
	IsSubscriber  bool
	IsBroadcaster bool
	Badges        map[string]int
}

// ChatMessage is our internal representation of a Twitch chat message.
type ChatMessage struct {
	User    ChatUser
	Channel string
	Text    string // original text (preserved case)
	Emotes  []*twitch.Emote
	ID      string
	Time    time.Time
}

// MessageHandler is the callback type for incoming chat messages.
type MessageHandler func(msg *ChatMessage)

// TokenFunc returns a fresh OAuth access token for the bot user.
type TokenFunc func() (string, error)

// ChatClient wraps go-twitch-irc with message deduplication, bot-message filtering,
// and automatic reconnection with token refresh.
type ChatClient struct {
	username  string
	channel   string
	botUserID string
	tokenFunc TokenFunc
	handler   MessageHandler

	mu             sync.Mutex
	client         *twitch.Client
	recentMessages map[string]time.Time
}

// NewChatClient creates a new IRC chat client.
// tokenFunc is called on each (re)connect to get a fresh OAuth token.
func NewChatClient(username string, tokenFunc TokenFunc, channel, botUserID string) *ChatClient {
	cc := &ChatClient{
		username:       username,
		channel:        channel,
		botUserID:      botUserID,
		tokenFunc:      tokenFunc,
		recentMessages: make(map[string]time.Time),
	}
	return cc
}

// OnMessage sets the handler for incoming chat messages.
func (cc *ChatClient) OnMessage(handler MessageHandler) {
	cc.handler = handler
}

// buildClient creates a new go-twitch-irc client with the given token.
func (cc *ChatClient) buildClient(token string) {
	client := twitch.NewClient(cc.username, "oauth:"+token)
	client.OnPrivateMessage(cc.onMessage)
	client.Join(cc.channel)

	cc.mu.Lock()
	cc.client = client
	cc.mu.Unlock()
}

// Connect connects to Twitch IRC with automatic reconnection and token refresh.
// Blocks indefinitely, reconnecting on errors with exponential backoff.
func (cc *ChatClient) Connect() error {
	backoff := 2 * time.Second
	maxBackoff := 2 * time.Minute

	for {
		token, err := cc.tokenFunc()
		if err != nil {
			slog.Error("failed to get IRC token, retrying", "error", err, "backoff", backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		cc.buildClient(token)
		slog.Info("connecting to Twitch IRC", "channel", cc.channel)

		err = cc.client.Connect()
		if err != nil {
			slog.Error("IRC connection error, reconnecting", "error", err, "backoff", backoff)
		} else {
			slog.Warn("IRC connection closed, reconnecting", "backoff", backoff)
		}

		time.Sleep(backoff)
		backoff = min(backoff*2, maxBackoff)
	}
}

// Disconnect closes the IRC connection.
func (cc *ChatClient) Disconnect() error {
	slog.Info("disconnecting from Twitch IRC")
	cc.mu.Lock()
	client := cc.client
	cc.mu.Unlock()
	if client != nil {
		return client.Disconnect()
	}
	return nil
}

// Say sends a message to the channel.
func (cc *ChatClient) Say(message string) {
	cc.mu.Lock()
	client := cc.client
	cc.mu.Unlock()
	if client != nil {
		client.Say(cc.channel, message)
	}
}

// onMessage handles incoming IRC messages with deduplication and filtering.
func (cc *ChatClient) onMessage(msg twitch.PrivateMessage) {
	// Skip bot's own messages
	if msg.User.ID == cc.botUserID {
		return
	}

	// Deduplication: strip invisible chars and check for recent duplicates
	cleanText := strings.TrimSpace(invisibleCharRE.ReplaceAllString(msg.Message, ""))
	dedupeKey := strings.ToLower(msg.User.Name) + ":" + cleanText

	cc.mu.Lock()
	now := time.Now()
	lastSeen, exists := cc.recentMessages[dedupeKey]
	cc.recentMessages[dedupeKey] = now

	// Clean up old entries (older than 5s)
	for k, t := range cc.recentMessages {
		if now.Sub(t) > 5*time.Second {
			delete(cc.recentMessages, k)
		}
	}
	cc.mu.Unlock()

	if exists && now.Sub(lastSeen) < 2*time.Second {
		slog.Debug("skipping duplicate message", "user", msg.User.Name)
		return
	}

	if cc.handler == nil {
		return
	}

	// Convert to our ChatMessage type
	chatMsg := &ChatMessage{
		User: ChatUser{
			ID:            msg.User.ID,
			Name:          msg.User.Name,
			DisplayName:   msg.User.DisplayName,
			Color:         msg.User.Color,
			IsMod:         msg.User.Badges["moderator"] > 0,
			IsVIP:         msg.User.Badges["vip"] > 0,
			IsSubscriber:  msg.User.Badges["subscriber"] > 0,
			IsBroadcaster: msg.User.Badges["broadcaster"] > 0,
			Badges:        msg.User.Badges,
		},
		Channel: msg.Channel,
		Text:    msg.Message,
		Emotes:  msg.Emotes,
		ID:      msg.ID,
		Time:    msg.Time,
	}

	cc.handler(chatMsg)
}
