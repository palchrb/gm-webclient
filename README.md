# Community Garmin Messenger Web Client

Not affiliated with Garmin in any way. 
A lightweight web client for Garmin Messenger / InReach devices. Chat with your Garmin contacts from any browser.

<img width="1022" height="776" alt="image" src="https://github.com/user-attachments/assets/8b5947a3-17c2-48d8-9933-e81cfc011f4f" />


## Features

- Login via SMS OTP (same as the Garmin Messenger app)
- Passkey (WebAuthn) support for quick re-login with fingerprint/face/PIN
- Real-time messages via SignalR
- Push notifications via Web Push, ntfy, or both — Web Push payloads are content-free, so message text never transits the browser vendor's push relay
- Installable PWA that opens offline and shows locally cached conversations
- Image and audio message support (with voice recording)
- Reactions (emoji) on messages
- Read receipts and delivery status indicators
- Start new conversations by phone number
- Multi-user support (multiple people can log in simultaneously)
- Encrypted session persistence across Docker restarts
- Mobile responsive layout
- Dark theme

## Privacy model

The server is a thin, stateless-as-possible bridge. Everything about your conversations stays in the browser:

| Data | Where it lives | Notes |
|------|---------------|-------|
| Messages, conversations, contacts | **Browser only** (IndexedDB + localStorage) | Never written on the server, never logged. Wiped on logout. |
| Images / voice notes | **Browser HTTP cache** (24 h) | Proxied through the server per request, not cached there. |
| Garmin access/refresh tokens | Server memory; encrypted on disk in `sessions.enc` when `-data-dir` is set | See *Session persistence* below. |
| Passkey public keys | `-data-dir/passkeys/` | Public keys only; the private key stays in your device's keychain. |
| Push subscription endpoints | `-data-dir/push/` | Removed on "log out everywhere". |
| FCM device credentials | `-data-dir/fcm/` | Google device IDs so the server can receive Garmin pushes; no message content. |

Web Push notifications carry only a conversation ID. The service worker fetches the sender and preview from your server over the session cookie, so message text is never handed to Google/Apple/Mozilla push relays, and no message body is written to the server log at the default log level. ntfy is separate: it sends "New message" unless you explicitly opt in to full text (`NTFY_FULL_MESSAGE` or the per-user toggle).

**Recommendation:** always set `PHONE_WHITELIST`. Without it anyone who finds the URL can register their own Garmin account on your instance (rate limits apply, but the whitelist is the real guard).

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

Web Push notifications and passkeys require HTTPS. Use a reverse proxy like Caddy:

### Docker Compose with Caddy

```yaml
services:
  garmin-web:
    image: ghcr.io/palchrb/gm-webclient:latest
    container_name: garmin-web
    restart: unless-stopped
    volumes:
      - garmin-web-data:/data
    environment:
      - ORIGIN=https://messenger.example.com
      # Optional: ntfy push notifications (see Push Notifications section)
      # - NTFY_URL=https://ntfy.sh

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

## Push Notifications

The bell icon in the header lets you choose between two push methods. Both can be active simultaneously.

### Web Push (browser-native)

Works out of the box with HTTPS. The browser handles delivery via its own push service (Google/Mozilla/Apple). No extra setup needed — just click the bell and select "Web Push".

### ntfy (mobile app)

[ntfy](https://ntfy.sh) provides native push notifications via a lightweight app available on iOS and Android. Useful as a more reliable alternative to Web Push, especially on mobile.

**Setup:**

1. Install the ntfy app on your phone ([iOS](https://apps.apple.com/us/app/ntfy/id1625396347) / [Android](https://play.google.com/store/apps/details?id=io.heckel.ntfy))
2. Set `NTFY_URL=https://ntfy.sh` (or your self-hosted ntfy server) in docker-compose
3. In the web client, click the bell icon and select "Subscribe via ntfy"
   - **Android**: Opens the ntfy app directly and subscribes with display name "Garmin Messenger"
   - **iOS**: Shows the topic name with a copy button — paste it in the ntfy app
   - **Desktop**: Opens the ntfy web interface for the topic

**Privacy:** By default, only "New message" is sent to ntfy servers — no message content. Set `NTFY_FULL_MESSAGE=true` to include the full message body. Per-user topics are derived via HMAC-SHA256 so phone numbers are never exposed.

Tapping a ntfy notification opens the web client and navigates to the specific conversation.

### Push behavior

By default (`PUSH_ALWAYS=false`), push notifications are sent whenever no tab is visible — a backgrounded or closed tab gets push, a tab you are looking at gets the message in-app only. Set `PUSH_ALWAYS=true` to also get a system notification while the app is in the foreground.

Logging out a single browser session removes Web Push for that browser only. ntfy and other browser sessions continue receiving notifications. "Log out everywhere" disconnects from Garmin entirely and stops all push.

## Passkeys (WebAuthn)

Set the `ORIGIN` environment variable to your public HTTPS URL to enable passkey support:

```yaml
environment:
  - ORIGIN=https://messenger.example.com
```

