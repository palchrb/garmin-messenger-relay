# garmin-messenger-relay

Forward messages from Garmin Messenger (inReach) to email, and send replies back to Garmin.

## Features

- Receives Garmin Messenger messages in real-time via SignalR WebSocket
- Forwards messages to one or more email addresses with location, map links, and media
- Listens for email replies via IMAP (IDLE or polling) and sends them back to Garmin
- Transcodes email attachments to Garmin-native formats (images to AVIF, audio to OGG Opus via ffmpeg)
- Caption-based routing: include email addresses in a media caption to route to specific recipients
- Reply validation: only legitimate replies to messages sent by the relay are forwarded back
- Runs as a single binary or Docker container (multi-arch: amd64 + arm64)

## Quick start

### 1. Install

Download a pre-built binary from [GitHub Releases](../../releases) for your platform, or use Docker (see below).

### 2. Create a config file

```bash
garmin-messenger-relay init
```

This writes an example `config.yaml` to the current directory. Edit it with your phone number, SMTP, and optionally IMAP settings. See [Configuration](#configuration) for details.

### 3. Authenticate with Garmin

```bash
garmin-messenger-relay login
```

This sends a 6-digit SMS code to the phone number in your config. Enter the code when prompted. The session is saved to disk and refreshes automatically — you only need to do this once.

### 4. Start the relay

```bash
garmin-messenger-relay run
```

The relay connects to Garmin Messenger via SignalR, listens for incoming messages, and forwards them to your configured email recipients. If IMAP is configured, it also listens for email replies and sends them back to Garmin.

## Docker

The Docker image includes ffmpeg and handles bind-mount permissions automatically. All data (config, session tokens) is stored in a single `/data` volume. The container starts as root to fix volume ownership, then drops to an unprivileged `relay` user before running the application.

### Setup

Create a directory for your data and add your config (see [Configuration](#configuration) for all options):

```bash
mkdir -p garmin-relay-data

cat > garmin-relay-data/config.yaml << 'EOF'
garmin:
  phone: "+4712345678"          # Required. E.164 phone number for Garmin Messenger.
  session_dir: "./sessions"     # Where session tokens are stored.

smtp:
  host: "smtp.gmail.com"       # Required. SMTP server hostname.
  port: 587                    # SMTP port (587 for STARTTLS, 465 for SSL, 25 for plain).
  username: "you@gmail.com"    # SMTP username.
  password: "your-app-password" # SMTP password. For Gmail, use an App Password.
  from: "Garmin Relay <you@gmail.com>" # Required. Sender address shown in forwarded emails.
  tls: "starttls"              # Connection security: "starttls", "ssl", or "none".

# IMAP is optional. Enable it for two-way messaging (receive email replies
# and send them back to Garmin). Without IMAP, the relay is one-way only.
imap:
  host: "imap.gmail.com"       # IMAP server hostname. Leave empty to disable.
  port: 993                    # IMAP port (993 for TLS).
  username: "you@gmail.com"
  password: "your-app-password"
  reply_window_days: 7         # Ignore replies to messages older than this.
  max_attachment_mb: 5         # Maximum size per email attachment in MB.

forwarding:
  default_recipients:          # Required. Emails that receive all Garmin messages.
    - "you@example.com"
  caption_routing: true        # Parse email addresses from media captions.
  caption_routing_replaces_default: false  # If true, caption addresses replace defaults.
  forward_text: true           # Forward text messages.
  forward_media: true          # Forward media attachments (images, audio).
  forward_location: true       # Forward GPS location and map links.

log:
  level: "info"                # Log level: trace, debug, info, warn, error.
  pretty: true                 # Human-readable logs (false for JSON).

# Optional: email alerts for operational issues (Garmin disconnect, IMAP auth
# failure, session expiry). Leave empty or omit to disable.
# alerts:
#   email: "admin@example.com"
#   cooldown_minutes: 30       # Minimum minutes between repeated alerts.
EOF
```

### Login (one-time)

You must authenticate interactively before starting the relay:

```bash
docker run -it --rm \
  -v ./garmin-relay-data:/data \
  ghcr.io/palchrb/garmin-messenger-relay:latest \
  login
```

This will:
1. Send an SMS with a 6-digit code to the phone number in your config
2. Prompt you to enter the code
3. Save the session token to `/data/sessions/hermes_credentials.json`

You only need to do this once. The session refreshes automatically.

### Run

```bash
docker run -d \
  --name garmin-relay \
  --restart unless-stopped \
  -v ./garmin-relay-data:/data \
  ghcr.io/palchrb/garmin-messenger-relay:latest
```

### Docker Compose

```yaml
services:
  garmin-relay:
    image: ghcr.io/palchrb/garmin-messenger-relay:latest
    restart: unless-stopped
    volumes:
      - ./garmin-relay-data:/data
```

```bash
# First-time login
docker compose run --rm garmin-relay login

# Start the relay
docker compose up -d

# Check status
docker compose run --rm garmin-relay status

# Send a test email
docker compose run --rm garmin-relay test-smtp

# View logs
docker compose logs -f garmin-relay
```

## Configuration

Copy `config.example.yaml` to `config.yaml` and edit it. The full config with all options and defaults is shown in the [Docker setup](#setup) above.

### Gmail setup

1. Enable 2-Step Verification on your Google account
2. Go to [App Passwords](https://myaccount.google.com/apppasswords)
3. Generate an app password for "Mail"
4. Use this password for both `smtp.password` and `imap.password`

### Caption routing

When `caption_routing` is enabled, the relay scans the entire media caption for email addresses and forwards the message to **all** addresses found, in addition to your default recipients.

Example: Send a photo from your inReach with caption `"admin@example.com, boss@work.com Base camp reached"` — the photo and caption will be forwarded to both `admin@example.com` and `boss@work.com` in addition to your default recipients. The addresses can appear anywhere in the caption and don't need any special separator.

Set `caption_routing_replaces_default: true` if you want caption addresses to **replace** the default recipients instead of being added to them.

## Commands

| Command | Description |
|---------|-------------|
| `init` | Write an example `config.yaml` to the current directory |
| `login` | Authenticate with Garmin Messenger via SMS OTP (one-time) |
| `logout` | Remove saved Garmin session |
| `run` | Start the relay (default if no command given) |
| `status` | Check Garmin session validity and SMTP connectivity |
| `test-smtp` | Send a test email to verify SMTP settings |
| `version` | Print version, commit hash, and build time |

All commands accept `-config <path>` to specify a config file (default: `config.yaml`).

## How it works

```
Garmin inReach  ──SignalR──>  garmin-messenger-relay  ──SMTP──>  Email
                                                      <──IMAP──
```

### Garmin to email

1. Messages arrive in real-time via SignalR WebSocket from Garmin Hermes
2. The relay formats each message as a plain-text email with sender, caption, GPS coordinates, OpenStreetMap link, altitude, and attachment info
3. Media attachments (AVIF images, OGG audio) are included as-is
4. The email is sent via SMTP to configured recipients

### Email to Garmin

1. The IMAP client listens for new emails using IDLE (push) or polling (30s interval, if IDLE is unavailable)
2. Only replies to emails sent by the relay are processed (validated via Message-ID, sender address, and time window)
3. The reply text is sent as a caption on the first attachment
4. Image attachments are transcoded to AVIF, audio to OGG Opus (via ffmpeg)
5. Media is uploaded to Garmin via presigned S3 URLs

### Reply validation

Replies are validated with multiple checks to prevent spam or accidental forwarding:

- The email must be a reply (`In-Reply-To` header) to a message sent by this relay
- The `In-Reply-To` Message-ID must exist in the relay's sent message store
- The sender (`From`) must match the original recipient
- The original message must be within the reply window (default: 7 days)

## Running as a system service

### Linux (systemd)

A systemd service file is included in the repository.

```bash
# Create a dedicated user
sudo useradd -r -s /sbin/nologin garmin-relay

# Set up the install directory
sudo mkdir -p /opt/garmin-messenger-relay
sudo cp garmin-messenger-relay /opt/garmin-messenger-relay/
sudo cp config.yaml /opt/garmin-messenger-relay/

# Authenticate (run as the service user)
sudo -u garmin-relay /opt/garmin-messenger-relay/garmin-messenger-relay \
  login -config /opt/garmin-messenger-relay/config.yaml

# Install and start the service
sudo cp garmin-messenger-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now garmin-messenger-relay

# Check status and logs
sudo systemctl status garmin-messenger-relay
sudo journalctl -u garmin-messenger-relay -f
```

### Windows (Task Scheduler)

The Windows binary works as-is — just download the `.exe` from [Releases](../../releases). To run it automatically at startup:

1. Open **Task Scheduler** (`taskschd.msc`)
2. Click **Create Task** (not "Create Basic Task")
3. **General** tab:
   - Name: `Garmin Messenger Relay`
   - Check **Run whether user is logged on or not**
   - Check **Run with highest privileges**
4. **Triggers** tab → New:
   - Begin the task: **At startup**
5. **Actions** tab → New:
   - Action: **Start a program**
   - Program: `C:\garmin-relay\garmin-messenger-relay.exe`
   - Arguments: `run -config C:\garmin-relay\config.yaml`
   - Start in: `C:\garmin-relay`
6. **Settings** tab:
   - Check **If the task fails, restart every** → `1 minute`, up to `3` times
   - Uncheck **Stop the task if it runs longer than**
7. Click **OK** and enter your Windows password

Set up the relay directory and config before creating the task:

```cmd
mkdir C:\garmin-relay
cd C:\garmin-relay

:: Download the .exe from GitHub Releases and place it here, then:
garmin-messenger-relay.exe init
:: Edit config.yaml with your settings (Notepad, VS Code, etc.)

:: Authenticate (one-time)
garmin-messenger-relay.exe login
```

> **Note:** ffmpeg must be installed and on your PATH for media transcoding.
> Download from [ffmpeg.org](https://ffmpeg.org/download.html) and add the `bin` folder to your system PATH.

## Build from source

Requires Go 1.24+ and ffmpeg (runtime dependency for media transcoding).

```bash
git clone https://github.com/palchrb/garmin-messenger-relay.git
cd garmin-messenger-relay
go build -o garmin-messenger-relay ./cmd/garmin-messenger-relay
```

### Regenerate protobuf (optional)

If you modify `.proto` files, regenerate the Go code:

```bash
# Install dependencies
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest

# Regenerate
cd internal/hermes && bash proto/generate.sh
```

Requires `protoc` (Protocol Buffers compiler) to be installed.

## Requirements

- **ffmpeg** (runtime): Required for transcoding email attachments to Garmin-compatible formats. Included in the Docker image.
- **SMTP server**: For sending forwarded messages (Gmail, Office 365, Fastmail, etc.)
- **IMAP server** (optional): For receiving email replies. Supports IDLE (push) with automatic fallback to polling.

## License

MIT
