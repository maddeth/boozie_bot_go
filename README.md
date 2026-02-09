# boozie_bot — Go Backend

The Go backend for boozie_bot. A single static binary that handles Twitch IRC, EventSub webhooks, a REST API, WebSocket server, and OBS integration.

~9,800 lines of application code, ~1,100 lines of tests, 3 direct dependencies.

## Dependencies

| Package | Purpose |
|---------|---------|
| `jackc/pgx/v5` | PostgreSQL driver with connection pooling |
| `gempir/go-twitch-irc/v4` | Twitch IRC client |
| `golang-jwt/jwt/v5` | Supabase JWT verification |

Indirect: `nhooyr.io/websocket`, `google/uuid`, and pgx internals.

## Package Overview

```
cmd/bot/
  main.go                   Entry point. Wires all components, runs periodic tasks,
                            handles graceful shutdown.

internal/
  config/config.go          Loads config.json (shared format with the JS bot).

  database/db.go            pgx connection pool. Health check on startup.

  auth/
    jwt.go                  HMAC-SHA256 JWT verification against SUPABASE_JWT_SECRET.
    middleware.go           AuthenticateToken, RequireModeratorRole, RequireAdminRole.
                            Injects claims into request context.

  twitch/
    oauth.go                Token management. Loads Twurple-format JSON files,
                            auto-refreshes via Twitch OAuth2, saves back to disk.
                            Also handles app access tokens (client credentials).
    helix.go                Helix API: user lookup, stream status, chatters (paginated),
                            subscription tier, shoutout, moderator list.
    chat.go                 IRC client wrapping go-twitch-irc. Message dedup via
                            invisible character stripping. Bot-message filtering.
    eventsub.go             EventSub webhook handler. HMAC-SHA256 signature verification,
                            replay protection, async processing, message ID dedup.
                            Handles: sub, resub, gift sub, follow, channel point
                            redemptions (Shadow Colour, egg conversions, alerts).

  bot/
    bot.go                  Chat command router. Dispatches messages to cmd_*.go handlers.
                            Manages chatter map, permissions, auto-shoutout.
    cmd_eggs.go             !eggs, !topeggs, !addeggs (mod)
    cmd_pools.go            !pool, !donate, !pools, !createpool (mod), !deletepool (mod)
    cmd_quotes.go           !quote, !addquote, !delquote (mod)
    cmd_shoutouts.go        !so / !shoutout (mod) — Helix API lookup + shoutout call
    cmd_merge.go            !mergeeggs (mod) — preview then execute
    cmd_system.go           !commands (lists custom + built-in), !reloadcommands (mod)
    cmd_custom.go           Fallthrough handler for database-driven custom commands

  services/
    users.go                User CRUD with 5min TTL cache. GetOrCreateUser with
                            3-step resolution (twitch_id -> username -> create).
    eggs.go                 Transactional egg updates (fixes JS race condition),
                            leaderboard, stats.
    commands.go             Custom commands with 60s cache, 3-tier matching
                            (exact/contains/regex), per-user cooldowns, egg costs.
    quotes.go               Quotes with pagination, search, soft delete.
    colours.go              Colour CRUD, search, duplicate detection, random by name.
    pools.go                Egg pools with transactional donations.
    alerts.go               Alert config with 60s cache, partial updates.
    usermerge.go            Transactional account merging with preview and history.
    shoutouts.go            Auto-shoutout list with per-stream tracking.
    emotes.go               BTTV + 7TV + Twitch emote fetching with 1hr cache.
                            ParseMessage replaces text with emote img tags.
    moderatorsync.go        Mod/sub sync via TwitchSyncClient interface (avoids
                            import cycle with twitch package).
    obs.go                  OBS WebSocket v5: auth handshake, colour filter control,
                            hex-to-ABGR conversion.
    tts.go                  Google Cloud TTS REST API, MP3 generation.

  handlers/
    helpers.go              writeJSON, readJSON, parseIntParam, CORS middleware.
    middleware.go           Request logger (method, path, status, duration).
    api.go                  ColourHandler: 7 colour endpoints + test endpoint.
    eggs.go                 EggHandler: my-eggs (with rank), leaderboard, stats.
    alerts.go               AlertHandler: CRUD, mod-protected writes.
    pools.go                PoolHandler: CRUD, donate (auth), admin adjust.
    commands.go             CommandHandler: CRUD, usage tracking, cache reload.
    quotes.go               QuoteHandler: paginated, search, random, CRUD.
    shoutouts.go            ShoutoutHandler: list, add (Helix lookup), delete.
    usermerge.go            UserMergeHandler: preview, execute, history. Admin-only.
    userroles.go            UserRoleHandler: /me, roles, link, moderators, stats.
    webhooks.go             EventSub notification receiver + webhook subscription
                            creation (app access token via client credentials).
    static.go               Chat teleprompter HTML, TTS file serving, root redirect.

  websocket/
    server.go               WebSocket server. Per-IP limits (5/IP, 100 total),
                            rate limiting (10/60s), heartbeat (30s), message batching
                            (50ms / 10 msg flush), emote caching for new clients.
    server_test.go          16 unit tests.
    integration_test.go     4 integration tests.
```

