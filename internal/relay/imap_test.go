package relay

import (
	"strings"
	"testing"
)

var (
	lenient = replyAuthPolicy{authservID: "mx.example.com"}
	strict  = replyAuthPolicy{authservID: "mx.example.com", require: true}
)

const plainReply = "From: Alice <alice@example.com>\r\n" +
	"To: relay@example.com\r\n" +
	"Subject: Re: [InReach] +47 \r\n" +
	"Message-ID: <reply-1@example.com>\r\n" +
	"In-Reply-To: <gmr-a1b2c3d4-e5f6-7890-abcd-ef1234567890-11111111-2222-3333-4444-555555555555-0badf00d@garmin-messenger-relay.local>\r\n" +
	"Authentication-Results: mx.example.com; dkim=pass header.d=example.com; spf=pass\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Ser bra ut, vi sees på hytta!\r\n" +
	"\r\n" +
	"On Mon, Jan 1, 2025 at 10:00 Relay <relay@example.com> wrote:\r\n" +
	"> From: +4712345678\r\n" +
	"> Caption: hello\r\n"

func TestParseReplyPlainText(t *testing.T) {
	r, err := parseReply(strings.NewReader(plainReply), 5<<20, lenient, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if r == nil {
		t.Fatal("expected a reply")
	}
	if r.From != "alice@example.com" || r.FromName != "Alice" {
		t.Errorf("from = %q %q", r.From, r.FromName)
	}
	if r.TextBody != "Ser bra ut, vi sees på hytta!" {
		t.Errorf("text = %q", r.TextBody)
	}
	if r.MessageID != "<reply-1@example.com>" || !r.Authenticated {
		t.Errorf("message id %q authenticated %v", r.MessageID, r.Authenticated)
	}
}

func TestParseReplyForgedAuthResultsIgnored(t *testing.T) {
	// A sender-added Authentication-Results header with a foreign
	// authserv-id must not satisfy strict mode.
	forged := strings.Replace(plainReply, "Authentication-Results: mx.example.com;", "Authentication-Results: attacker.example;", 1)
	if r, _ := parseReply(strings.NewReader(forged), 5<<20, strict, testLogger()); r != nil {
		t.Fatal("forged Authentication-Results accepted")
	}
	// DKIM pass by an unrelated signing domain does not vouch for From.
	other := strings.Replace(plainReply, "dkim=pass header.d=example.com", "dkim=pass header.d=evil.net", 1)
	if r, _ := parseReply(strings.NewReader(other), 5<<20, strict, testLogger()); r != nil {
		t.Fatal("unaligned DKIM pass accepted")
	}
}

func TestParseReplyRequireAuth(t *testing.T) {
	unauth := strings.Replace(plainReply, "Authentication-Results: mx.example.com; dkim=pass header.d=example.com; spf=pass\r\n", "", 1)
	r, err := parseReply(strings.NewReader(unauth), 5<<20, strict, testLogger())
	if err != nil || r != nil {
		t.Fatalf("unauthenticated reply should be dropped in strict mode, got %v %v", r, err)
	}
	r, err = parseReply(strings.NewReader(unauth), 5<<20, lenient, testLogger())
	if err != nil || r == nil {
		t.Fatalf("unauthenticated reply should be accepted in lenient mode, got %v %v", r, err)
	}
	failed := strings.Replace(plainReply, "dkim=pass header.d=example.com; spf=pass", "dkim=fail; dmarc=fail", 1)
	r, err = parseReply(strings.NewReader(failed), 5<<20, lenient, testLogger())
	if err != nil || r != nil {
		t.Fatalf("dmarc=fail must always be rejected, got %v %v", r, err)
	}
}

func TestParseReplySkipsUnrelatedAndAutomatic(t *testing.T) {
	other := strings.Replace(plainReply, "<gmr-a1b2c3d4-e5f6-7890-abcd-ef1234567890-11111111-2222-3333-4444-555555555555-0badf00d@garmin-messenger-relay.local>", "<something-else@example.com>", 1)
	if r, _ := parseReply(strings.NewReader(other), 5<<20, lenient, testLogger()); r != nil {
		t.Fatal("reply to a non-relay email should be ignored")
	}
	auto := strings.Replace(plainReply, "Message-ID:", "Auto-Submitted: auto-replied\r\nMessage-ID:", 1)
	if r, _ := parseReply(strings.NewReader(auto), 5<<20, lenient, testLogger()); r != nil {
		t.Fatal("auto-submitted mail must never become a satellite message")
	}
}

func TestParseReplyHTMLOnlyAndInlineImage(t *testing.T) {
	msg := "From: bob@example.com\r\n" +
		"Subject: Re: x\r\n" +
		"In-Reply-To: <gmr-a1b2c3d4-e5f6-7890-abcd-ef1234567890-11111111-2222-3333-4444-555555555555-0badf00d@garmin-messenger-relay.local>\r\n" +
		"Content-Type: multipart/mixed; boundary=B1\r\n" +
		"\r\n" +
		"--B1\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<html><body><div>Kommer <b>i morgen</b>&nbsp;kl 10</div><br><div class=\"gmail_quote\">On Mon, Bob wrote:<blockquote>old stuff</blockquote></div></body></html>\r\n" +
		"--B1\r\n" +
		"Content-Type: image/jpeg; name=\"IMG_1.jpg\"\r\n" +
		"Content-Disposition: inline; filename=\"IMG_1.jpg\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"anBlZ2RhdGE=\r\n" +
		"--B1\r\n" +
		"Content-Type: application/pdf; name=\"doc.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"doc.pdf\"\r\n" +
		"\r\n" +
		"pdf\r\n" +
		"--B1--\r\n"
	r, err := parseReply(strings.NewReader(msg), 5<<20, lenient, testLogger())
	if err != nil || r == nil {
		t.Fatalf("parse: %v %v", r, err)
	}
	if r.TextBody != "Kommer i morgen kl 10" {
		t.Errorf("html fallback text = %q", r.TextBody)
	}
	if len(r.Attachments) != 1 || r.Attachments[0].Filename != "IMG_1.jpg" || string(r.Attachments[0].Data) != "jpegdata" {
		t.Errorf("inline image not captured: %+v", r.Attachments)
	}
	if len(r.SkippedAttachments) != 1 || !strings.Contains(r.SkippedAttachments[0], "doc.pdf") {
		t.Errorf("unsupported attachment should be reported: %v", r.SkippedAttachments)
	}
}

func TestParseReplyAttachmentTooLarge(t *testing.T) {
	msg := "From: bob@example.com\r\n" +
		"In-Reply-To: <gmr-a1b2c3d4-e5f6-7890-abcd-ef1234567890-11111111-2222-3333-4444-555555555555-0badf00d@garmin-messenger-relay.local>\r\n" +
		"Content-Type: multipart/mixed; boundary=B1\r\n" +
		"\r\n--B1\r\n" +
		"Content-Type: audio/mpeg\r\n" +
		"Content-Disposition: attachment; filename=\"clip.mp3\"\r\n" +
		"\r\n" + strings.Repeat("x", 100) + "\r\n" +
		"--B1--\r\n"
	r, err := parseReply(strings.NewReader(msg), 50, lenient, testLogger())
	if err != nil || r == nil {
		t.Fatalf("parse: %v %v", r, err)
	}
	if len(r.Attachments) != 0 || len(r.SkippedAttachments) != 1 {
		t.Errorf("oversized attachment should be skipped and reported: %+v", r)
	}
}

func TestStripQuotedReply(t *testing.T) {
	cases := map[string]string{
		"Yes\n\nOn Tue, X wrote:\n> quoted":                                  "Yes",
		"Ja\r\n\r\nDen 1. jan 2025 skrev Ola <o@x>:\r\n> sitat":              "Ja",
		"Ok\n________________________________\nFrom: a\nSent: b\nSubject: c": "Ok",
		"Kommer\n\nSendt fra min iPhone":                                     "Kommer",
		"Sure\n-- \nsig":                                                     "Sure",
		"Line 1\nLine 2\n-----Original Message-----\nold":                    "Line 1\nLine 2",
	}
	for in, want := range cases {
		if got := stripQuotedReply(in); got != want {
			t.Errorf("stripQuotedReply(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateRunesKeepsUTF8(t *testing.T) {
	s := strings.Repeat("æ", 10)
	got := truncateRunes(s, 4)
	if got != "ææææ" {
		t.Fatalf("got %q", got)
	}
	if truncateRunes("abc", 10) != "abc" {
		t.Fatal("short strings must be untouched")
	}
}

func TestAuthenticationVerdict(t *testing.T) {
	const from = "alice@mail.example.com"
	cases := []struct {
		name    string
		headers []string
		id      string
		want    authVerdict
	}{
		{"no header", nil, "mx.google.com", authUnknown},
		{"spf only", []string{"mx.google.com; spf=pass"}, "mx.google.com", authUnknown},
		{"dmarc pass trusted", []string{"mx.google.com; dkim=fail; dmarc=pass"}, "mx.google.com", authPass},
		{"dmarc pass foreign id", []string{"attacker.example; dmarc=pass"}, "mx.google.com", authUnknown},
		{"dkim aligned parent domain", []string{"mx.google.com; dkim=pass header.i=@example.com"}, "mx.google.com", authPass},
		{"dkim aligned exact", []string{"mx.google.com; dkim=pass header.d=mail.example.com"}, "mx.google.com", authPass},
		{"dkim unaligned", []string{"mx.google.com; dkim=pass header.d=evil.net"}, "mx.google.com", authUnknown},
		{"dmarc fail trusted", []string{"mx.google.com; dkim=pass header.d=example.com", "mx.google.com; dmarc=fail"}, "mx.google.com", authFail},
		{"dmarc fail foreign id ignored when id configured", []string{"attacker.example; dmarc=fail", "mx.google.com; dmarc=pass"}, "mx.google.com", authPass},
		{"dmarc fail any id when unconfigured", []string{"whatever; dmarc=fail"}, "", authFail},
		{"pass never trusted when unconfigured", []string{"mx.google.com; dmarc=pass"}, "", authUnknown},
		{"subdomain authserv id", []string{"mx1.messagingengine.com; dmarc=pass"}, "messagingengine.com", authPass},
	}
	for _, c := range cases {
		if got := authenticationVerdict(c.headers, c.id, from); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestFirstRelayMessageID(t *testing.T) {
	good := "<gmr-a1b2c3d4-e5f6-7890-abcd-ef1234567890-11111111-2222-3333-4444-555555555555-0badf00d@garmin-messenger-relay.local>"
	if got := firstRelayMessageID("<other@x> " + good); got != good {
		t.Errorf("got %q", got)
	}
	for _, bad := range []string{"<gmr-anything@x>", "<gmr-1@garmin-messenger-relay.local>", ""} {
		if firstRelayMessageID(bad) != "" {
			t.Errorf("%q should not match", bad)
		}
	}
}

func TestParseReplyAttachmentCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("From: bob@example.com\r\nIn-Reply-To: <gmr-a1b2c3d4-e5f6-7890-abcd-ef1234567890-11111111-2222-3333-4444-555555555555-0badf00d@garmin-messenger-relay.local>\r\nContent-Type: multipart/mixed; boundary=B1\r\n\r\n")
	for i := 0; i < maxAttachmentsPerReply+2; i++ {
		b.WriteString("--B1\r\nContent-Type: image/jpeg\r\nContent-Disposition: attachment; filename=\"p.jpg\"\r\n\r\nxx\r\n")
	}
	b.WriteString("--B1--\r\n")
	r, err := parseReply(strings.NewReader(b.String()), 1<<20, lenient, testLogger())
	if err != nil || r == nil {
		t.Fatalf("parse: %v %v", r, err)
	}
	if len(r.Attachments) != maxAttachmentsPerReply || len(r.SkippedAttachments) != 2 {
		t.Fatalf("attachments %d skipped %d", len(r.Attachments), len(r.SkippedAttachments))
	}
}
