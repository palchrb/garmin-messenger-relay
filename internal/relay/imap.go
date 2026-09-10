package relay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/textproto"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"
)

// relayMessageIDRegexp is the exact shape of a Message-ID the relay generates
// for forwarded Garmin messages:
// <gmr-CONVERSATION-MESSAGE-RANDOM@garmin-messenger-relay.local>.
// Only replies referencing such an ID are ever read from the inbox.
var relayMessageIDRegexp = regexp.MustCompile(`^<gmr-[0-9a-f-]{36}-[0-9a-f-]{36}-[0-9a-f]{8}@garmin-messenger-relay\.local>$`)

// maxAttachmentsPerReply bounds how many media files one reply may turn
// into satellite messages.
const maxAttachmentsPerReply = 3

// InboundReply holds the parsed content of an email reply from a recipient.
type InboundReply struct {
	// InReplyTo is the Message-ID this email is replying to (from In-Reply-To header).
	InReplyTo string
	// MessageID is the Message-ID of the reply email itself, used to thread
	// failure notices back to the sender.
	MessageID string
	// Subject is the reply's subject line.
	Subject string
	// From is the reply sender's email address.
	From string
	// FromName is the display name of the reply sender (e.g. "Alice").
	FromName string
	// TextBody is the plain-text reply content with quoted lines stripped.
	TextBody string
	// Attachments holds any media files attached to the reply.
	Attachments []InboundAttachment
	// SkippedAttachments lists attachments that were dropped (too large,
	// unreadable) so the sender can be told about them.
	SkippedAttachments []string
	// Authenticated reports whether the mail provider's Authentication-Results
	// header showed DKIM or DMARC pass for this message.
	Authenticated bool
}

// InboundAttachment is a single media attachment from an inbound email reply.
type InboundAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// errIMAPAuth marks IMAP login failures so callers can alert on them.
var errIMAPAuth = errors.New("IMAP authentication failed")

// IMAPClient connects to an IMAP server and uses IDLE to receive replies in
// near-real-time. Falls back to polling if the server does not support IDLE.
// Discovered replies are sent on the Replies channel.
type IMAPClient struct {
	cfg     IMAPConfig
	log     *slog.Logger
	alerter *Alerter
	Replies chan InboundReply

	newMsg chan struct{} // notified by UnilateralDataHandler when EXISTS arrives
	// since bounds the first search on a connection. Replies older than this
	// cannot match a stored message anyway, so there is no point fetching them.
	since time.Time
	// lastUID is the highest UID examined on the current connection; later
	// searches only look above it. Reset on every new connection because
	// UIDs are only stable within a UIDVALIDITY epoch. Accessed from the
	// connection goroutine only.
	lastUID imap.UID
}

// NewIMAPClient creates a new IMAPClient. Call Start to begin listening.
func NewIMAPClient(cfg IMAPConfig, log *slog.Logger, alerter *Alerter) *IMAPClient {
	return &IMAPClient{
		cfg:     cfg,
		log:     log,
		alerter: alerter,
		Replies: make(chan InboundReply, 16),
		newMsg:  make(chan struct{}, 1),
		since:   time.Now().Add(-time.Duration(cfg.ReplyWindowDays) * 24 * time.Hour),
	}
}

