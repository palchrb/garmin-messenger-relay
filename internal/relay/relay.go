package relay

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	gm "github.com/palchrb/garmin-messenger-relay/internal/hermes"
)

var emailRegexp = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// Relay listens for incoming Garmin messages, forwards them via email,
// and routes email replies back to Garmin Messenger.
type Relay struct {
	cfg      *Config
	auth     *gm.HermesAuth
	api      *gm.HermesAPI
	sr       *gm.HermesSignalR
	mailer   *Mailer
	store    *MsgStore
	imap     *IMAPClient
	log      *slog.Logger
}

// New creates a new Relay. Call Run() to start it.
func New(cfg *Config, log *slog.Logger) (*Relay, error) {
	sessDir := cfg.Garmin.SessionDir
	if sessDir == "" {
		sessDir = "./sessions"
	}

	auth := gm.NewHermesAuth(
		gm.WithSessionDir(sessDir),
		gm.WithLogger(log),
	)

	api := gm.NewHermesAPI(auth, gm.WithAPILogger(log))
	sr := gm.NewHermesSignalR(auth, gm.WithSignalRLogger(log))
	mailer := NewMailer(cfg.SMTP)

	store, err := NewMsgStore(sessDir, cfg.IMAP.ReplyWindowDays)
	if err != nil {
		return nil, fmt.Errorf("loading message store: %w", err)
	}

	r := &Relay{
		cfg:    cfg,
		auth:   auth,
		api:    api,
		sr:     sr,
		mailer: mailer,
		store:  store,
		log:    log,
	}

	if cfg.IMAPEnabled() {
		r.imap = NewIMAPClient(cfg.IMAP, log)
	}

	return r, nil
}

// Resume loads the saved Garmin session from disk.
func (r *Relay) Resume(ctx context.Context) error {
	return r.auth.Resume(ctx)
}

// Run starts the SignalR listener (and IMAP poller if configured) and
// blocks until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) error {
	// Validate session with a lightweight API call.
	if _, err := r.api.GetConversations(ctx, gm.WithLimit(1)); err != nil {
		return fmt.Errorf("connecting to Garmin Messenger: %w\n\nRun 'garmin-messenger-relay login' to authenticate", err)
	}

	r.log.Info("Connected to Garmin Messenger", "phone", r.cfg.Garmin.Phone)

	r.sr.OnMessage(func(msg gm.MessageModel) {
		r.handleMessage(msg)
	})

	r.sr.OnOpen(func() {
		r.log.Info("SignalR connected")
	})

	r.sr.OnClose(func() {
		r.log.Warn("SignalR disconnected — will reconnect automatically")
	})

	r.sr.OnError(func(err error) {
		r.log.Error("SignalR error", "error", err)
	})

	r.log.Info("Listening for Garmin messages", "default_recipients", r.cfg.Forwarding.DefaultRecipients)

	// Start IMAP listener for inbound replies in a separate goroutine.
	if r.imap != nil {
		go r.imap.Start(ctx)
		go r.processReplies(ctx)
		r.log.Info("IMAP reply listener started", "host", r.cfg.IMAP.Host)
	}

	if err := r.sr.Start(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("SignalR: %w", err)
	}
	return nil
}

// Stop shuts down the SignalR connection.
func (r *Relay) Stop() {
	r.sr.Stop()
	r.api.Close()
}

// processReplies drains the IMAP reply channel and handles each reply.
func (r *Relay) processReplies(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case reply := <-r.imap.Replies:
			r.handleReply(ctx, reply)
		}
	}
}

// handleReply sends an email reply back to the originating Garmin conversation.
func (r *Relay) handleReply(ctx context.Context, reply InboundReply) {
	sent, ok := r.store.Lookup(reply.InReplyTo)
	if !ok {
		r.log.Debug("reply to unknown/expired message, ignoring", "in_reply_to", reply.InReplyTo)
		return
	}

	// Verify the reply came from the expected recipient.
	if !strings.EqualFold(reply.From, sent.RecipientEmail) {
		r.log.Warn("reply sender mismatch — ignoring",
			"from", reply.From,
			"expected", sent.RecipientEmail)
		return
	}

	r.log.Info("Received email reply, forwarding to Garmin",
		"from", reply.From,
		"conversation_id", sent.ConversationID,
		"attachments", len(reply.Attachments),
	)

	to := []string{sent.SenderPhone}

	// Send text body if present.
	if strings.TrimSpace(reply.TextBody) != "" {
		_, err := r.api.SendMessage(ctx, to, reply.TextBody)
		if err != nil {
			r.log.Error("failed to send reply text to Garmin", "err", err)
		}
	}

	// Send each attachment (transcode to Garmin-native formats first).
	for _, att := range reply.Attachments {
		if err := r.forwardAttachmentToGarmin(ctx, to, att); err != nil {
			r.log.Error("failed to forward attachment to Garmin",
				"filename", att.Filename, "err", err)
		}
	}
}

