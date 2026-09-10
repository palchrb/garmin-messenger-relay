package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	gm "github.com/palchrb/garmin-messenger-relay/internal/hermes"
)

var emailRegexp = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

const (
	// inboxSize bounds the number of Garmin messages waiting for the worker.
	inboxSize = 256
	// catchupMargin is how far before the last successful catch-up the next
	// pass looks. Satellite messages can be received long after sentAt, so
	// the window is generous; the processed set prevents duplicates.
	catchupMargin = 1 * time.Hour
	// catchupPageLimit is the number of messages fetched per conversation page.
	catchupPageLimit = 50
	// catchupMaxPages bounds pagination per conversation during catch-up.
	catchupMaxPages = 3
	// maxCatchupLookback caps how far back the catch-up window can reach
	// when undelivered messages keep the anchor from advancing.
	maxCatchupLookback = 24 * time.Hour
	// maxDeliveryPasses is how many catch-up passes may retry a message
	// whose email delivery keeps failing before the relay gives up on it.
	maxDeliveryPasses = 12
	// shutdownGrace is how long Run waits for the worker to finish the
	// message it is handling after ctx is cancelled.
	shutdownGrace = 30 * time.Second
	// disconnectAlertAfter is how long SignalR may stay disconnected before
	// an alert email is sent.
	disconnectAlertAfter = 5 * time.Minute
	// healthInterval is how often the health file is refreshed.
	healthInterval = 60 * time.Second
	// replyTimeout bounds processing of one inbound email reply, including
	// ffmpeg transcoding and the upload to Garmin.
	replyTimeout = 5 * time.Minute
	// stageTimeout bounds a single network stage (media download, one SMTP
	// send, one Garmin API call).
	stageTimeout = 60 * time.Second
	// transcodeTimeout bounds one ffmpeg run.
	transcodeTimeout = 2 * time.Minute
	// noticeCooldown is the minimum time between failure notices to the
	// same address.
	noticeCooldown = 10 * time.Minute
	// ffmpegMaxPixels caps the decoded image size so a tiny compressed file
	// cannot expand into a multi-gigabyte frame.
	ffmpegMaxPixels = "50000000"
)

// retryDelays are the waits between successive attempts of a send. Three
// attempts in total; overridden in tests.
var retryDelays = []time.Duration{10 * time.Second, 30 * time.Second}

// emailSender abstracts the SMTP mailer so it can be faked in tests.
type emailSender interface {
	Send(Message) error
	SendContext(context.Context, Message) error
}

// Relay listens for incoming Garmin messages, forwards them via email,
// and routes email replies back to Garmin Messenger.
type Relay struct {
	cfg     *Config
	auth    *gm.HermesAuth
	api     *gm.HermesAPI
	sr      *gm.HermesSignalR
	mailer  emailSender
	store   *MsgStore
	state   *State
	imap    *IMAPClient
	alerter *Alerter
	log     *slog.Logger

	inbox     chan gm.MessageModel
	catchupCh chan struct{}
	connected atomic.Bool

	// noticeMu guards noticeSent, a per-address cooldown for failure notices
	// so the relay cannot be used as a mail reflector.
	noticeMu   sync.Mutex
	noticeSent map[string]time.Time

	// deliveryFailures counts total delivery failures per message; worker
	// goroutine only.
	deliveryFailures map[uuid.UUID]int
	workerDone       chan struct{}
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

	store, err := NewMsgStore(sessDir, cfg.IMAP.ReplyWindowDays, log)
	if err != nil {
		return nil, fmt.Errorf("loading message store: %w", err)
	}
	state, err := LoadState(sessDir, log)
	if err != nil {
		return nil, fmt.Errorf("loading relay state: %w", err)
	}

	r := &Relay{
		cfg:              cfg,
		auth:             auth,
		api:              api,
		sr:               sr,
		mailer:           mailer,
		store:            store,
		state:            state,
		alerter:          NewAlerter(cfg.Alerts, mailer, log),
		log:              log,
		inbox:            make(chan gm.MessageModel, inboxSize),
		catchupCh:        make(chan struct{}, 1),
		noticeSent:       make(map[string]time.Time),
		deliveryFailures: make(map[uuid.UUID]int),
		workerDone:       make(chan struct{}),
	}

	if cfg.IMAPEnabled() {
		r.imap = NewIMAPClient(cfg.IMAP, log, r.alerter)
	}

	return r, nil
}