// Start connects to the IMAP server and listens for new messages using IDLE.
// It reconnects automatically on failure. Blocks until ctx is cancelled.
func (c *IMAPClient) Start(ctx context.Context) {
	backoff := 5 * time.Second
	for {
		err := c.run(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			return
		}
		c.log.Error("IMAP connection lost, reconnecting", "err", err, "backoff", backoff)
		if errors.Is(err, errIMAPAuth) {
			c.alerter.Send(AlertIMAPAuthFailure,
				"IMAP authentication failed",
				fmt.Sprintf("The relay cannot log in to the IMAP server (%s).\n"+
					"Error: %v\n\nCheck your imap.username and imap.password in config.yaml.",
					c.cfg.Host, err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Minute {
			backoff *= 2
		}
	}
}

const pollInterval = 30 * time.Second

// dial connects, logs in and selects INBOX. The caller must Close the client.
func (c *IMAPClient) dial(options *imapclient.Options) (*imapclient.Client, error) {
	addr := fmt.Sprintf("%s:%d", c.cfg.Host, c.cfg.Port)
	client, err := imapclient.DialTLS(addr, options)
	if err != nil {
		return nil, fmt.Errorf("IMAP dial %s: %w", addr, err)
	}
	if err := client.Login(c.cfg.Username, c.cfg.Password).Wait(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("%w: %v", errIMAPAuth, err)
	}
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("IMAP SELECT INBOX: %w", err)
	}
	return client, nil
}

// Check verifies that the IMAP server is reachable and the credentials work.
// Used by the "status" command.
func (c *IMAPClient) Check() error {
	client, err := c.dial(nil)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	return client.Logout().Wait()
}

func (c *IMAPClient) run(ctx context.Context) error {
	// The UnilateralDataHandler fires on server-push events (new messages, expunges).
	options := &imapclient.Options{
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data.NumMessages != nil {
					// Non-blocking send: if the channel is already full we don't need
					// to queue another notification.
					select {
					case c.newMsg <- struct{}{}:
					default:
					}
				}
			},
		},
	}

	client, err := c.dial(options)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	c.log.Info("IMAP connected", "host", c.cfg.Host)
	c.lastUID = 0

	// Pick up replies that arrived while the relay was not running.
	if err := c.fetchUnseenReplies(ctx, client); err != nil {
		c.log.Warn("initial unseen fetch failed", "err", err)
	}

	if client.Caps().Has(imap.CapIdle) {
		c.log.Info("IMAP server supports IDLE, using push notifications")
		return c.runIdle(ctx, client)
	}
	c.log.Info("IMAP server does not support IDLE, falling back to polling", "interval", pollInterval)
	return c.runPoll(ctx, client)
}

// runIdle uses IMAP IDLE for real-time push notifications.
func (c *IMAPClient) runIdle(ctx context.Context, client *imapclient.Client) error {
	startIdle := func() (*imapclient.IdleCommand, chan error, error) {
		cmd, err := client.Idle()
		if err != nil {
			return nil, nil, err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		return cmd, done, nil
	}

	idleCmd, idleDone, err := startIdle()
	if err != nil {
		return fmt.Errorf("IMAP IDLE: %w", err)
	}

	// RFC 2177: re-send IDLE at least every 29 minutes.
	ticker := time.NewTicker(25 * time.Minute)
	defer ticker.Stop()

	for {
		var reason string
		select {
		case <-ctx.Done():
			_ = idleCmd.Close()
			<-idleDone
			return nil

		case err := <-idleDone:
			if err != nil {
				return fmt.Errorf("IMAP IDLE ended: %w", err)
			}
			return nil

		case <-ticker.C:
			reason = "keepalive"

		case <-c.newMsg:
			reason = "EXISTS"
			c.log.Info("IMAP: new message notification received (EXISTS)")
		}

		// Stop IDLE, fetch, restart.
		if err := idleCmd.Close(); err != nil {
			return fmt.Errorf("IMAP IDLE close (%s): %w", reason, err)
		}
		<-idleDone
		if err := c.fetchUnseenReplies(ctx, client); err != nil {
			c.log.Warn("unseen fetch failed", "reason", reason, "err", err)
		}
		idleCmd, idleDone, err = startIdle()
		if err != nil {
			return fmt.Errorf("IMAP IDLE restart (%s): %w", reason, err)
		}
	}
}

// runPoll periodically checks for unseen messages when IDLE is not available.
func (c *IMAPClient) runPoll(ctx context.Context, client *imapclient.Client) error {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.fetchUnseenReplies(ctx, client); err != nil {
				return fmt.Errorf("poll fetch failed: %w", err)
			}
		}
	}
}

