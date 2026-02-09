# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -o /bot ./cmd/bot/

# Runtime stage
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# Copy the binary
COPY --from=builder /bot /app/bot

# Copy config, token, and credential files (copied from bot_js/ by rebuild.sh)
COPY config.json ./
COPY tokens.*.json ./
COPY google-credentials.json ./

EXPOSE 3000 3001

ENTRYPOINT ["/app/bot"]