// forwardAttachmentToGarmin transcodes an inbound email attachment to a
// Garmin-compatible format and sends it via the Hermes API.
func (r *Relay) forwardAttachmentToGarmin(ctx context.Context, to []string, att InboundAttachment) error {
	ct := strings.ToLower(att.ContentType)
	switch {
	case strings.HasPrefix(ct, "image/"):
		avif, err := transcodeImageToAVIF(ctx, att.Data)
		if err != nil {
			return fmt.Errorf("transcoding image to AVIF: %w", err)
		}
		_, err = r.api.SendMediaMessage(ctx, to, "", avif, gm.MediaTypeImageAvif)
		return err

	case strings.HasPrefix(ct, "audio/"):
		ogg, err := transcodeAudioToOGG(ctx, att.Data)
		if err != nil {
			return fmt.Errorf("transcoding audio to OGG: %w", err)
		}
		durationMs, _ := getAudioDurationMs(ctx, ogg)
		var opts []gm.SendMessageOption
		if durationMs > 0 {
			ms := int(durationMs)
			opts = append(opts, gm.WithMediaMetadata(gm.MediaMetadata{DurationMs: &ms}))
		}
		_, err = r.api.SendMediaMessage(ctx, to, "", ogg, gm.MediaTypeAudioOgg, opts...)
		return err

	default:
		return fmt.Errorf("unsupported attachment type %q", att.ContentType)
	}
}

// handleMessage processes an incoming Garmin message and forwards it by email.
func (r *Relay) handleMessage(msg gm.MessageModel) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	r.log.Info("Received Garmin message",
		"msg_id", msg.MessageID,
		"conversation_id", msg.ConversationID,
		"from", derefStr(msg.From),
	)

	// Skip reaction messages — not meaningful as email.
	if msg.MessageType.IsReaction() {
		r.log.Debug("Skipping reaction message")
		return
	}

	body := r.buildEmailBody(msg)
	caption := derefStr(msg.MessageBody)

	// Determine recipients.
	recipients := r.resolveRecipients(caption)
	if len(recipients) == 0 {
		r.log.Warn("No recipients configured — dropping message")
		return
	}

	// Generate a unique Message-ID so replies can be matched back.
	msgID := fmt.Sprintf("<gmr-%s-%s-%08x@garmin-messenger-relay.local>",
		msg.ConversationID, msg.MessageID, rand.Uint32())

	emailMsg := Message{
		To:        recipients,
		Subject:   r.buildSubject(msg),
		Body:      body,
		MessageID: msgID,
	}

	// Attach media if present.
	if msg.MediaID != nil && r.cfg.Forwarding.ForwardMedia {
		att, err := r.downloadMedia(ctx, msg)
		if err != nil {
			r.log.Error("Failed to download media", "error", err)
			emailMsg.Body += "\n\n[Media attachment could not be downloaded: " + err.Error() + "]"
		} else {
			emailMsg.Attachments = append(emailMsg.Attachments, *att)
		}
	}

	if err := r.mailer.Send(emailMsg); err != nil {
		r.log.Error("Failed to send email", "error", err, "recipients", recipients)
		return
	}

	r.log.Info("Email forwarded", "recipients", recipients, "subject", emailMsg.Subject)

	// Store mapping so inbound replies can be routed back.
	if r.cfg.IMAPEnabled() {
		for _, recipient := range recipients {
			_ = r.store.Add(SentMessage{
				MessageID:      msgID,
				ConversationID: msg.ConversationID,
				SenderPhone:    derefStr(msg.From),
				RecipientEmail: recipient,
				SentAt:         time.Now(),
			})
		}
	}

	// Mark message as delivered.
	r.sr.MarkAsDelivered(msg.ConversationID, msg.MessageID)
}

// buildSubject constructs the email subject line.
func (r *Relay) buildSubject(msg gm.MessageModel) string {
	from := derefStr(msg.From)
	if from == "" {
		from = "unknown"
	}

	ts := time.Now()
	if msg.SentAt != nil {
		ts = *msg.SentAt
	}

	deviceTag := ""
	if msg.FromDeviceType != nil && *msg.FromDeviceType == gm.DeviceTypeInReach {
		deviceTag = " [InReach]"
	}

	return fmt.Sprintf("Garmin Message from %s%s — %s",
		from, deviceTag, ts.Format("2006-01-02 15:04 MST"))
}