// Resume loads the saved Garmin session from disk.
func (r *Relay) Resume(ctx context.Context) error {
	return r.auth.Resume(ctx)
}

// Run starts the SignalR listener, the message worker, the catch-up loop and
// the IMAP poller (if configured), and blocks until ctx is cancelled or the
// Garmin connection is lost permanently.
func (r *Relay) Run(ctx context.Context) error {
	// Validate session with a lightweight API call.
	if _, err := r.api.GetConversations(ctx, gm.WithLimit(1)); err != nil {
		r.alerter.Send(AlertSessionExpired,
			"Garmin session expired",
			fmt.Sprintf("The relay could not connect to Garmin Messenger.\n"+
				"Error: %v\n\nRun 'garmin-messenger-relay login' to re-authenticate.", err))
		return fmt.Errorf("connecting to Garmin Messenger: %w\n\nRun 'garmin-messenger-relay login' to authenticate", err)
	}

	r.log.Info("Connected to Garmin Messenger", "phone", r.cfg.Garmin.Phone)

	// The SignalR callbacks below run on the library's goroutines and must
	// never block: they only hand work to channels.
	r.sr.OnMessage(r.enqueue)

	var disconnectTimer *time.Timer
	var disconnectMu sync.Mutex

	r.sr.OnOpen(func() {
		r.log.Info("SignalR connected")
		r.setConnected(true)
		disconnectMu.Lock()
		if disconnectTimer != nil {
			disconnectTimer.Stop()
			disconnectTimer = nil
		}
		disconnectMu.Unlock()
		r.triggerCatchup()
	})

	r.sr.OnClose(func() {
		r.log.Warn("SignalR disconnected, will reconnect automatically")
		r.setConnected(false)
		disconnectMu.Lock()
		if disconnectTimer == nil {
			disconnectTimer = time.AfterFunc(disconnectAlertAfter, func() {
				r.alerter.Send(AlertSignalRDisconnect,
					"Garmin SignalR disconnected for 5+ minutes",
					"The relay has been unable to reconnect to Garmin Messenger for over 5 minutes.\n\n"+
						"Check the logs or run 'garmin-messenger-relay status'.")
			})
		}
		disconnectMu.Unlock()
	})

	r.sr.OnError(func(err error) {
		r.log.Error("SignalR error", "error", err)
		var apiErr *gm.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == 401 || apiErr.StatusCode == 403) {
			go r.alerter.Send(AlertSessionExpired,
				"Garmin session expired",
				fmt.Sprintf("Garmin rejected the relay's credentials while reconnecting.\n"+
					"Error: %v\n\nRun 'garmin-messenger-relay login' to re-authenticate.", err))
		}
	})

	r.log.Info("Listening for Garmin messages", "default_recipients", r.cfg.Forwarding.DefaultRecipients)

	go r.worker(ctx)
	go r.catchupLoop(ctx)
	go r.healthLoop(ctx)

	// Start IMAP listener for inbound replies in a separate goroutine.
	if r.imap != nil {
		go r.imap.Start(ctx)
		go r.processReplies(ctx)
		r.log.Info("IMAP reply listener started", "host", r.cfg.IMAP.Host)
	}

	err := r.sr.Start(ctx)
	r.setConnected(false)

	// Let the worker finish the message it is on so an email that was
	// just sent is also recorded as processed.
	select {
	case <-r.workerDone:
	case <-time.After(shutdownGrace):
		r.log.Warn("worker did not finish within shutdown grace period")
	}

	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("SignalR: %w", err)
	}
	return nil
}

// Stop shuts down the SignalR connection.
func (r *Relay) Stop() {
	r.sr.Stop()
	r.api.Close()
}

// setConnected records the connection state for the health file.
func (r *Relay) setConnected(up bool) {
	r.connected.Store(up)
	r.writeHealth()
}

func (r *Relay) writeHealth() {
	h := HealthStatus{Connected: r.connected.Load()}
	if err := WriteHealth(r.cfg.Garmin.SessionDir, h); err != nil {
		r.log.Warn("failed to write health file", "err", err)
	}
}

// healthLoop refreshes the health file so a hung process is detectable.
func (r *Relay) healthLoop(ctx context.Context) {
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.writeHealth()
		}
	}
}

// enqueue hands a live SignalR message to the worker without blocking the
// library goroutine. Catch-up recovers anything dropped here.
func (r *Relay) enqueue(msg gm.MessageModel) {
	select {
	case r.inbox <- msg:
	default:
		r.log.Error("message inbox full, dropping live message; catch-up will recover it",
			"msg_id", msg.MessageID)
	}
}