## Init Chain

`main.go` initialises components in dependency order:

```
config.json -> database pool -> JWT verifier -> TokenManager -> HelixClient
    -> services (users, eggs, commands, quotes, colours, pools, alerts, usermerge, shoutouts)
    -> supporting services (emotes, OBS, TTS, moderator sync)
    -> WebSocket server -> IRC ChatClient -> Bot command router -> EventSubHandler
    -> HTTP handlers -> register routes on mux -> start HTTP server
    -> start periodic tasks -> wait for SIGINT/SIGTERM
```

Graceful shutdown: cancel periodic tasks -> IRC disconnect -> WebSocket cancel -> HTTP shutdown (5s) -> DB close.

## Periodic Tasks

Runs on a ticker matching `eggUpdateInterval` (default 15 minutes):

- EventSub message ID dedup cleanup
- TTS file cleanup (remove .mp3 files older than 5 minutes)
- Stream live check -> egg distribution (5/10/15/20 eggs by sub tier) + subscriber sync
- Moderator sync (always, regardless of stream status)
- Emote refresh (1hr cache built-in)

## Building

```bash
# Build binary
go build -o bot ./cmd/bot/

# Build Docker image
docker build -t maddeth/booziebot:latest .

# Run tests
go test ./... -v

# Run a single package's tests
go test ./internal/websocket/ -v

# Build check (no output)
go build ./...
go vet ./...
```

## Docker

Multi-stage build: `golang:1.24-alpine` (builder) -> `alpine:3.20` (runtime).

The final image contains:
- `/app/bot` — static binary (CGO_ENABLED=0, ~17 MB)
- `/app/config.json` — bot configuration
- `/app/tokens.*.json` — Twitch OAuth tokens (Twurple format)
- `/app/google-credentials.json` — Google Cloud TTS credentials
- `ca-certificates` + `tzdata`

Exposes ports 3000 (HTTP API) and 3001 (WebSocket).

## Environment Variables

| Variable | Required | Purpose |
|----------|----------|---------|
| `DATABASE_URL` | Yes | PostgreSQL connection string |
| `SUPABASE_JWT_SECRET` | Yes | JWT secret for Supabase auth |
| `GOOGLE_APPLICATION_CREDENTIALS` | No | Google Cloud credentials path (for TTS) |

## Key Design Decisions

- **No web framework.** stdlib `net/http` with Go 1.22+ pattern routing (`GET /api/eggs/leaderboard`, `POST /api/commands`).
- **No ORM.** Direct `pgx` queries. Services own their SQL.
- **Import cycle avoidance.** `twitch` imports `services` (EventSub needs EggService). `services` uses a `TwitchSyncClient` interface instead of importing `twitch`. `helixSyncAdapter` in main.go bridges the two.
- **Twurple token compatibility.** Token files use camelCase JSON (`accessToken`, `refreshToken`, `obtainmentTimestamp`) so they work with both the Go and JS bots.
- **Async EventSub processing.** Webhook handler responds 204 immediately, processes events in a goroutine. Message ID dedup prevents retries from being processed twice.
