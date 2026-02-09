package handlers

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// StaticHandler serves static pages and TTS audio files.
type StaticHandler struct {
	ttsDirectory string
}

// NewStaticHandler creates a new static handler.
func NewStaticHandler(ttsDirectory string) *StaticHandler {
	return &StaticHandler{ttsDirectory: ttsDirectory}
}

// Register registers static routes on the given mux.
func (h *StaticHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /chat", h.chatPage)
	mux.HandleFunc("GET /tts/{id}", h.serveTTS)
	mux.HandleFunc("GET /{$}", h.rootRedirect)
}

func (h *StaticHandler) rootRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "https://www.twitch.tv/maddeth", http.StatusMovedPermanently)
}

func (h *StaticHandler) serveTTS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Path traversal protection: reject anything with slashes or dots
	if strings.ContainsAny(id, "./\\") || id == "" {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	filePath := filepath.Join(h.ttsDirectory, id+".mp3")
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "audio/mpeg")
	http.ServeFile(w, r, filePath)
}

func (h *StaticHandler) chatPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(chatHTML))
}

const chatHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Mirrored Chat - Teleprompter View</title>
    <style>
        * {
            margin: 0;
            padding: 0;
            box-sizing: border-box;
        }

        body {
            background: #000;
            color: #fff;
            font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
            height: 100vh;
            overflow: hidden;
            transform: scaleX(-1); /* Mirror by default for teleprompter */
        }

        .chat-container {
            height: 100vh;
            display: flex;
            flex-direction: column;
            padding: 70px 20px 20px 20px;
            overflow: hidden;
        }

        .connection-status {
            text-align: center;
            color: #fbbf24;
            padding: 20px;
            font-size: 18px;
        }

        .chat-messages {
            flex: 1;
            overflow-y: auto;
            overflow-x: hidden;
            display: flex;
            flex-direction: column;
            gap: 10px;
        }

        .chat-message {
            padding: 8px 12px;
            background: rgba(255, 255, 255, 0.05);
            border-radius: 8px;
            word-wrap: break-word;
            font-size: 24px;
            line-height: 1.4;
        }

        .chat-message.broadcaster {
            background: rgba(16, 185, 129, 0.15);
            border-left: 3px solid #10b981;
        }

        .chat-message.mod {
            background: rgba(139, 92, 246, 0.15);
            border-left: 3px solid #8b5cf6;
        }

        .chat-message.vip {
            background: rgba(236, 72, 153, 0.15);
            border-left: 3px solid #ec4899;
        }

        .chat-message.sub {
            background: rgba(251, 191, 36, 0.15);
            border-left: 3px solid #fbbf24;
        }

        .chat-message.redemption {
            background: rgba(147, 51, 234, 0.15);
            border-left: 3px solid #9333ea;
            border-radius: 12px;
            padding: 12px 16px;
        }

        .redemption-header {
            display: flex;
            align-items: center;
            gap: 8px;
            margin-bottom: 4px;
        }

        .redemption-icon {
            font-size: 1.2em;
        }

        .redemption-title {
            color: #c084fc;
            font-weight: bold;
            font-size: 0.9em;
        }

        .redemption-user {
            color: #9ca3af;
            font-size: 0.9em;
        }

        .redemption-input {
            color: #f3f4f6;
            margin-top: 4px;
            font-style: italic;
        }

        .chat-author {
            font-weight: bold;
            margin-right: 8px;
        }

        .chat-text {
            color: #f3f4f6;
        }

        .chat-text img {
            height: 1.5em;
            vertical-align: middle;
            margin: 0 2px;
            display: inline-block;
        }

        .controls {
            position: fixed;
            top: 20px;
            right: 20px;
            background: rgba(0, 0, 0, 0.8);
            padding: 10px;
            border-radius: 8px;
            transform: scaleX(-1); /* Un-mirror controls */
            display: flex;
            gap: 10px;
            align-items: center;
            z-index: 1000;
        }

        .clock {
            position: fixed;
            top: 20px;
            left: 20px;
            background: rgba(0, 0, 0, 0.8);
            padding: 10px 15px;
            border-radius: 8px;
            color: #fff;
            font-size: 24px;
            font-weight: bold;
            font-family: monospace;
            z-index: 1000;
        }

        .control-btn {
            background: rgba(255, 255, 255, 0.1);
            border: 1px solid rgba(255, 255, 255, 0.2);
            color: #fff;
            padding: 8px 12px;
            border-radius: 4px;
            cursor: pointer;
            font-size: 14px;
        }

        .control-btn:hover {
            background: rgba(255, 255, 255, 0.2);
        }

        .font-size-display {
            color: #9ca3af;
            font-size: 14px;
            min-width: 50px;
            text-align: center;
        }

        /* Scrollbar styling */
        .chat-messages::-webkit-scrollbar {
            width: 8px;
        }

        .chat-messages::-webkit-scrollbar-track {
            background: rgba(255, 255, 255, 0.1);
        }

        .chat-messages::-webkit-scrollbar-thumb {
            background: rgba(255, 255, 255, 0.3);
            border-radius: 4px;
        }
    </style>