// triggerCatchup requests a catch-up pass; bursts are coalesced.
func (r *Relay) triggerCatchup() {
	select {
	case r.catchupCh <- struct{}{}:
	default:
	}
}

// worker is the single consumer of the inbox. Serialising here keeps emails
// in order and makes the processed set single-writer.
func (r *Relay) worker(ctx context.Context) {
	defer close(r.workerDone)
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-r.inbox:
			r.processMessage(ctx, msg)
		}
	}
}

// catchupLoop runs a catch-up pass whenever triggered (every reconnect) and
// periodically as a safety net. The periodic pass also retries messages whose
// email delivery failed, since those are left unprocessed.
func (r *Relay) catchupLoop(ctx context.Context) {
	var tick <-chan time.Time
	if r.cfg.Forwarding.CatchupIntervalMinutes > 0 {
		ticker := time.NewTicker(time.Duration(r.cfg.Forwarding.CatchupIntervalMinutes) * time.Minute)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.catchupCh:
		case <-tick:
		}
		if err := r.runCatchup(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("catch-up pass failed; will retry on next trigger", "err", err)
		}
	}
}

// messageTime returns the best available timestamp for ordering and window
// checks: receivedAt when present (server time), otherwise sentAt (device
// time, which for satellite messages can be well before receipt).
func messageTime(m gm.MessageModel) time.Time {
	if m.ReceivedAt != nil {
		return *m.ReceivedAt
	}
	if m.SentAt != nil {
		return *m.SentAt
	}
	return time.Time{}
}

// runCatchup fetches conversations updated since the last successful pass
// and enqueues any message the relay has not processed yet.
func (r *Relay) runCatchup(ctx context.Context) error {
	started := time.Now()
	windowStart := r.state.LastCatchupOK()
	if !r.state.FirstRun() {
		// On the very first pass the anchor is "now" and everything before
		// it was handled by whatever ran previously; looking back would
		// re-send emails. Later passes overlap to absorb delayed receipt.
		windowStart = windowStart.Add(-catchupMargin)
	}
	r.log.Debug("catch-up pass starting", "since", windowStart)

	cctx, cancel := context.WithTimeout(ctx, stageTimeout)
	convs, err := r.api.GetConversations(cctx, gm.WithAfterDate(windowStart), gm.WithLimit(100))
	cancel()
	if err != nil {
		return fmt.Errorf("listing conversations: %w", err)
	}

	var pending []gm.MessageModel
	for _, conv := range convs.Conversations {
		msgs, err := r.fetchConversationSince(ctx, conv.ConversationID, windowStart)
		if err != nil {
			return fmt.Errorf("conversation %s: %w", conv.ConversationID, err)
		}
		for _, m := range msgs {
			if r.state.IsProcessed(m.MessageID) {
				continue
			}
			if skip, _ := r.shouldSkip(m); skip {
				continue
			}
			pending = append(pending, m)
		}
	}

	sort.Slice(pending, func(i, j int) bool {
		return messageTime(pending[i]).Before(messageTime(pending[j]))
	})
	if len(pending) > 0 {
		r.log.Info("catch-up found unprocessed messages", "count", len(pending))
	}
	for _, m := range pending {
		select {
		case r.inbox <- m:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// The anchor must not move past a message that is still unprocessed
	// (e.g. SMTP down), otherwise a later pass would no longer find it. It
	// is capped so one poison message cannot pin the window forever; the
	// worker gives up on such a message after maxDeliveryPasses.
	anchor := started
	if len(pending) > 0 {
		oldest := messageTime(pending[0])
		if oldest.Before(anchor) {
			anchor = oldest
		}
		if floor := started.Add(-maxCatchupLookback); anchor.Before(floor) {
			anchor = floor
		}
	}
	r.state.SetLastCatchupOK(anchor)
	return nil
}

// fetchConversationSince returns messages in the conversation newer than
// since, paginating backwards a bounded number of times.
func (r *Relay) fetchConversationSince(ctx context.Context, convID uuid.UUID, since time.Time) ([]gm.MessageModel, error) {
	var out []gm.MessageModel
	seen := make(map[uuid.UUID]struct{})
	var olderThan *uuid.UUID
	for page := 0; page < catchupMaxPages; page++ {
		opts := []gm.GetConversationDetailOption{gm.WithDetailLimit(catchupPageLimit)}
		if olderThan != nil {
			opts = append(opts, gm.WithOlderThanID(*olderThan))
		}
		cctx, cancel := context.WithTimeout(ctx, stageTimeout)
		detail, err := r.api.GetConversationDetail(cctx, convID, opts...)
		cancel()
		if err != nil {
			return nil, err
		}
		if len(detail.Messages) == 0 {
			break
		}
		// Do not assume the page order: find the oldest message on the
		// page and paginate from it.
		var oldest time.Time
		var oldestID uuid.UUID
		for i, cm := range detail.Messages {
			m := cm.ToMessageModel(convID)
			t := messageTime(m)
			if i == 0 || t.Before(oldest) {
				oldest, oldestID = t, m.MessageID
			}
			if _, dup := seen[m.MessageID]; dup {
				continue // pages overlap or the server ignored olderThanId
			}
			seen[m.MessageID] = struct{}{}
			if !t.IsZero() && t.After(since) {
				out = append(out, m)
			}
		}
		if oldest.IsZero() || oldest.Before(since) || (olderThan != nil && oldestID == *olderThan) {
			break
		}
		olderThan = &oldestID
	}
	return out, nil
}

// shouldSkip decides whether a Garmin message is ignored rather than
// forwarded, returning the reason. Skipped messages are marked processed so
// they are not re-evaluated on every catch-up.
func (r *Relay) shouldSkip(msg gm.MessageModel) (bool, string) {
	if msg.MessageType.IsReaction() {
		return true, "reaction"
	}
	from := derefStr(msg.From)
	if from != "" && from == r.cfg.Garmin.Phone {
		return true, "own message"
	}
	if !r.cfg.Forwarding.SenderAllowed(from) {
		return true, "sender not in allowed_senders"
	}
	return false, ""
}

// processReplies drains the IMAP reply channel and handles each reply.
func (r *Relay) processReplies(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case reply := <-r.imap.Replies:
			rctx, cancel := context.WithTimeout(ctx, replyTimeout)
			r.handleReply(rctx, reply)
			cancel()
		}
	}
}