// fetchUnseenReplies finds unseen replies to relay emails, marks them as
// seen, parses them and dispatches InboundReply values to the Replies
// channel.
//
// The server is asked only for unseen messages (newer than the last UID
// handled on this connection, or within the reply window on the first pass)
// and only the In-Reply-To header of those is fetched. Full bodies are
// fetched for the messages that reply to a relay email, so unrelated mail is
// never read or flagged. Server-side HEADER searches are avoided because
// some providers (Gmail) match whole tokens only.
//
// Replies are flagged \Seen before they are handed over for delivery. A
// crash in between loses the reply rather than risking a duplicate paid
// satellite message.
func (c *IMAPClient) fetchUnseenReplies(ctx context.Context, client *imapclient.Client) error {
	criteria := &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}
	if c.lastUID > 0 {
		criteria.UID = []imap.UIDSet{{imap.UIDRange{Start: c.lastUID + 1, Stop: 0}}}
	} else {
		criteria.Since = c.since
	}
	searchData, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return fmt.Errorf("IMAP UID SEARCH: %w", err)
	}
	uids := searchData.AllUIDs()
	if len(uids) == 0 {
		c.log.Debug("No new unseen messages in INBOX")
		return nil
	}

	// Phase 1: In-Reply-To headers only.
	candidates, maxUID, err := c.findRelayReplies(client, uids)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		c.log.Debug("No unseen relay replies in INBOX", "unseen", len(uids))
		c.lastUID = maxUID
		return nil
	}
	c.log.Info("Found unseen relay replies", "count", len(candidates))

	// Phase 2: full bodies of the candidates.
	candidateSet := imap.UIDSetNum(candidates...)
	fetchOptions := &imap.FetchOptions{
		UID: true,
		BodySection: []*imap.FetchItemBodySection{
			{Peek: true}, // BODY.PEEK[] — fetch whole message without setting \Seen
		},
	}
	var replies []InboundReply
	messages := client.Fetch(candidateSet, fetchOptions)
	for {
		msg := messages.Next()
		if msg == nil {
			break
		}
		reply, err := c.parseMessage(msg)
		if err != nil {
			c.log.Warn("Skipping unparseable message", "err", err)
			continue
		}
		if reply == nil {
			continue
		}
		replies = append(replies, *reply)
	}
	if err := messages.Close(); err != nil {
		return fmt.Errorf("IMAP UID FETCH: %w", err)
	}

	// Mark the candidates as seen before dispatching.
	storeFlags := &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Flags:  []imap.Flag{imap.FlagSeen},
		Silent: true,
	}
	if err := client.Store(candidateSet, storeFlags, nil).Close(); err != nil {
		return fmt.Errorf("IMAP UID STORE: %w", err)
	}
	c.lastUID = maxUID

	for _, reply := range replies {
		c.log.Info("Parsed inbound reply", "from", reply.From, "in_reply_to", reply.InReplyTo, "authenticated", reply.Authenticated)
		select {
		case c.Replies <- reply:
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

// findRelayReplies fetches only the In-Reply-To header of the given
// messages and returns the UIDs that reply to a relay email, plus the
// highest UID examined.
func (c *IMAPClient) findRelayReplies(client *imapclient.Client, uids []imap.UID) ([]imap.UID, imap.UID, error) {
	var maxUID imap.UID
	for _, u := range uids {
		if u > maxUID {
			maxUID = u
		}
	}
	fetchOptions := &imap.FetchOptions{
		UID: true,
		BodySection: []*imap.FetchItemBodySection{{
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: []string{"In-Reply-To"},
			Peek:         true,
		}},
	}
	var candidates []imap.UID
	messages := client.Fetch(imap.UIDSetNum(uids...), fetchOptions)
	for {
		msg := messages.Next()
		if msg == nil {
			break
		}
		var uid imap.UID
		var inReplyTo string
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			switch it := item.(type) {
			case imapclient.FetchItemDataUID:
				uid = it.UID
			case imapclient.FetchItemDataBodySection:
				raw, err := io.ReadAll(io.LimitReader(it.Literal, 64*1024))
				if err != nil {
					continue
				}
				hdr, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(append(raw, '\r', '\n')))).ReadMIMEHeader()
				if err != nil && len(hdr) == 0 {
					continue
				}
				inReplyTo = hdr.Get("In-Reply-To")
			}
		}
		if uid != 0 && firstRelayMessageID(inReplyTo) != "" {
			candidates = append(candidates, uid)
		}
	}
	if err := messages.Close(); err != nil {
		return nil, 0, fmt.Errorf("IMAP UID FETCH headers: %w", err)
	}
	return candidates, maxUID, nil
}

