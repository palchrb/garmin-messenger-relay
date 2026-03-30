package relay

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/mail"
)

// InboundReply holds the parsed content of an email reply from a recipient.
type InboundReply struct {
	// InReplyTo is the Message-ID this email is replying to (from In-Reply-To header).
	InReplyTo string
	// From is the reply sender's email address.
	From string
	// FromName is the display name of the reply sender (e.g. "Alice").
	FromName string
	// TextBody is the plain-text reply content with quoted lines stripped.
	TextBody string
	// Attachments holds any media files attached to the reply.
	Attachments []InboundAttachment
}

// InboundAttachment is a single media attachment from an inbound email reply.
type InboundAttachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// IMAPClient connects to an IMAP server and uses IDLE to receive replies in
// near-real-time. Falls back to polling if the server does not support IDLE.
// Discovered replies are sent on the Replies channel.
type IMAPClient struct {
	cfg     IMAPConfig
	log     *slog.Logger
	Replies chan InboundReply

	newMsg    chan struct{} // notified by UnilateralDataHandler when EXISTS arrives
	startedAt time.Time    // only process messages received after this time
}

// NewIMAPClient creates a new IMAPClient. Call Start to begin listening.
func NewIMAPClient(cfg IMAPConfig, log *slog.Logger) *IMAPClient {
	return &IMAPClient{
		cfg:       cfg,
		log:       log,
		Replies:   make(chan InboundReply, 16),
		newMsg:    make(chan struct{}, 1),
		startedAt: time.Now(),
	}
}

// Start connects to the IMAP server and listens for new messages using IDLE.
// It reconnects automatically on failure. Blocks until ctx is cancelled.
func (c *IMAPClient) Start(ctx context.Context) {
	backoff := 5 * time.Second
	for {
		if err := c.run(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Error("IMAP connection lost, reconnecting", "err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 5*time.Minute {
				backoff *= 2
			}
		} else {
			return
		}
	}
}

const pollInterval = 30 * time.Second

func (c *IMAPClient) run(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", c.cfg.Host, c.cfg.Port)

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

	client, err := imapclient.DialTLS(addr, options)
	if err != nil {
		return fmt.Errorf("IMAP dial: %w", err)
	}
	defer client.Close()

	if err := client.Login(c.cfg.Username, c.cfg.Password).Wait(); err != nil {
		return fmt.Errorf("IMAP login: %w", err)
	}
	c.log.Info("IMAP connected", "host", c.cfg.Host)

	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		return fmt.Errorf("IMAP SELECT INBOX: %w", err)
	}

	// Fetch unseen messages that arrived since the relay started (skip old mail).
	if err := c.fetchUnseenSince(ctx, client, c.startedAt); err != nil {
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
	idleCmd, err := client.Idle()
	if err != nil {
		return fmt.Errorf("IMAP IDLE: %w", err)
	}

	idleDone := make(chan error, 1)
	go func() { idleDone <- idleCmd.Wait() }()

	// RFC 2177: re-send IDLE at least every 29 minutes.
	ticker := time.NewTicker(25 * time.Minute)
	defer ticker.Stop()

	for {
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
			// Keepalive: stop and restart IDLE.
			if err := idleCmd.Close(); err != nil {
				return fmt.Errorf("IMAP IDLE close: %w", err)
			}
			<-idleDone
			if err := c.fetchUnseenSince(ctx, client, c.startedAt); err != nil {
				c.log.Warn("keepalive unseen fetch failed", "err", err)
			}
			idleCmd, err = client.Idle()
			if err != nil {
				return fmt.Errorf("IMAP IDLE restart: %w", err)
			}
			idleDone = make(chan error, 1)
			go func() { idleDone <- idleCmd.Wait() }()

		case <-c.newMsg:
			c.log.Info("IMAP: new message notification received (EXISTS)")
			// Server pushed EXISTS: new message arrived. Stop IDLE, fetch, restart.
			if err := idleCmd.Close(); err != nil {
				return fmt.Errorf("IMAP IDLE close on new msg: %w", err)
			}
			<-idleDone
			if err := c.fetchUnseenSince(ctx, client, c.startedAt); err != nil {
				c.log.Warn("post-EXISTS unseen fetch failed", "err", err)
			}
			idleCmd, err = client.Idle()
			if err != nil {
				return fmt.Errorf("IMAP IDLE restart after EXISTS: %w", err)
			}
			idleDone = make(chan error, 1)
			go func() { idleDone <- idleCmd.Wait() }()
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
			if err := c.fetchUnseenSince(ctx, client, c.startedAt); err != nil {
				return fmt.Errorf("poll fetch failed: %w", err)
			}
		}
	}
}