// handleReply sends an email reply back to the originating Garmin sender.
func (r *Relay) handleReply(ctx context.Context, reply InboundReply) {
	sent, ok := r.store.Lookup(reply.InReplyTo)
	if !ok {
		r.log.Info("Reply to unknown/expired message, ignoring", "in_reply_to", reply.InReplyTo, "from", reply.From)
		// Only people the relay has actually emailed get a notice; anyone
		// else could be forging the reply to bounce mail off the relay.
		if r.isKnownRecipient(reply.From) {
			r.notifyReplyFailure(reply, fmt.Sprintf(
				"The message you replied to is unknown to the relay or older than %d days, so the reply cannot be routed to the inReach.",
				r.cfg.IMAP.ReplyWindowDays))
		}
		return
	}

	// Verify the reply came from the expected recipient. No notice is sent
	// on mismatch: the From address may be forged.
	if !strings.EqualFold(reply.From, sent.RecipientEmail) {
		r.log.Warn("reply sender mismatch, ignoring",
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
	caption := buildReplyCaption(reply)

	var failures []string
	captionSent := false
	for _, att := range reply.Attachments {
		// Transcoding is deterministic, so it is done once and only the
		// upload is retried.
		media, err := transcodeAttachment(ctx, att)
		if err != nil {
			r.log.Error("failed to transcode attachment", "filename", att.Filename, "err", err)
			failures = append(failures, fmt.Sprintf("%s: %v", att.Filename, err))
			continue
		}
		c := ""
		if !captionSent {
			c = caption
		}
		// Fixed IDs across attempts: a retry after an ambiguous failure
		// (timeout after Garmin accepted) must not become a second paid
		// satellite message.
		opts, err := idempotentSendOptions()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", att.Filename, err))
			continue
		}
		opts = append(opts, media.opts...)
		err = r.withRetry(ctx, "send attachment to Garmin", func(ctx context.Context) error {
			_, err := r.api.SendMediaMessage(ctx, to, c, media.data, media.mediaType, opts...)
			return err
		})
		if err != nil {
			r.log.Error("failed to forward attachment to Garmin", "filename", att.Filename, "err", err)
			failures = append(failures, fmt.Sprintf("%s: %v", att.Filename, err))
			continue
		}
		if c != "" {
			captionSent = true
		}
	}

	// Send the text on its own when there were no attachments or all of
	// them failed.
	if !captionSent && caption != "" {
		opts, err := idempotentSendOptions()
		if err == nil {
			err = r.withRetry(ctx, "send reply to Garmin", func(ctx context.Context) error {
				_, err := r.api.SendMessage(ctx, to, caption, opts...)
				return err
			})
		}
		if err != nil {
			r.log.Error("failed to send reply text to Garmin", "err", err)
			failures = append(failures, fmt.Sprintf("text: %v", err))
		}
	}

	if caption == "" && len(reply.Attachments) == 0 {
		failures = append(failures, "the reply contained no text and no supported image/audio attachment")
	}
	failures = append(failures, reply.SkippedAttachments...)

	if len(failures) > 0 {
		r.notifyReplyFailure(reply, "Parts of your reply could not be delivered to the inReach:\n\n  - "+
			strings.Join(failures, "\n  - "))
	}
}

// idempotentSendOptions returns SendMessage options that pin the message,
// OTA and media IDs so that every retry of one logical send carries the
// same identifiers.
func idempotentSendOptions() ([]gm.SendMessageOption, error) {
	otaUUID, err := gm.GenerateOTAUUID()
	if err != nil {
		return nil, fmt.Errorf("generating OTA UUID: %w", err)
	}
	return []gm.SendMessageOption{
		gm.WithMessageUUID(uuid.New(), otaUUID),
		gm.WithMediaID(uuid.New()),
	}, nil
}

// buildReplyCaption composes the text sent to the inReach: the sender's name
// followed by their message, limited to what the device can display.
func buildReplyCaption(reply InboundReply) string {
	caption := strings.TrimSpace(reply.TextBody)
	if caption == "" {
		return ""
	}
	// Prefix with sender name so the inReach user knows who replied.
	// All replies appear to come from the relay's phone number, so
	// without this the inReach user cannot tell senders apart.
	sender := strings.TrimSpace(reply.FromName)
	if sender == "" {
		sender = reply.From
	}
	return truncateRunes(sender+": "+caption, maxReplyChars)
}

// isKnownRecipient reports whether the relay has ever addressed email to
// addr: a configured recipient, the alert address, or a recipient of a
// forwarded message still in the store.
func (r *Relay) isKnownRecipient(addr string) bool {
	for _, d := range r.cfg.Forwarding.DefaultRecipients {
		if strings.EqualFold(d, addr) {
			return true
		}
	}
	if r.cfg.Alerts.Email != "" && strings.EqualFold(r.cfg.Alerts.Email, addr) {
		return true
	}
	return r.store.HasRecipient(addr)
}

// notifyReplyFailure emails the replier about a reply that could not be
// delivered. The notice's Message-ID deliberately lacks the relay marker so
// that replies to it are ignored, the subject is fixed rather than echoed,
// and each address gets at most one notice per cooldown period.
func (r *Relay) notifyReplyFailure(reply InboundReply, reason string) {
	addr := strings.ToLower(reply.From)
	r.noticeMu.Lock()
	last, seen := r.noticeSent[addr]
	if seen && time.Since(last) < noticeCooldown {
		r.noticeMu.Unlock()
		r.log.Info("failure notice suppressed (cooldown)", "to", reply.From)
		return
	}
	r.noticeSent[addr] = time.Now()
	r.noticeMu.Unlock()

	msg := Message{
		To:        []string{reply.From},
		Subject:   "Your reply could not be delivered to the inReach",
		InReplyTo: reply.MessageID,
		MessageID: fmt.Sprintf("<notice-%08x-%08x@garmin-messenger-relay.local>", rand.Uint32(), rand.Uint32()),
		Body: "This is an automatic notice from garmin-messenger-relay.\n\n" +
			reason + "\n\n" +
			"Supported attachments are images (sent as AVIF) and audio clips (sent as OGG Opus, max 30 seconds).\n",
	}
	if err := r.mailer.Send(msg); err != nil {
		r.log.Warn("failed to send reply failure notice", "to", reply.From, "err", err)
	}
}

// withRetry runs op up to len(retryDelays)+1 times with a per-attempt
// timeout, waiting retryDelays[i] between attempts.
func (r *Relay) withRetry(ctx context.Context, what string, op func(context.Context) error) error {
	var err error
	for attempt := 0; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, stageTimeout)
		err = op(actx)
		cancel()
		if err == nil || ctx.Err() != nil || attempt >= len(retryDelays) {
			return err
		}
		r.log.Warn("operation failed, retrying", "what", what, "attempt", attempt+1, "retry_in", retryDelays[attempt], "err", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(retryDelays[attempt]):
		}
	}
}

