# Garmin Messenger Web Client

A lightweight web client for Garmin Messenger / InReach devices. Chat with your Garmin contacts from any browser.

## Features

- Login via SMS OTP (same as the Garmin Messenger app)
- Real-time messages via SignalR
- Push notifications when the browser tab is closed (Web Push + FCM)
- Image and audio message support
- Read receipts and delivery status indicators
- Start new conversations by phone number
- Multi-user support (multiple people can log in simultaneously)
- Mobile responsive layout
- Dark theme

## Quick Start with Docker Compose

### 1. Create a `docker-compose.yml`

```yaml
services:
  garmin-web:
    image: ghcr.io/palchrb/gm-webclient:latest
    container_name: garmin-web
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - garmin-web-data:/data

volumes:
  garmin-web-data:
```

### 2. Start it

```bash
docker compose up -d
```

### 3. Open in browser

Go to `http://localhost:8080`, enter your phone number, confirm the SMS code, and start chatting.

## Production Setup with HTTPS

Web Push notifications require HTTPS. Use a reverse proxy like Caddy:

### Docker Compose with Caddy

```yaml
services:
  garmin-web:
    image: ghcr.io/palchrb/gm-webclient:latest
    container_name: garmin-web
    restart: unless-stopped
    volumes:
      - garmin-web-data:/data
    # No ports exposed — Caddy handles external traffic

  caddy:
    image: caddy:2-alpine
    container_name: caddy
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile
      - caddy-data:/data

volumes:
  garmin-web-data:
  caddy-data:
```

### Caddyfile

```
messenger.example.com {
    reverse_proxy garmin-web:8080
}
```

Replace `messenger.example.com` with your domain. Caddy automatically provisions TLS certificates via Let's Encrypt.

### Start

```bash
docker compose up -d
```

## Build from Source

### Prerequisites

- Go 1.24+

### Build

```bash
go build -o garmin-web ./cmd/garmin-web
```

### Run

```bash
# Basic (no push notifications)
./garmin-web -addr :8080

# With push notifications and FCM
./garmin-web -addr :8080 -data-dir ./data
```

### Build Docker Image Locally

```bash
docker build -t garmin-web .
docker run -p 8080:8080 -v garmin-web-data:/data garmin-web
```

## Command Line Options

| Flag | Default | Description |
|------|---------|-------------|
| `-addr` | `:8080` | HTTP listen address |
| `-data-dir` | (empty) | Directory for persistent data. Enables FCM push and Web Push notifications. Stores VAPID keys, FCM device credentials, and push subscriptions. |
| `-log-level` | `info` | Log level: `debug`, `info`, `warn`, `error` |

## Architecture

```
Browser                          Go Server                      Garmin Cloud
  |                                |                               |
  |-- Login (SMS OTP) ----------->|-- RequestOTP / ConfirmOTP --->|
  |<-- Session cookie ------------|                               |
  |                                |                               |
  |-- GET /api/conversations ---->|-- HermesAPI.GetConversations ->|
  |-- GET /api/conversations/X -->|-- HermesAPI.GetConversationDetail ->|
  |-- POST /api/messages/send --->|-- HermesAPI.SendMessage ------>|
  |                                |                               |
  |<-- SSE (real-time) -----------|<-- SignalR WebSocket ----------|
  |                                |<-- FCM / MCS push ------------|
  |                                |                               |
  |  (tab closed)                  |                               |
  |<-- Web Push notification -----|  (message arrives via FCM)    |
```

- **Garmin auth tokens** are held in server memory while the session is active. They are **never written to disk**.
- **FCM credentials** (Google device IDs for push delivery) are persisted in `-data-dir` to avoid Google's registration rate limits.
- **VAPID keys** (for Web Push) are auto-generated on first start and persisted in `-data-dir`.
- **Push subscriptions** (browser endpoints) are persisted per phone number in `-data-dir`.

## Data Directory Layout

When `-data-dir` is set:

```
data/
  vapid_keys.json           # Web Push VAPID key pair (auto-generated)
  fcm/<phone>/              # Per-user FCM credentials
    fcm_credentials.json
  push/<phone>/             # Per-user browser push subscriptions
    subscriptions.json
```

## Multi-Arch Docker Images

Pre-built images are available for `linux/amd64` and `linux/arm64`:

```bash
docker pull ghcr.io/palchrb/gm-webclient:latest
```

The image is ~15 MB (Alpine-based, single static binary).