</head>
<body>
    <div class="chat-container">
        <div id="connectionStatus" class="connection-status">
            Connecting to chat...
        </div>
        <div id="chatMessages" class="chat-messages"></div>
    </div>

    <div class="clock" id="clock"></div>

    <div class="controls">
        <button class="control-btn" onclick="toggleMirror()">Toggle Mirror</button>
        <button class="control-btn" onclick="changeFontSize(-2)">A-</button>
        <span class="font-size-display" id="fontSizeDisplay">24px</span>
        <button class="control-btn" onclick="changeFontSize(2)">A+</button>
        <button class="control-btn" onclick="clearChat()">Clear</button>
    </div>

    <script>
        let websocket = null;
        let connected = false;
        let messages = [];
        let fontSize = 24;
        let mirrored = true;
        const maxMessages = 50;
        let emotes = [];

        const userColors = {};
        function getUserColor(username, twitchColor) {
            if (twitchColor) {
                return twitchColor;
            }
            if (!userColors[username]) {
                const hue = Math.floor(Math.random() * 360);
                userColors[username] = "hsl(" + hue + ", 70%, 60%)";
            }
            return userColors[username];
        }

        function processMessageFragments(fragments) {
            if (!fragments || !Array.isArray(fragments)) {
                return fragments || '';
            }

            return fragments.map(function(fragment) {
                if (fragment.type === 'emote') {
                    const emote = fragment.content;
                    return '<img src="' + emote.url + '" alt="' + emote.name + '" title="' + emote.name + '" style="height: 1.5em; vertical-align: middle; margin: 0 2px;">';
                } else {
                    return fragment.content.replace(/[&<>"']/g, function(match) {
                        const escapes = {
                            '&': '&amp;',
                            '<': '&lt;',
                            '>': '&gt;',
                            '"': '&quot;',
                            "'": '&#x27;'
                        };
                        return escapes[match];
                    });
                }
            }).join('');
        }

        function connectWebSocket() {
            const wsProtocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
            const wsHost = wsProtocol + '//' + window.location.host + '/websocket/';

            try {
                websocket = new WebSocket(wsHost);

                websocket.onopen = function() {
                    console.log('Connected to chat WebSocket');
                    connected = true;
                    document.getElementById('connectionStatus').style.display = 'none';
                };

                websocket.onmessage = function(event) {
                    try {
                        const data = JSON.parse(event.data);

                        if (data.type === 'emotes' && data.emotes) {
                            emotes = data.emotes;
                            console.log('Loaded ' + emotes.length + ' emotes');
                        }
                        else if (data.type === 'batch' && data.messages) {
                            data.messages.forEach(function(msg) {
                                if (msg.type === 'chat') {
                                    addMessage(msg);
                                } else if (msg.type === 'redemption') {
                                    addRedemption(msg);
                                }
                            });
                        }
                        else if (data.type === 'chat') {
                            addMessage(data);
                        }
                        else if (data.type === 'redemption') {
                            addRedemption(data);
                        }
                    } catch (error) {
                        console.error('Error parsing WebSocket message:', error);
                    }
                };

                websocket.onerror = function(error) {
                    console.error('WebSocket error:', error);
                    connected = false;
                    showConnectionStatus('Connection error');
                };

                websocket.onclose = function() {
                    console.log('WebSocket connection closed');
                    connected = false;
                    showConnectionStatus('Reconnecting...');
                    setTimeout(connectWebSocket, 3000);
                };
            } catch (error) {
                console.error('Failed to create WebSocket connection:', error);
                showConnectionStatus('Failed to connect');
            }
        }

        function showConnectionStatus(message) {
            const status = document.getElementById('connectionStatus');
            status.textContent = message;
            status.style.display = 'block';
        }

        function addMessage(data) {
            const messageDiv = document.createElement('div');
            messageDiv.className = 'chat-message';

            if (data.isBroadcaster) messageDiv.classList.add('broadcaster');
            else if (data.isMod) messageDiv.classList.add('mod');
            else if (data.isVip) messageDiv.classList.add('vip');
            else if (data.isSubscriber) messageDiv.classList.add('sub');

            const authorColor = getUserColor(data.user, data.color);
            const processedMessage = processMessageFragments(data.parsedMessage);
            messageDiv.innerHTML =
                '<span class="chat-author" style="color: ' + authorColor + '">' + data.user + ':</span>' +
                '<span class="chat-text">' + processedMessage + '</span>';

            const chatContainer = document.getElementById('chatMessages');
            chatContainer.appendChild(messageDiv);

            const messageElements = chatContainer.children;
            if (messageElements.length > maxMessages) {
                chatContainer.removeChild(messageElements[0]);
            }

            chatContainer.scrollTop = chatContainer.scrollHeight;
        }

        function addRedemption(data) {
            const messageDiv = document.createElement('div');
            messageDiv.className = 'chat-message redemption';

            let redemptionHTML =
                '<div class="redemption-header">' +
                '<span class="redemption-title">' + data.rewardTitle + '</span>' +
                '<span class="redemption-user">by ' + data.user + '</span>' +
                '</div>';

            if (data.userInput && data.userInput.trim() !== '') {
                redemptionHTML += '<div class="redemption-input">' + data.userInput + '</div>';
            }

            messageDiv.innerHTML = redemptionHTML;

            const chatContainer = document.getElementById('chatMessages');
            chatContainer.appendChild(messageDiv);

            const messageElements = chatContainer.children;
            if (messageElements.length > maxMessages) {
                chatContainer.removeChild(messageElements[0]);
            }

            chatContainer.scrollTop = chatContainer.scrollHeight;
        }

        function toggleMirror() {
            mirrored = !mirrored;
            document.body.style.transform = mirrored ? 'scaleX(-1)' : 'scaleX(1)';
        }

        function changeFontSize(delta) {
            const newSize = fontSize + delta;
            if (newSize >= 12 && newSize <= 48) {
                fontSize = newSize;
                document.getElementById('fontSizeDisplay').textContent = fontSize + 'px';
                document.querySelectorAll('.chat-message').forEach(function(msg) {
                    msg.style.fontSize = fontSize + 'px';
                });
                document.querySelectorAll('.chat-text img').forEach(function(img) {
                    img.style.height = '1.5em';
                });
            }
        }

        function clearChat() {
            document.getElementById('chatMessages').innerHTML = '';
        }

        function updateClock() {
            const now = new Date();
            const hours = now.getHours().toString().padStart(2, '0');
            const minutes = now.getMinutes().toString().padStart(2, '0');
            const seconds = now.getSeconds().toString().padStart(2, '0');
            document.getElementById('clock').textContent = hours + ':' + minutes + ':' + seconds;
        }

        updateClock();
        setInterval(updateClock, 1000);
        connectWebSocket();
    </script>
</body>
</html>`