// transcodedMedia is an attachment converted to a Garmin-native format.
type transcodedMedia struct {
	data      []byte
	mediaType gm.MediaType
	opts      []gm.SendMessageOption
}

// transcodeAttachment converts an inbound email attachment to a
// Garmin-compatible format (AVIF for images, OGG Opus for audio).
func transcodeAttachment(ctx context.Context, att InboundAttachment) (*transcodedMedia, error) {
	tctx, cancel := context.WithTimeout(ctx, transcodeTimeout)
	defer cancel()

	ct := strings.ToLower(att.ContentType)
	switch {
	case strings.HasPrefix(ct, "image/"):
		avif, err := transcodeImageToAVIF(tctx, att.Data)
		if err != nil {
			return nil, fmt.Errorf("transcoding image to AVIF: %w", err)
		}
		return &transcodedMedia{data: avif, mediaType: gm.MediaTypeImageAvif}, nil

	case strings.HasPrefix(ct, "audio/"):
		ogg, err := transcodeAudioToOGG(tctx, att.Data)
		if err != nil {
			return nil, fmt.Errorf("transcoding audio to OGG: %w", err)
		}
		m := &transcodedMedia{data: ogg, mediaType: gm.MediaTypeAudioOgg}
		if durationMs, _ := getAudioDurationMs(tctx, ogg); durationMs > 0 {
			ms := durationMs
			m.opts = append(m.opts, gm.WithMediaMetadata(gm.MediaMetadata{DurationMs: &ms}))
		}
		return m, nil

	default:
		return nil, fmt.Errorf("unsupported attachment type %q", att.ContentType)
	}
}