// parseMessage extracts an InboundReply from a fetched IMAP message.
// Returns nil, nil if the message should be ignored.
func (c *IMAPClient) parseMessage(msg *imapclient.FetchMessageData) (*InboundReply, error) {
	maxBytes := int64(c.cfg.MaxAttachmentMB) * 1024 * 1024

	// Read the body section immediately — the literal reader is invalidated
	// once msg.Next() is called again (stream-based protocol).
	var bodyData []byte
	for {
		item := msg.Next()
		if item == nil {
			break
		}
		if bs, ok := item.(imapclient.FetchItemDataBodySection); ok {
			// Bound memory: a reply can never usefully be larger than the
			// attachments it may carry (base64 inflates by 4/3) plus text.
			limit := maxBytes*maxAttachmentsPerReply*4/3 + 1024*1024
			var err error
			bodyData, err = io.ReadAll(io.LimitReader(bs.Literal, limit+1))
			if err != nil {
				return nil, fmt.Errorf("reading message body: %w", err)
			}
			if int64(len(bodyData)) > limit {
				// Drain the rest so the protocol stream stays in sync.
				_, _ = io.Copy(io.Discard, bs.Literal)
				return nil, fmt.Errorf("message larger than %d bytes, skipping", limit)
			}
		}
	}
	if bodyData == nil {
		return nil, fmt.Errorf("no body section")
	}

	return parseReply(bytes.NewReader(bodyData), maxBytes, replyAuthPolicy{
		authservID: c.cfg.AuthservID,
		require:    c.cfg.RequireAuthenticatedReplies,
	}, c.log)
}

// replyAuthPolicy controls how Authentication-Results headers are judged.
type replyAuthPolicy struct {
	// authservID identifies the trusted header; empty trusts none for
	// passes but still honours explicit DMARC failures from any header.
	authservID string
	require    bool
}