After logging in with OTP, you'll be prompted to register a passkey. On subsequent logins, you can authenticate with fingerprint, face, or device PIN instead of waiting for an SMS code.

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

# With passkeys
./garmin-web -addr :8080 -data-dir ./data -origin https://messenger.example.com
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
| `-data-dir` | (empty) | Directory for persistent data. Enables FCM push, Web Push, ntfy, passkeys and session persistence. Leave empty for a fully memory-only server (no push, re-login after every restart). |
| `-log-level` | `info` | Log level: `debug`, `info`, `warn`, `error`. `debug` dumps raw API bodies (tokens, OTP codes) — never use it in production. |
| `-phone-whitelist` | (empty) | Comma-separated list of phone numbers allowed to log in (e.g. `+4712345678,+4787654321`). Also `PHONE_WHITELIST`. Empty allows all — **set this**. |
| `-session-days` | `90` | Days of *inactivity* before a login expires. The cookie is renewed on every visit, so an active user never has to log in again. Also `SESSION_DAYS`. |
| `-origin` | (empty) | Public origin (e.g. `https://messenger.example.com`) — required for passkeys, also used as the ntfy click URL. Also `ORIGIN`. |

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `PHONE_WHITELIST` | (empty) | Comma-separated phone numbers allowed to log in |
| `SESSION_DAYS` | `90` | Days of inactivity before a login expires (renewed on use) |
| `SESSION_KEY` | (auto) | Key for the encrypted session file. If unset and `-data-dir` is given, a random key is generated and stored in `-data-dir/session_key`. See *Session persistence*. |
| `ORIGIN` | (empty) | Public HTTPS URL. Enables passkeys (WebAuthn) and sets the click URL for ntfy notifications. |
| `PUSH_ALWAYS` | `false` | Push only when no tab is *visible* (a backgrounded tab still gets push). Set `true` to also push while you are looking at the app. |
| `NTFY_URL` | (empty) | ntfy server URL to enable ntfy push forwarding (e.g. `https://ntfy.sh`) |
| `NTFY_FULL_MESSAGE` | `false` | Include full message body in ntfy notifications. When false, sends "New message" only. |

### Session persistence

With `-data-dir` set, Garmin access/refresh tokens are written to `sessions.enc` (AES-256-GCM) so logins, push and FCM survive restarts. The file is re-written every time Garmin rotates a token, and the Garmin account is persisted even when no browser is logged in, so push keeps working across restarts.

Be clear about what the encryption buys you: if `SESSION_KEY` is **auto-generated**, the key sits next to the file in the same volume, and the encryption only protects against casual reads of a backup or copied volume — not against someone with access to the whole directory. Provide `SESSION_KEY` yourself (e.g. via a secret manager) if that distinction matters to you. Without `-data-dir` nothing touches disk at all.

## Architecture

```
Browser                          Go Server                      Garmin Cloud
  |                                |                               |
  |-- Login (OTP / Passkey) ----->|-- RequestOTP / ConfirmOTP --->|
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
  |                                |-- ntfy POST ---------------->| ntfy.sh
```

### Data at rest

- **Message content**: Never stored or logged on the server. Proxied to the browser and cached there in IndexedDB; cleared on logout.
- **Sessions / Garmin auth tokens**: AES-256-GCM encrypted in `sessions.enc` (see *Session persistence*) — never in plaintext on disk
- **The Garmin connection outlives the browser session.** Logging out of a browser keeps the server-side account (and push) alive so a passkey re-login is instant; "log out everywhere" tears everything down and deregisters the app instance with Garmin.
- **FCM credentials**: Google device IDs for push delivery, stored in data dir (not sensitive user data)
- **Push subscriptions**: browser Web Push endpoints, per phone number; removed on "log out everywhere"
- **Passkeys**: WebAuthn public keys stored per phone number (private keys live in your device's secure enclave)
- **VAPID keys**: Auto-generated Web Push signing keys
- **ntfy HMAC key**: Auto-generated key for deriving per-user ntfy topics

## Data Directory Layout

When `-data-dir` is set:

```
data/
  sessions.enc                  # Encrypted Garmin tokens + browser session IDs (AES-256-GCM)
  session_key                   # Auto-generated encryption key (if SESSION_KEY not set)
  vapid_keys.json               # Web Push VAPID key pair (auto-generated)
  ntfy_hmac_key                 # ntfy topic derivation key (auto-generated, if NTFY_URL set)
  ntfy/<phone>.json             # Per-user ntfy preferences
  passkeys/<phone>.json         # WebAuthn public keys per user (if ORIGIN set)
  fcm/<phone>/                  # Per-user FCM credentials
    fcm_credentials.json
  push/<phone>/                 # Per-user browser push subscriptions
    subscriptions.json
```

All files are created with `0600` permissions (owner read/write only).

## Multi-Arch Docker Images

Pre-built images are available for `linux/amd64` and `linux/arm64`:

```bash
docker pull ghcr.io/palchrb/gm-webclient:latest
```

The image is ~15 MB (Alpine-based, single static binary).