// buildEmailBody constructs the plain-text email body from a message.
func (r *Relay) buildEmailBody(msg gm.MessageModel) string {
	var sb strings.Builder

	from := derefStr(msg.From)
	ts := time.Now()
	if msg.SentAt != nil {
		ts = *msg.SentAt
	}

	sb.WriteString("═══════════════════════════════════════\n")
	sb.WriteString("  GARMIN MESSENGER\n")
	sb.WriteString("═══════════════════════════════════════\n\n")

	sb.WriteString(fmt.Sprintf("From:    %s\n", from))
	sb.WriteString(fmt.Sprintf("Time:    %s\n", ts.Format("2006-01-02 15:04:05 MST")))

	if msg.FromDeviceType != nil {
		sb.WriteString(fmt.Sprintf("Device:  %s\n", *msg.FromDeviceType))
	}

	sb.WriteString("\n")

	// Text body
	if body := derefStr(msg.MessageBody); body != "" && r.cfg.Forwarding.ForwardText {
		sb.WriteString("Message:\n")
		sb.WriteString("  " + strings.ReplaceAll(body, "\n", "\n  ") + "\n\n")
	}

	// Location
	if msg.UserLocation != nil && r.cfg.Forwarding.ForwardLocation {
		loc := msg.UserLocation
		lat := derefFloat64(loc.LatitudeDegrees)
		lon := derefFloat64(loc.LongitudeDegrees)

		sb.WriteString("Location:\n")
		sb.WriteString(fmt.Sprintf("  Coordinates: %.6f, %.6f\n", lat, lon))
		sb.WriteString(fmt.Sprintf("  Maps: https://maps.google.com/?q=%.6f,%.6f\n", lat, lon))

		if alt := derefFloat64(loc.ElevationMeters); alt != 0 {
			sb.WriteString(fmt.Sprintf("  Elevation:   %.0f m\n", alt))
		}
		if spd := derefFloat64(loc.GroundVelocityMetersPerSecond); spd != 0 {
			sb.WriteString(fmt.Sprintf("  Speed:       %.1f km/h\n", spd*3.6))
		}
		sb.WriteString("\n")
	}

	// MapShare / LiveTrack links
	if url := derefStr(msg.MapShareUrl); url != "" {
		sb.WriteString(fmt.Sprintf("MapShare: %s\n", url))
		if pw := derefStr(msg.MapSharePassword); pw != "" {
			sb.WriteString(fmt.Sprintf("Password: %s\n", pw))
		}
		sb.WriteString("\n")
	}
	if url := derefStr(msg.LiveTrackUrl); url != "" {
		sb.WriteString(fmt.Sprintf("LiveTrack: %s\n\n", url))
	}

	// Audio transcription
	if t := derefStr(msg.Transcription); t != "" {
		sb.WriteString("Transcription:\n")
		sb.WriteString("  " + t + "\n\n")
	}

	sb.WriteString("───────────────────────────────────────\n")
	sb.WriteString(fmt.Sprintf("Message ID:      %s\n", msg.MessageID))
	sb.WriteString(fmt.Sprintf("Conversation ID: %s\n", msg.ConversationID))
	sb.WriteString("Forwarded by garmin-messenger-relay\n")

	return sb.String()
}

// resolveRecipients returns the list of email recipients for a message.
// If caption_routing is enabled, email addresses are parsed from the caption.
func (r *Relay) resolveRecipients(caption string) []string {
	captionEmails := r.cfg.Forwarding.CaptionRouting && caption != ""
	var parsed []string
	if captionEmails {
		parsed = emailRegexp.FindAllString(caption, -1)
	}

	if len(parsed) > 0 && r.cfg.Forwarding.CaptionRoutingReplacesDefault {
		return unique(parsed)
	}

	all := append([]string{}, r.cfg.Forwarding.DefaultRecipients...)
	all = append(all, parsed...)
	return unique(all)
}