// parseReply parses a raw RFC 5322 message into an InboundReply. It is
// separated from the IMAP transport so it can be unit-tested.
func parseReply(r io.Reader, maxAttachmentBytes int64, policy replyAuthPolicy, log *slog.Logger) (*InboundReply, error) {
	mr, err := mail.CreateReader(r)
	if err != nil {
		return nil, fmt.Errorf("parsing mail: %w", err)
	}
	header := mr.Header

	subject := header.Get("Subject")
	fromHeader := header.Get("From")

	inReplyTo := strings.TrimSpace(header.Get("In-Reply-To"))
	if inReplyTo == "" {
		log.Debug("Skipping non-reply email (no In-Reply-To header)", "from", fromHeader, "subject", subject)
		return nil, nil
	}
	// Only process replies to emails we sent. The In-Reply-To may list
	// several IDs; use the first relay-shaped one.
	inReplyTo = firstRelayMessageID(inReplyTo)
	if inReplyTo == "" {
		log.Debug("Skipping reply to non-relay email", "in_reply_to", header.Get("In-Reply-To"), "from", fromHeader, "subject", subject)
		return nil, nil
	}

	// Never react to automatic responses (out-of-office, bounces), which
	// would otherwise turn into satellite messages.
	if reason := autoResponseReason(header); reason != "" {
		log.Info("Skipping automatic reply", "reason", reason, "from", fromHeader)
		return nil, nil
	}

	from, err := header.AddressList("From")
	if err != nil || len(from) == 0 {
		return nil, fmt.Errorf("missing From header")
	}

	auth := authenticationVerdict(header.Values("Authentication-Results"), policy.authservID, from[0].Address)
	switch {
	case auth == authFail:
		log.Warn("Rejecting reply that failed sender authentication (DMARC)", "from", from[0].Address)
		return nil, nil
	case policy.require && auth != authPass:
		log.Warn("Rejecting reply without DKIM/DMARC pass from the trusted authserv-id (require_authenticated_replies is on)", "from", from[0].Address, "authserv_id", policy.authservID)
		return nil, nil
	}

	reply := &InboundReply{
		InReplyTo:     inReplyTo,
		MessageID:     strings.TrimSpace(header.Get("Message-Id")),
		Subject:       subject,
		From:          from[0].Address,
		FromName:      from[0].Name,
		Authenticated: auth == authPass,
	}

	var htmlBody string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Debug("stopping at unreadable MIME part", "err", err)
			break
		}
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := h.ContentType()
			ct = strings.ToLower(ct)
			switch {
			case strings.HasPrefix(ct, "text/plain"):
				if reply.TextBody == "" {
					body, _ := io.ReadAll(io.LimitReader(p.Body, 64*1024))
					reply.TextBody = stripQuotedReply(string(body))
				}
			case strings.HasPrefix(ct, "text/html"):
				if htmlBody == "" {
					body, _ := io.ReadAll(io.LimitReader(p.Body, 256*1024))
					htmlBody = string(body)
				}
			case isMediaType(ct):
				// Inline media (Apple Mail sends photos this way).
				_, params, _ := h.ContentType()
				filename := params["name"]
				if _, dispParams, err := h.ContentDisposition(); err == nil && dispParams["filename"] != "" {
					filename = dispParams["filename"]
				}
				if filename == "" {
					filename = "inline"
				}
				readAttachment(reply, p.Body, filename, ct, maxAttachmentBytes, log)
			}
		case *mail.AttachmentHeader:
			ct, _, _ := h.ContentType()
			ct = strings.ToLower(ct)
			filename, _ := h.Filename()
			readAttachment(reply, p.Body, filename, ct, maxAttachmentBytes, log)
		}
	}

	// Fall back to the HTML part when the client sent no plain-text version.
	if reply.TextBody == "" && htmlBody != "" {
		reply.TextBody = stripQuotedReply(htmlToText(htmlBody))
	}

	return reply, nil
}

// isMediaType reports whether the content type is media the relay can
// transcode for Garmin.
func isMediaType(ct string) bool {
	return strings.HasPrefix(ct, "image/") || strings.HasPrefix(ct, "audio/")
}

// firstRelayMessageID returns the first ID in an In-Reply-To value that has
// the exact shape of a relay Message-ID, or "".
func firstRelayMessageID(inReplyTo string) string {
	for _, id := range strings.Fields(inReplyTo) {
		if relayMessageIDRegexp.MatchString(id) {
			return id
		}
	}
	return ""
}

// autoResponseReason reports why a message looks like an automatic reply
// (vacation responder, bounce, list traffic), or "" if it looks human.
func autoResponseReason(h mail.Header) string {
	if v := strings.ToLower(strings.TrimSpace(h.Get("Auto-Submitted"))); v != "" && v != "no" {
		return "Auto-Submitted: " + v
	}
	if v := strings.ToLower(strings.TrimSpace(h.Get("Precedence"))); v == "bulk" || v == "junk" || v == "list" || v == "auto_reply" {
		return "Precedence: " + v
	}
	if v := strings.TrimSpace(h.Get("X-Auto-Response-Suppress")); v != "" {
		return "X-Auto-Response-Suppress: " + v
	}
	if v := strings.TrimSpace(h.Get("X-Autoreply")); v != "" {
		return "X-Autoreply: " + v
	}
	return ""
}