// processMessage forwards one Garmin message by email to every recipient,
// each in its own email with its own Message-ID. It runs on the worker
// goroutine only.
func (r *Relay) processMessage(ctx context.Context, msg gm.MessageModel) {
	if r.state.IsProcessed(msg.MessageID) {
		r.log.Debug("message already processed, skipping", "msg_id", msg.MessageID)
		return
	}

	r.log.Info("Processing Garmin message",
		"msg_id", msg.MessageID,
		"conversation_id", msg.ConversationID,
		"from", derefStr(msg.From),
	)

	if skip, reason := r.shouldSkip(msg); skip {
		r.log.Debug("skipping message", "reason", reason, "msg_id", msg.MessageID, "from", derefStr(msg.From))
		r.state.MarkProcessed(msg.MessageID)
		return
	}

	caption := derefStr(msg.MessageBody)
	recipients := r.resolveRecipients(caption)
	if len(recipients) == 0 {
		r.log.Warn("No recipients for message, dropping", "msg_id", msg.MessageID)
		r.state.MarkProcessed(msg.MessageID)
		return
	}

	body := r.buildEmailBody(msg)
	subject := r.buildSubject(msg)

	// Download media once; it is shared between recipients.
	var attachments []Attachment
	if msg.MediaID != nil && r.cfg.Forwarding.ForwardMedia {
		dctx, cancel := context.WithTimeout(ctx, stageTimeout)
		att, err := r.downloadMedia(dctx, msg)
		cancel()
		if err != nil {
			r.log.Error("Failed to download media", "error", err, "msg_id", msg.MessageID)
			body += "\n\n[Media attachment could not be downloaded: " + err.Error() + "]"
		} else {
			attachments = append(attachments, *att)
		}
	}

	var delivered, failed []string
	for _, recipient := range recipients {
		msgID := fmt.Sprintf("<gmr-%s-%s-%08x@garmin-messenger-relay.local>",
			msg.ConversationID, msg.MessageID, rand.Uint32())
		email := Message{
			To:          []string{recipient},
			Subject:     subject,
			Body:        body,
			MessageID:   msgID,
			Attachments: attachments,
		}
		err := r.withRetry(ctx, "send email to "+recipient, func(ctx context.Context) error {
			return r.mailer.SendContext(ctx, email)
		})
		if err != nil {
			r.log.Error("Failed to send email", "error", err, "recipient", recipient, "msg_id", msg.MessageID)
			failed = append(failed, recipient)
			continue
		}
		delivered = append(delivered, recipient)
		if r.cfg.IMAPEnabled() {
			if err := r.store.Add(SentMessage{
				MessageID:      msgID,
				ConversationID: msg.ConversationID,
				SenderPhone:    derefStr(msg.From),
				RecipientEmail: recipient,
				SentAt:         time.Now(),
			}); err != nil {
				r.log.Error("failed to record sent message; replies from this recipient will not route", "err", err)
			}
		}
	}

	if len(delivered) == 0 {
		r.deliveryFailures[msg.MessageID]++
		passes := r.deliveryFailures[msg.MessageID]
		if passes < maxDeliveryPasses {
			// Nothing went out: leave the message unprocessed so the next
			// catch-up pass retries it, and tell the operator.
			r.alerter.Send(AlertSMTPFailure,
				"Email delivery failed",
				fmt.Sprintf("A Garmin message from %s could not be emailed to %s after %d attempts.\n"+
					"The relay will retry on the next catch-up pass (%d of %d).\n\nSubject: %s",
					derefStr(msg.From), strings.Join(failed, ", "), len(retryDelays)+1, passes, maxDeliveryPasses, subject))
			return
		}
		// Giving up keeps one undeliverable message from pinning the
		// catch-up window and re-alerting forever.
		r.log.Error("giving up on message after repeated delivery failures", "msg_id", msg.MessageID, "passes", passes)
		delete(r.deliveryFailures, msg.MessageID)
		r.state.MarkProcessed(msg.MessageID)
		r.alerter.Send(AlertSMTPFailure,
			"Giving up on undeliverable message",
			fmt.Sprintf("A Garmin message from %s could not be emailed to %s in %d catch-up passes and will not be retried.\n\n"+
				"Subject: %s\nMessage ID: %s\n\nThe message is still in Garmin Messenger.",
				derefStr(msg.From), strings.Join(failed, ", "), passes, subject, msg.MessageID))
		return
	}
	delete(r.deliveryFailures, msg.MessageID)

	r.log.Info("Email forwarded", "recipients", delivered, "subject", subject)
	if len(failed) > 0 {
		// Partial failure is almost always a rejected address rather than an
		// outage; retrying would re-send to the recipients that succeeded.
		r.log.Error("some recipients could not be reached; not retrying", "failed", failed, "msg_id", msg.MessageID)
		r.alerter.Send(AlertSMTPFailure,
			"Email delivery failed for some recipients",
			fmt.Sprintf("A Garmin message from %s was delivered to %s but not to %s.\n\nSubject: %s",
				derefStr(msg.From), strings.Join(delivered, ", "), strings.Join(failed, ", "), subject))
	}

	r.state.MarkProcessed(msg.MessageID)

	// Mark as delivered through the REST API so it works identically for
	// live and catch-up messages, and failures are visible.
	mctx, cancel := context.WithTimeout(ctx, stageTimeout)
	defer cancel()
	if _, err := r.api.MarkAsDelivered(mctx, msg.ConversationID, msg.MessageID); err != nil {
		r.log.Warn("failed to mark message as delivered", "msg_id", msg.MessageID, "err", err)
	}
}

