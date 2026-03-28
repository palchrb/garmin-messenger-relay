package relay

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

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
}

// Send sends the message to all recipients.
func (m *Mailer) Send(msg Message) error {
	if len(msg.To) == 0 {
		return fmt.Errorf("no recipients")
	}

	raw, err := m.buildRaw(msg)
	if err != nil {
		return fmt.Errorf("building email: %w", err)
	}

	addr := fmt.Sprintf("%s:%d", m.cfg.Host, m.cfg.Port)

	switch m.cfg.TLS {
	case "ssl":
		return m.sendSSL(addr, raw, msg.To)
	case "starttls":
		return m.sendStartTLS(addr, raw, msg.To)
	default: // "none"
		return m.sendPlain(addr, raw, msg.To)
	}
}

// sendStartTLS connects plain and upgrades to TLS via STARTTLS (port 587).
func (m *Mailer) sendStartTLS(addr string, raw []byte, to []string) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", addr, err)
	}
	defer c.Close()

	if err := c.Hello("localhost"); err != nil {
		return err
	}

	tlsCfg := &tls.Config{ServerName: m.cfg.Host}
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}

	if m.cfg.Username != "" {
		auth := smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}

	return m.deliver(c, raw, to)
}

// sendSSL connects directly over TLS (port 465).
func (m *Mailer) sendSSL(addr string, raw []byte, to []string) error {
	tlsCfg := &tls.Config{ServerName: m.cfg.Host}
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("TLS dial %s: %w", addr, err)
	}

	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		return fmt.Errorf("SMTP client: %w", err)
	}
	defer c.Close()

	if m.cfg.Username != "" {
		auth := smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}

	return m.deliver(c, raw, to)
}

// sendPlain connects with no encryption (port 25).
func (m *Mailer) sendPlain(addr string, raw []byte, to []string) error {
	var auth smtp.Auth
	if m.cfg.Username != "" {
		auth = smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
	}
	from, _ := parseAddress(m.cfg.From)
	return smtp.SendMail(addr, auth, from, to, raw)
}

// deliver sets envelope and writes the message data on an open SMTP client.
func (m *Mailer) deliver(c *smtp.Client, raw []byte, to []string) error {
	from, _ := parseAddress(m.cfg.From)

	if err := c.Mail(from); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	for _, r := range to {
		addr, _ := parseAddress(r)
		if err := c.Rcpt(addr); err != nil {
			return fmt.Errorf("RCPT TO %s: %w", r, err)
		}
	}

	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := wc.Write(raw); err != nil {
		return fmt.Errorf("writing message: %w", err)
	}
	return wc.Close()
}

// Ping checks that the SMTP server is reachable and accepts authentication.
// It does not send any email.
func (m *Mailer) Ping() error {
	addr := fmt.Sprintf("%s:%d", m.cfg.Host, m.cfg.Port)

	switch m.cfg.TLS {
	case "ssl":
		tlsCfg := &tls.Config{ServerName: m.cfg.Host}
		conn, err := tls.Dial("tcp", addr, tlsCfg)
		if err != nil {
			return fmt.Errorf("TLS dial %s: %w", addr, err)
		}
		c, err := smtp.NewClient(conn, m.cfg.Host)
		if err != nil {
			return fmt.Errorf("SMTP client: %w", err)
		}
		defer c.Close()
		if m.cfg.Username != "" {
			auth := smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
			if err := c.Auth(auth); err != nil {
				return fmt.Errorf("SMTP auth: %w", err)
			}
		}
		return nil

	default: // starttls or none
		c, err := smtp.Dial(addr)
		if err != nil {
			return fmt.Errorf("dialing %s: %w", addr, err)
		}
		defer c.Close()
		if m.cfg.TLS == "starttls" {
			tlsCfg := &tls.Config{ServerName: m.cfg.Host}
			if ok, _ := c.Extension("STARTTLS"); ok {
				if err := c.StartTLS(tlsCfg); err != nil {
					return fmt.Errorf("STARTTLS: %w", err)
				}
			}
		}
		if m.cfg.Username != "" {
			auth := smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
			if err := c.Auth(auth); err != nil {
				return fmt.Errorf("SMTP auth: %w", err)
			}
		}
		return nil
	}
}

// buildRaw assembles the raw MIME email bytes.
func (m *Mailer) buildRaw(msg Message) ([]byte, error) {
	var buf bytes.Buffer

	// Headers
	from := m.cfg.From
	if from == "" {
		from = m.cfg.Username
	}

	buf.WriteString("From: " + from + "\r\n")
	buf.WriteString("To: " + strings.Join(msg.To, ", ") + "\r\n")
	buf.WriteString("Subject: " + encodeMIMEHeader(msg.Subject) + "\r\n")
	buf.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	if msg.MessageID != "" {
		buf.WriteString("Message-ID: " + msg.MessageID + "\r\n")
	}
	buf.WriteString("MIME-Version: 1.0\r\n")

	if len(msg.Attachments) == 0 {
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		buf.WriteString("\r\n")
		buf.WriteString(msg.Body)
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
	if _, err := tw.Write([]byte(msg.Body)); err != nil {
		return nil, err
	}

	// Attachment parts
	for _, a := range msg.Attachments {
		ah := textproto.MIMEHeader{}
		ah.Set("Content-Type", a.ContentType)
		ah.Set("Content-Transfer-Encoding", "base64")
		ah.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, a.Filename))
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

	mw.Close()
	return buf.Bytes(), nil
}

// parseAddress extracts the raw email address from a display-name address.
// e.g. "Foo Bar <foo@bar.com>" → "foo@bar.com"
func parseAddress(s string) (string, error) {
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return s, err
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
