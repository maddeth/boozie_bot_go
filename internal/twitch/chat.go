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

// ChatClient wraps go-twitch-irc with message deduplication and bot-message filtering.
type ChatClient struct {
	client    *twitch.Client
	channel   string
	botUserID string
	handler   MessageHandler

	mu             sync.Mutex
	recentMessages map[string]time.Time
}

// NewChatClient creates a new IRC chat client.
func NewChatClient(username, oauthToken, channel, botUserID string) *ChatClient {
	client := twitch.NewClient(username, "oauth:"+oauthToken)

	cc := &ChatClient{
		client:         client,
		channel:        channel,
		botUserID:      botUserID,
		recentMessages: make(map[string]time.Time),
	}

	client.OnPrivateMessage(cc.onMessage)
	client.Join(channel)

	return cc
}

// OnMessage sets the handler for incoming chat messages.
func (cc *ChatClient) OnMessage(handler MessageHandler) {
	cc.handler = handler
}

// Connect starts the IRC connection. Blocks until disconnected.
func (cc *ChatClient) Connect() error {
	slog.Info("connecting to Twitch IRC", "channel", cc.channel)
	return cc.client.Connect()
}

// Disconnect closes the IRC connection.
func (cc *ChatClient) Disconnect() error {
	slog.Info("disconnecting from Twitch IRC")
	return cc.client.Disconnect()
}

// Say sends a message to the channel.
func (cc *ChatClient) Say(message string) {
	cc.client.Say(cc.channel, message)
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