// senderLabel renders the sender for emails: "Name (+47...)" when a contact
// name is configured, otherwise the phone number.
func (r *Relay) senderLabel(msg gm.MessageModel) string {
	from := derefStr(msg.From)
	if from == "" {
		return "(unknown)"
	}
	if name := r.cfg.Forwarding.DisplayName(from); name != from {
		return fmt.Sprintf("%s (%s)", name, from)
	}
	return from
}

// formatTime renders a timestamp in the relay's local time zone with the
// zone name, so recipients are not left guessing whether it is UTC.
func formatTime(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04:05 MST")
}

// buildSubject constructs the email subject line.
// Format: [InReach] Ola (+47...) • 2025-03-28 14:32:15 CET • image.avif
func (r *Relay) buildSubject(msg gm.MessageModel) string {
	ts := time.Now()
	if msg.SentAt != nil {
		ts = *msg.SentAt
	}

	subject := fmt.Sprintf("[InReach] %s • %s", r.senderLabel(msg), formatTime(ts))

	if msg.MediaType != nil {
		switch *msg.MediaType {
		case gm.MediaTypeImageAvif:
			subject += " • image.avif"
		case gm.MediaTypeAudioOgg:
			subject += " • audio.ogg"
		}
	}

	return subject
}

// buildEmailBody constructs the plain-text email body from a message.
func (r *Relay) buildEmailBody(msg gm.MessageModel) string {
	var sb strings.Builder

	ts := time.Now()
	if msg.SentAt != nil {
		ts = *msg.SentAt
	}

	fmt.Fprintf(&sb, "From: %s\n", r.senderLabel(msg))

	// Caption / text body
	caption := derefStr(msg.MessageBody)
	if caption != "" && r.cfg.Forwarding.ForwardText {
		fmt.Fprintf(&sb, "Caption: %s\n", caption)
	} else {
		sb.WriteString("Caption: (empty)\n")
	}

	// Location
	if msg.UserLocation != nil && r.cfg.Forwarding.ForwardLocation {
		loc := msg.UserLocation
		lat := derefFloat64(loc.LatitudeDegrees)
		lon := derefFloat64(loc.LongitudeDegrees)

		if lat != 0 || lon != 0 {
			fmt.Fprintf(&sb, "Location: %.6f, %.6f\n", lat, lon)
			fmt.Fprintf(&sb, "Map: https://www.openstreetmap.org/?mlat=%.6f&mlon=%.6f#map=14/%.6f/%.6f&layers=P\n",
				lat, lon, lat, lon)
		}

		if alt := derefFloat64(loc.ElevationMeters); alt != 0 {
			fmt.Fprintf(&sb, "Altitude: %.1f m\n", alt)
		}
	}

	fmt.Fprintf(&sb, "Sent: %s\n", formatTime(ts))
	fmt.Fprintf(&sb, "Message ID: %s\n", msg.MessageID)

	// Attachment info
	if msg.MediaType != nil {
		switch *msg.MediaType {
		case gm.MediaTypeImageAvif:
			sb.WriteString("Attachment: image.avif\n")
		case gm.MediaTypeAudioOgg:
			sb.WriteString("Attachment: audio.ogg\n")
		}
	}

	return sb.String()
}