// readAttachment reads a MIME part into the reply's attachment list,
// enforcing the size and count limits.
func readAttachment(reply *InboundReply, body io.Reader, filename, ct string, maxBytes int64, log *slog.Logger) {
	if len(reply.Attachments) >= maxAttachmentsPerReply {
		log.Info("too many attachments, skipping", "filename", filename, "max", maxAttachmentsPerReply)
		reply.SkippedAttachments = append(reply.SkippedAttachments,
			fmt.Sprintf("%s (at most %d attachments per reply)", filename, maxAttachmentsPerReply))
		return
	}
	if !isMediaType(ct) {
		log.Info("ignoring unsupported attachment type", "filename", filename, "content_type", ct)
		reply.SkippedAttachments = append(reply.SkippedAttachments, fmt.Sprintf("%s (unsupported type %s)", filename, ct))
		return
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		log.Warn("reading attachment failed", "filename", filename, "err", err)
		reply.SkippedAttachments = append(reply.SkippedAttachments, fmt.Sprintf("%s (unreadable)", filename))
		return
	}
	if int64(len(data)) > maxBytes {
		log.Warn("attachment too large, skipping",
			"filename", filename,
			"size_mb", len(data)/1024/1024,
			"max_mb", maxBytes/1024/1024)
		reply.SkippedAttachments = append(reply.SkippedAttachments,
			fmt.Sprintf("%s (larger than %d MB)", filename, maxBytes/1024/1024))
		return
	}
	reply.Attachments = append(reply.Attachments, InboundAttachment{
		Filename:    filename,
		ContentType: ct,
		Data:        data,
	})
}

// authVerdict summarises the mail provider's Authentication-Results header.
type authVerdict int

const (
	authUnknown authVerdict = iota
	authPass
	authFail
)

var authResultTokenRegexp = regexp.MustCompile(`(?i)\b(dmarc|dkim)=(pass|fail)\b|\bheader\.(?:d|i)=([^\s;]+)`)

// authenticationVerdict inspects Authentication-Results headers (RFC 8601).
//
// Only headers whose authserv-id matches the configured one are trusted for
// a pass, because a sender can add Authentication-Results headers of their
// own and providers only strip the ones claiming the provider's own
// identifier. A pass requires dmarc=pass, or dkim=pass where the signing
// domain (header.d/header.i) is the From domain or a parent of it, since a
// DKIM signature by an unrelated domain says nothing about From.
//
// A dmarc=fail is honoured from a trusted header, and, when no authserv-id
// is configured, from any header: a sender gains nothing by forging a
// failure.
func authenticationVerdict(headers []string, authservID, fromAddr string) authVerdict {
	fromDomain := ""
	if i := strings.LastIndex(fromAddr, "@"); i >= 0 {
		fromDomain = strings.ToLower(fromAddr[i+1:])
	}
	verdict := authUnknown
	for _, h := range headers {
		l := strings.ToLower(h)
		id := l
		if i := strings.IndexAny(l, "; "); i >= 0 {
			id = l[:i]
		}
		id = strings.TrimSpace(id)
		trusted := authservID != "" && (id == authservID || strings.HasSuffix(id, "."+authservID))

		dmarc, dkim, signer := "", "", ""
		for _, m := range authResultTokenRegexp.FindAllStringSubmatch(l, -1) {
			switch {
			case m[1] == "dmarc":
				dmarc = m[2]
			case m[1] == "dkim":
				dkim = m[2]
			case m[3] != "":
				signer = strings.TrimPrefix(m[3], "@")
				if i := strings.LastIndex(signer, "@"); i >= 0 {
					signer = signer[i+1:]
				}
			}
		}

		if dmarc == "fail" && (trusted || authservID == "") {
			return authFail
		}
		if !trusted {
			continue
		}
		if dmarc == "pass" {
			verdict = authPass
		}
		if dkim == "pass" && signer != "" && fromDomain != "" &&
			(signer == fromDomain || strings.HasSuffix(fromDomain, "."+signer)) {
			verdict = authPass
		}
	}
	return verdict
}