// fetchUnseenSince retrieves UNSEEN messages received on or after the given date,
// parses them, marks them as seen, and dispatches InboundReply values to the Replies channel.
func (c *IMAPClient) fetchUnseenSince(ctx context.Context, client *imapclient.Client, since time.Time) error {
	criteria := &imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
		Since:   since,
	}
	searchData, err := client.Search(criteria, nil).Wait()
	if err != nil {
		return fmt.Errorf("IMAP SEARCH UNSEEN: %w", err)
	}
	if len(searchData.AllSeqNums()) == 0 {
		c.log.Debug("No unseen messages in INBOX")
		return nil
	}
	c.log.Info("Found unseen messages", "count", len(searchData.AllSeqNums()))

	seqSet := imap.SeqSetNum(searchData.AllSeqNums()...)
	fetchOptions := &imap.FetchOptions{
		Envelope: true,
		BodySection: []*imap.FetchItemBodySection{
			{}, // fetch whole message
		},
	}
	messages := client.Fetch(seqSet, fetchOptions)
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
		c.log.Info("Parsed inbound reply", "from", reply.From, "in_reply_to", reply.InReplyTo)
		select {
		case c.Replies <- *reply:
		case <-ctx.Done():
			return nil
		}
	}
	if err := messages.Close(); err != nil {
		return err
	}

	// Mark fetched messages as seen.
	storeFlags := &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagSeen},
	}
	return client.Store(seqSet, storeFlags, nil).Close()
}

// parseMessage extracts an InboundReply from a fetched IMAP message.
// Returns nil, nil if the message is not a reply to one of our outgoing emails.
func (c *IMAPClient) parseMessage(msg *imapclient.FetchMessageData) (*InboundReply, error) {
	var bodyLiteral imap.LiteralReader
	for {
		item := msg.Next()
		if item == nil {
			break
		}
		if bs, ok := item.(imapclient.FetchItemDataBodySection); ok {
			bodyLiteral = bs.Literal
		}
	}
	if bodyLiteral == nil {
		return nil, fmt.Errorf("no body section")
	}

	mr, err := mail.CreateReader(bodyLiteral)
	if err != nil {
		return nil, fmt.Errorf("parsing mail: %w", err)
	}
	header := mr.Header

	subject := header.Get("Subject")
	fromHeader := header.Get("From")

	inReplyTo := strings.TrimSpace(header.Get("In-Reply-To"))
	if inReplyTo == "" {
		c.log.Debug("Skipping non-reply email (no In-Reply-To header)", "from", fromHeader, "subject", subject)
		return nil, nil
	}
	// Only process replies to emails we sent (our Message-IDs contain "gmr-").
	if !strings.Contains(inReplyTo, "gmr-") {
		c.log.Debug("Skipping reply to non-relay email", "in_reply_to", inReplyTo, "from", fromHeader, "subject", subject)
		return nil, nil
	}
	c.log.Debug("Processing relay reply", "in_reply_to", inReplyTo, "from", fromHeader, "subject", subject)

	from, err := header.AddressList("From")
	if err != nil || len(from) == 0 {
		return nil, fmt.Errorf("missing From header")
	}

	reply := &InboundReply{
		InReplyTo: inReplyTo,
		From:      from[0].Address,
		FromName:  from[0].Name,
	}

	maxBytes := int64(c.cfg.MaxAttachmentMB) * 1024 * 1024
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := h.ContentType()
			if strings.HasPrefix(ct, "text/plain") {
				body, _ := io.ReadAll(io.LimitReader(p.Body, 64*1024))
				reply.TextBody = stripQuotedReply(string(body))
			}
		case *mail.AttachmentHeader:
			ct, _, _ := h.ContentType()
			filename, _ := h.Filename()
			data, err := io.ReadAll(io.LimitReader(p.Body, maxBytes+1))
			if err != nil {
				c.log.Warn("reading attachment failed", "filename", filename, "err", err)
				continue
			}
			if int64(len(data)) > maxBytes {
				c.log.Warn("attachment too large, skipping",
					"filename", filename,
					"size_mb", len(data)/1024/1024,
					"max_mb", c.cfg.MaxAttachmentMB)
				continue
			}
			reply.Attachments = append(reply.Attachments, InboundAttachment{
				Filename:    filename,
				ContentType: ct,
				Data:        data,
			})
		}
	}

	return reply, nil
}

// maxReplyChars is the maximum number of characters to send back to Garmin.
// inReach devices support up to 1600 characters per message.
const maxReplyChars = 1600

// stripQuotedReply extracts only the new reply text from an email body,
// removing quoted original messages, signatures, and email client boilerplate.
// This is critical because Garmin inReach messages are limited to 1600 characters
// and sent over satellite.
func stripQuotedReply(body string) string {
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

		// Stop at common signature markers
		if trimmed == "--" || trimmed == "-- " {
			break
		}

		kept = append(kept, line)
	}

	result := strings.TrimSpace(strings.Join(kept, "\n"))

	// Enforce character limit for satellite transmission
	if len(result) > maxReplyChars {
		result = result[:maxReplyChars]
	}

	return result
}

// replyHeader detects "On ... wrote:" patterns used by Gmail and Apple Mail.
func replyHeader(line string) bool {
	lower := strings.ToLower(line)
	if strings.HasPrefix(lower, "on ") && strings.HasSuffix(lower, "wrote:") {
		return true
	}
	// Norwegian variant
	if strings.HasSuffix(lower, "skrev:") && (strings.Contains(lower, " den ") || strings.HasPrefix(lower, "den ")) {
		return true
	}
	return false
}
