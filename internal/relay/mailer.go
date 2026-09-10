package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// smtpTimeout bounds one SMTP session when the caller's context has no
// earlier deadline, so a black-holed server cannot hang the sender.
const smtpTimeout = 60 * time.Second

// Mailer sends emails via SMTP.
type Mailer struct {
	cfg SMTPConfig
}

// NewMailer creates a new Mailer from the given SMTP config.
func NewMailer(cfg SMTPConfig) *Mailer {
	return &Mailer{cfg: cfg}
}

// Attachment is a file to attach to an email.
type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// Message describes an email to be sent.
type Message struct {
	To          []string
	Subject     string
	Body        string
	Attachments []Attachment
	// MessageID is an optional RFC 5322 Message-ID to set on the outgoing email.
	// If empty, the SMTP server assigns one automatically.
	// Format: "<unique-id@domain>" e.g. "<gmr-CONVID-MSGID@garmin-messenger-relay.local>"
	MessageID string
	// InReplyTo, when set, threads this email under the given Message-ID.
	InReplyTo string
}

// Send sends the message to all recipients with a default timeout.
func (m *Mailer) Send(msg Message) error {
	return m.SendContext(context.Background(), msg)
}

// SendContext sends the message to all recipients. The whole SMTP session
// is bounded by ctx's deadline, or smtpTimeout when it has none.
func (m *Mailer) SendContext(ctx context.Context, msg Message) error {
	if len(msg.To) == 0 {
		return fmt.Errorf("no recipients")
	}

	raw, err := m.buildRaw(msg)
	if err != nil {
		return fmt.Errorf("building email: %w", err)
	}

	c, err := m.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	return m.deliver(c, raw, msg.To)
}

// Ping checks that the SMTP server is reachable, negotiates the configured
// encryption and accepts authentication. It does not send any email.
func (m *Mailer) Ping() error {
	c, err := m.connect(context.Background())
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.Quit()
}

// connect dials the SMTP server according to the configured TLS mode and
// authenticates. The caller must Close the returned client. Every read and
// write on the connection is subject to the session deadline.
func (m *Mailer) connect(ctx context.Context) (*smtp.Client, error) {
	addr := fmt.Sprintf("%s:%d", m.cfg.Host, m.cfg.Port)
	tlsCfg := &tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}

	deadline := time.Now().Add(smtpTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := &net.Dialer{Deadline: deadline}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", addr, err)
	}
	_ = conn.SetDeadline(deadline)

	if m.cfg.TLS == "ssl" {
		tconn := tls.Client(conn, tlsCfg)
		if err := tconn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("TLS handshake with %s: %w", addr, err)
		}
		conn = tconn
	}

	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("SMTP client: %w", err)
	}
	if err := c.Hello("localhost"); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("EHLO: %w", err)
	}

	if m.cfg.TLS == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			_ = c.Close()
			return nil, fmt.Errorf("server %s does not offer STARTTLS; refusing to continue unencrypted (set smtp.tls to \"ssl\" or, for a trusted local relay, \"none\")", addr)
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("STARTTLS: %w", err)
		}
	}

	if m.cfg.Username != "" {
		auth := smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
		if err := c.Auth(auth); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("SMTP auth: %w", err)
		}
	}
	return c, nil
}