// downloadMedia fetches the media attachment from Garmin and returns an Attachment.
// If ffmpeg is available, AVIF images are converted to JPEG and OGG audio to MP3
// for better email client compatibility. Falls back to the original format otherwise.
func (r *Relay) downloadMedia(ctx context.Context, msg gm.MessageModel) (*Attachment, error) {
	if msg.MediaID == nil || msg.MediaType == nil {
		return nil, fmt.Errorf("message has no media")
	}

	// Resolve message UUID required by the download API.
	msgUUID, err := r.resolveMediaUUID(ctx, msg)
	if err != nil {
		return nil, err
	}

	data, err := r.api.DownloadMedia(ctx, msgUUID, *msg.MediaID, msg.MessageID, msg.ConversationID, *msg.MediaType)
	if err != nil {
		return nil, fmt.Errorf("downloading media: %w", err)
	}

	switch *msg.MediaType {
	case gm.MediaTypeImageAvif:
		if jpeg, err := transcodeAVIFtoJPEG(ctx, data); err == nil {
			return &Attachment{
				Filename:    "image.jpg",
				ContentType: "image/jpeg",
				Data:        jpeg,
			}, nil
		}
		// ffmpeg not available — attach as-is
		return &Attachment{
			Filename:    "image.avif",
			ContentType: "image/avif",
			Data:        data,
		}, nil

	case gm.MediaTypeAudioOgg:
		if mp3, err := transcodeOGGtoMP3(ctx, data); err == nil {
			return &Attachment{
				Filename:    "audio.mp3",
				ContentType: "audio/mpeg",
				Data:        mp3,
			}, nil
		}
		// ffmpeg not available — attach as-is
		return &Attachment{
			Filename:    "audio.ogg",
			ContentType: "audio/ogg",
			Data:        data,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported media type: %s", *msg.MediaType)
	}
}

// resolveMediaUUID finds the UUID required by the Garmin media download API.
func (r *Relay) resolveMediaUUID(ctx context.Context, msg gm.MessageModel) (uuid.UUID, error) {
	if msg.UUID != nil {
		return *msg.UUID, nil
	}
	detail, err := r.api.GetConversationDetail(ctx, msg.ConversationID, gm.WithDetailLimit(100))
	if err != nil {
		return uuid.Nil, fmt.Errorf("lookup failed: %w", err)
	}
	for _, m := range detail.Messages {
		if m.MessageID == msg.MessageID && m.UUID != nil {
			return *m.UUID, nil
		}
	}
	// Fall back to MessageID as UUID (best-effort).
	return msg.MessageID, nil
}

// transcodeAVIFtoJPEG converts AVIF image data to JPEG using ffmpeg.
// Returns an error if ffmpeg is not available.
func transcodeAVIFtoJPEG(ctx context.Context, data []byte) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found")
	}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "avif", "-i", "pipe:0",
		"-f", "mjpeg", "-q:v", "3",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(data)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, errBuf.String())
	}
	return out.Bytes(), nil
}

// transcodeOGGtoMP3 converts OGG audio data to MP3 using ffmpeg.
// Returns an error if ffmpeg is not available.
func transcodeOGGtoMP3(ctx context.Context, data []byte) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found")
	}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "ogg", "-i", "pipe:0",
		"-f", "mp3", "-b:a", "128k",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(data)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, errBuf.String())
	}
	return out.Bytes(), nil
}

// transcodeImageToAVIF converts image data (JPEG, PNG, etc.) to AVIF using ffmpeg.
func transcodeImageToAVIF(ctx context.Context, data []byte) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found")
	}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", "pipe:0",
		"-f", "avif", "-c:v", "libaom-av1", "-crf", "30", "-b:v", "0",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(data)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg avif: %w: %s", err, errBuf.String())
	}
	return out.Bytes(), nil
}

// transcodeAudioToOGG converts audio data (MP3, M4A, WAV, etc.) to OGG Opus at 48kHz.
func transcodeAudioToOGG(ctx context.Context, data []byte) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found")
	}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", "pipe:0",
		"-f", "ogg", "-c:a", "libopus", "-ar", "48000", "-b:a", "32k",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(data)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg ogg: %w: %s", err, errBuf.String())
	}
	return out.Bytes(), nil
}

// getAudioDurationMs returns the duration of an OGG clip in milliseconds.
// Returns 0 if ffprobe is unavailable or the duration cannot be determined.
func getAudioDurationMs(ctx context.Context, data []byte) (int, error) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		return 0, nil
	}
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		"-f", "ogg", "pipe:0",
	)
	cmd.Stdin = bytes.NewReader(data)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0, nil
	}
	var secs float64
	if _, err := fmt.Sscanf(strings.TrimSpace(out.String()), "%f", &secs); err != nil {
		return 0, nil
	}
	return int(secs * 1000), nil
}

// unique returns a deduplicated copy of the slice.
func unique(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefFloat64(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}
