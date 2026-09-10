package relay

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakeSMTP is a minimal SMTP server that records one delivery.
type fakeSMTP struct {
	ln       net.Listener
	starttls bool
	mu       sync.Mutex
	from     string
	rcpts    []string
	data     string
}

func newFakeSMTP(t *testing.T, starttls bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, starttls: starttls}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	w("220 fake ESMTP")
	inData := false
	var data strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				f.mu.Lock()
				f.data = data.String()
				f.mu.Unlock()
				w("250 OK")
				continue
			}
			data.WriteString(line + "\n")
			continue
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			if f.starttls {
				w("250-fake")
				w("250 STARTTLS")
			} else {
				w("250 fake")
			}
		case strings.HasPrefix(upper, "HELO"):
			w("250 fake")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			f.mu.Lock()
			f.from = line[len("MAIL FROM:"):]
			f.mu.Unlock()
			w("250 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			f.mu.Lock()
			f.rcpts = append(f.rcpts, line[len("RCPT TO:"):])
			f.mu.Unlock()
			w("250 OK")
		case upper == "DATA":
			inData = true
			w("354 go")
		case upper == "QUIT":
			w("221 bye")
			return
		default:
			w("500 unknown")
		}
	}
}

func TestMailerSendPlain(t *testing.T) {
	srv := newFakeSMTP(t, false)
	m := NewMailer(SMTPConfig{Host: "127.0.0.1", Port: srv.port(), From: "Relay <relay@example.com>", TLS: "none"})
	err := m.Send(Message{
		To:        []string{"Bob <bob@example.com>"},
		Subject:   "Hei på deg\r\nBcc: evil@example.com",
		Body:      "Hallo æøå",
		MessageID: "<gmr-1@garmin-messenger-relay.local>",
		Attachments: []Attachment{
			{Filename: `pic".jpg`, ContentType: "image/jpeg", Data: []byte("jpegdata")},
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.from != "<relay@example.com>" {
		t.Errorf("MAIL FROM = %q", srv.from)
	}
	if len(srv.rcpts) != 1 || srv.rcpts[0] != "<bob@example.com>" {
		t.Errorf("RCPT TO = %v", srv.rcpts)
	}
	if strings.Contains(srv.data, "\nBcc:") {
		t.Error("header injection via Subject was not neutralised")
	}
	for _, want := range []string{
		"Message-ID: <gmr-1@garmin-messenger-relay.local>",
		"Subject: =?utf-8?q?",
		"Auto-Submitted: auto-generated",
		`filename="pic.jpg"`,
		"Content-Type: multipart/mixed",
	} {
		if !strings.Contains(srv.data, want) {
			t.Errorf("email missing %q:\n%s", want, srv.data)
		}
	}
}

func TestMailerRefusesMissingSTARTTLS(t *testing.T) {
	srv := newFakeSMTP(t, false)
	m := NewMailer(SMTPConfig{Host: "127.0.0.1", Port: srv.port(), From: "relay@example.com", TLS: "starttls"})
	err := m.Send(Message{To: []string{"bob@example.com"}, Subject: "x", Body: "y"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("expected STARTTLS refusal, got %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.data != "" {
		t.Fatal("message must not be transmitted without encryption")
	}
	if err := m.Ping(); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("Ping should also refuse: %v", err)
	}
}

func TestBuildRawTextOnlyAndThreading(t *testing.T) {
	m := NewMailer(SMTPConfig{From: "relay@example.com"})
	raw, err := m.buildRaw(Message{To: []string{"a@example.com"}, Subject: "Plain", Body: "hi", InReplyTo: "<abc@x>"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"In-Reply-To: <abc@x>", "References: <abc@x>", "Content-Type: text/plain; charset=utf-8", "\r\n\r\nhi"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}