// deliver sets envelope and writes the message data on an open SMTP client.
func (m *Mailer) deliver(c *smtp.Client, raw []byte, to []string) error {
	from, err := parseAddress(m.cfg.From)
	if err != nil {
		return fmt.Errorf("invalid smtp.from %q: %w", m.cfg.From, err)
	}

	if err := c.Mail(from); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	for _, r := range to {
		addr, err := parseAddress(r)
		if err != nil {
			return fmt.Errorf("invalid recipient %q: %w", r, err)
		}
		if err := c.Rcpt(addr); err != nil {
			return fmt.Errorf("RCPT TO %s: %w", addr, err)
		}
	}

	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := wc.Write(raw); err != nil {
		_ = wc.Close()
		return fmt.Errorf("writing message: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("finishing DATA: %w", err)
	}
	return c.Quit()
}

// buildRaw assembles the raw MIME email bytes.
func (m *Mailer) buildRaw(msg Message) ([]byte, error) {
	var buf bytes.Buffer

	// Headers
	from := m.cfg.From
	if from == "" {
		from = m.cfg.Username
	}

	buf.WriteString("From: " + sanitizeHeader(from) + "\r\n")
	buf.WriteString("To: " + sanitizeHeader(strings.Join(msg.To, ", ")) + "\r\n")
	buf.WriteString("Subject: " + encodeMIMEHeader(sanitizeHeader(msg.Subject)) + "\r\n")
	buf.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	if msg.MessageID != "" {
		buf.WriteString("Message-ID: " + sanitizeHeader(msg.MessageID) + "\r\n")
	}
	if msg.InReplyTo != "" {
		buf.WriteString("In-Reply-To: " + sanitizeHeader(msg.InReplyTo) + "\r\n")
		buf.WriteString("References: " + sanitizeHeader(msg.InReplyTo) + "\r\n")
	}
	buf.WriteString("Auto-Submitted: auto-generated\r\n")
	buf.WriteString("MIME-Version: 1.0\r\n")

	if len(msg.Attachments) == 0 {
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		buf.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
		buf.WriteString("\r\n")
		if err := writeQuotedPrintable(&buf, msg.Body); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	// multipart/mixed for messages with attachments
	mw := multipart.NewWriter(&buf)
	buf.WriteString("Content-Type: multipart/mixed; boundary=\"" + mw.Boundary() + "\"\r\n")
	buf.WriteString("\r\n")

	// Text part
	th := textproto.MIMEHeader{}
	th.Set("Content-Type", "text/plain; charset=utf-8")
	th.Set("Content-Transfer-Encoding", "quoted-printable")
	tw, err := mw.CreatePart(th)
	if err != nil {
		return nil, err
	}
	if err := writeQuotedPrintable(tw, msg.Body); err != nil {
		return nil, err
	}

	// Attachment parts
	for _, a := range msg.Attachments {
		ah := textproto.MIMEHeader{}
		ah.Set("Content-Type", sanitizeHeader(a.ContentType))
		ah.Set("Content-Transfer-Encoding", "base64")
		ah.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, sanitizeFilename(a.Filename)))
		aw, err := mw.CreatePart(ah)
		if err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(a.Data)
		// Wrap at 76 chars per RFC 2045
		for len(enc) > 76 {
			if _, err := aw.Write([]byte(enc[:76] + "\r\n")); err != nil {
				return nil, err
			}
			enc = enc[76:]
		}
		if len(enc) > 0 {
			if _, err := aw.Write([]byte(enc + "\r\n")); err != nil {
				return nil, err
			}
		}
	}

	if err := mw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeQuotedPrintable writes body to w using quoted-printable encoding.
func writeQuotedPrintable(w interface{ Write([]byte) (int, error) }, body string) error {
	qw := quotedprintable.NewWriter(w)
	if _, err := qw.Write([]byte(body)); err != nil {
		return err
	}
	return qw.Close()
}

// sanitizeHeader strips CR and LF so untrusted values (Garmin sender names,
// caption-parsed addresses) can never inject additional email headers.
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// sanitizeFilename keeps a filename safe for a quoted Content-Disposition
// parameter.
func sanitizeFilename(s string) string {
	s = sanitizeHeader(s)
	s = strings.ReplaceAll(s, `"`, "")
	s = strings.ReplaceAll(s, `\`, "")
	if s == "" {
		return "attachment"
	}
	return s
}

// parseAddress extracts the raw email address from a display-name address.
// e.g. "Foo Bar <foo@bar.com>" → "foo@bar.com"
func parseAddress(s string) (string, error) {
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return "", err
	}
	return addr.Address, nil
}

// encodeMIMEHeader encodes a UTF-8 string for use in an email Subject header.
func encodeMIMEHeader(s string) string {
	needsEncoding := false
	for _, r := range s {
		if r > 127 {
			needsEncoding = true
			break
		}
	}
	if !needsEncoding {
		return s
	}
	return mime.QEncoding.Encode("utf-8", s)
}
