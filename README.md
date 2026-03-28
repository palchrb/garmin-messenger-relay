# garmin-messenger-relay

Forward messages from Garmin Messenger (inReach) to email, and send replies back to Garmin.

## Features

- Receives Garmin Messenger messages in real-time via SignalR
- Forwards messages to one or more email addresses (text, media, location, MapShare/LiveTrack links)
- Listens for email replies via IMAP IDLE and sends them back to Garmin
- Transcodes email attachments to Garmin-native formats (images to AVIF, audio to OGG Opus)
- Caption-based routing: include email addresses in a photo caption to route that message to specific recipients
- Runs as a single binary or Docker container

## Quick start

```bash
# Create a config file
garmin-messenger-relay init

# Edit config.yaml with your phone number and email settings
# Then authenticate with Garmin
garmin-messenger-relay login

# Start forwarding
garmin-messenger-relay run
```

## Installation

### Pre-built binaries

Download from [GitHub Releases](../../releases) for Linux, macOS, and Windows (amd64/arm64).

### Docker

```bash
docker run -d \
  --name garmin-relay \
  -v /path/to/data:/data \
  ghcr.io/palchrb/garmin-messenger-relay:latest
```

Place your `config.yaml` in the mounted `/data` directory. The session token is also stored there.

To log in interactively before starting:

```bash
docker run -it --rm \
  -v /path/to/data:/data \
  ghcr.io/palchrb/garmin-messenger-relay:latest \
  login -config /data/config.yaml
```

### Docker Compose

```yaml
services:
  garmin-relay:
    image: ghcr.io/palchrb/garmin-messenger-relay:latest
    restart: unless-stopped
    volumes:
      - ./data:/data
```

### Build from source

Requires Go 1.24+ and ffmpeg (for media transcoding).

```bash
go build -o garmin-messenger-relay ./cmd/garmin-messenger-relay
```

## Configuration

Copy `config.example.yaml` to `config.yaml` and edit it. See the example file for all options.

Key settings:

| Section | Field | Description |
|---------|-------|-------------|
| `garmin.phone` | E.164 phone number | The phone number registered with Garmin Messenger |
| `smtp.*` | SMTP settings | Outgoing mail server (Gmail, Office 365, etc.) |
| `imap.*` | IMAP settings | Optional, enables two-way messaging via email replies |
| `forwarding.default_recipients` | Email list | Where to forward incoming Garmin messages |
| `forwarding.caption_routing` | bool | Parse email addresses from media captions |

For Gmail, use an [App Password](https://myaccount.google.com/apppasswords) for both SMTP and IMAP.

## Commands

| Command | Description |
|---------|-------------|
| `init` | Write an example `config.yaml` |
| `login` | Authenticate with Garmin via SMS OTP |
| `logout` | Remove saved session |
| `run` | Start the forwarder (default) |
| `status` | Check Garmin session and SMTP connectivity |
| `test-smtp` | Send a test email |
| `version` | Print version info |

## How it works

```
Garmin inReach  ──SignalR──>  garmin-messenger-relay  ──SMTP──>  Email
                                                      <──IMAP──
                              (transcodes media)
```

1. **Garmin to email**: Messages arrive via SignalR WebSocket. Text, location, media attachments, and tracking links are formatted into an email and sent via SMTP.
2. **Email to Garmin**: Replies are detected via IMAP IDLE. Email attachments are transcoded to Garmin-native formats (AVIF/OGG Opus via ffmpeg) and sent back through the Garmin Hermes API.

## Requirements

- **ffmpeg** (runtime): Required for transcoding email attachments to Garmin formats (image to AVIF, audio to OGG Opus). Included in the Docker image.
- **SMTP server**: For sending forwarded messages.
- **IMAP server** (optional): For receiving email replies and sending them back to Garmin.

## License

MIT