// maxReplyChars is the maximum number of characters to send back to Garmin.
// inReach devices support up to 1600 characters per message.
const maxReplyChars = 1600

// truncateRunes shortens s to at most n runes without splitting a UTF-8
// sequence.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// stripQuotedReply extracts only the new reply text from an email body,
// removing quoted original messages, signatures, and email client boilerplate.
// This is critical because Garmin inReach messages are limited to 1600 characters
// and sent over satellite.
func stripQuotedReply(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")
	var kept []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Stop at standard ">" quoting
		if strings.HasPrefix(trimmed, ">") {
			break
		}

		// Stop at "On <date> <sender> wrote:" (Gmail, Apple Mail)
		if replyHeader(trimmed) {
			break
		}

		// Stop at Outlook separator
		if strings.HasPrefix(trimmed, "________________________________") {
			break
		}
		if strings.HasPrefix(trimmed, "-----Original Message-----") {
			break
		}

		// Stop at Outlook-style header block "From: ... Sent: ..."
		if strings.HasPrefix(trimmed, "From:") && strings.Contains(body, "Sent:") && strings.Contains(body, "Subject:") {
			break
		}

		// Stop at Proton Mail / iOS separator
		if strings.HasPrefix(trimmed, "-------- Original Message --------") {
			break
		}

		// Stop at "Sent from" boilerplate (iOS, Proton Mail, Samsung, etc.)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "sent from ") || strings.HasPrefix(lower, "sendt fra ") {
			break
		}

		// Stop at common signature markers
		if trimmed == "--" || trimmed == "-- " {
			break
		}

		kept = append(kept, line)
	}

	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// replyHeader detects "On ... wrote:" patterns used by Gmail and Apple Mail.
func replyHeader(line string) bool {
	lower := strings.ToLower(line)
	if strings.HasPrefix(lower, "on ") && strings.HasSuffix(lower, "wrote:") {
		return true
	}
	// Norwegian/Danish/Swedish variants, e.g.
	// "den 1. jan. 2025 kl. 10:00 skrev Ola Nordmann <ola@example.com>:"
	if strings.HasSuffix(lower, ":") && (strings.HasPrefix(lower, "den ") || strings.HasPrefix(lower, "på ") || strings.HasPrefix(lower, "man ") || strings.HasPrefix(lower, "tir ") || strings.HasPrefix(lower, "ons ") || strings.HasPrefix(lower, "tor ") || strings.HasPrefix(lower, "fre ") || strings.HasPrefix(lower, "lør ") || strings.HasPrefix(lower, "søn ")) &&
		(strings.Contains(lower, " skrev ") || strings.HasSuffix(lower, " skrev:") || strings.Contains(lower, " skrev:")) {
		return true
	}
	return false
}

var (
	htmlDropRegexp  = regexp.MustCompile(`(?is)<(style|script|head)[^>]*>.*?</(style|script|head)>`)
	htmlBreakRegexp = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|tr|h[1-6]|blockquote)>`)
	htmlTagRegexp   = regexp.MustCompile(`(?s)<[^>]*>`)
	htmlQuoteRegexp = regexp.MustCompile(`(?i)<blockquote[^>]*>`)
	blankLineRegexp = regexp.MustCompile(`\n{3,}`)
)

// htmlToText converts an HTML email body into plain text good enough for
// stripQuotedReply to work on. Gmail's quote container becomes an
// "On ... wrote:" line, blockquotes become ">" lines.
func htmlToText(s string) string {
	s = htmlDropRegexp.ReplaceAllString(s, "")
	// Mark quoted blocks so stripQuotedReply stops there.
	s = htmlQuoteRegexp.ReplaceAllString(s, "\n> ")
	s = htmlBreakRegexp.ReplaceAllString(s, "\n")
	s = htmlTagRegexp.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, " ", " ")
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	s = strings.Join(lines, "\n")
	s = blankLineRegexp.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