// resolveRecipients returns the list of email recipients for a message.
// If caption_routing is enabled, email addresses are parsed from the caption
// and filtered through the caption allowlist.
func (r *Relay) resolveRecipients(caption string) []string {
	var parsed []string
	if r.cfg.Forwarding.CaptionRouting && caption != "" {
		for _, addr := range emailRegexp.FindAllString(caption, -1) {
			if r.cfg.Forwarding.CaptionAddressAllowed(addr) {
				parsed = append(parsed, addr)
			} else {
				r.log.Warn("caption address not in caption_routing_allowlist, ignoring", "address", addr)
			}
		}
	}

	if len(parsed) > 0 && r.cfg.Forwarding.CaptionRoutingReplacesDefault {
		return unique(parsed)
	}

	all := append([]string{}, r.cfg.Forwarding.DefaultRecipients...)
	all = append(all, parsed...)
	return unique(all)
}

// downloadMedia fetches the media attachment from Garmin and returns an Attachment
// in its original format (AVIF for images, OGG for audio).
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
		return &Attachment{
			Filename:    "image.avif",
			ContentType: "image/avif",
			Data:        data,
		}, nil

	case gm.MediaTypeAudioOgg:
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

// transcodeImageToAVIF converts image data (JPEG, PNG, etc.) to AVIF using ffmpeg.
// The AVIF muxer requires seekable output, so we write to a temp file.
func transcodeImageToAVIF(ctx context.Context, data []byte) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found")
	}
	tmp, err := os.CreateTemp("", "garmin-avif-*.avif")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpPath) }()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-max_pixels", ffmpegMaxPixels,
		"-i", "pipe:0",
		"-vf", "scale='min(1920,iw)':'min(1920,ih)':force_original_aspect_ratio=decrease:flags=lanczos",
		"-c:v", "libaom-av1",
		"-crf", "30", "-b:v", "0",
		"-cpu-used", "6",
		"-pix_fmt", "yuv444p",
		"-y", tmpPath,
	)
	cmd.Stdin = bytes.NewReader(data)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg avif: %w: %s", err, errBuf.String())
	}
	return os.ReadFile(tmpPath)
}

// transcodeAudioToOGG converts audio data (MP3, M4A, WAV, etc.) to OGG Opus.
// Uses mono 16kbps at 48kHz with a 30-second limit for satellite bandwidth.
func transcodeAudioToOGG(ctx context.Context, data []byte) ([]byte, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, fmt.Errorf("ffmpeg not found")
	}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", "pipe:0",
		"-t", "30",
		"-ar", "48000",
		"-ac", "1",
		"-c:a", "libopus",
		"-b:a", "16k",
		"-f", "ogg", "pipe:1",
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
