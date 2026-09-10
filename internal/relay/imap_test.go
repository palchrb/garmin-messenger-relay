package relay

import (
	"strings"
	"testing"
)

const plainReply = "From: Alice <alice@example.com>\r\n" +
	"To: relay@example.com\r\n" +
	"Subject: Re: [InReach] +47 \r\n" +
	"Message-ID: <reply-1@example.com>\r\n" +
	"In-Reply-To: <gmr-conv-msg-0001@garmin-messenger-relay.local>\r\n" +
	"Authentication-Results: mx.example.com; dkim=pass header.d=example.com; spf=pass\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Ser bra ut, vi sees på hytta!\r\n" +
	"\r\n" +
	"On Mon, Jan 1, 2025 at 10:00 Relay <relay@example.com> wrote:\r\n" +
	"> From: +4712345678\r\n" +
	"> Caption: hello\r\n"

func TestParseReplyPlainText(t *testing.T) {
	r, err := parseReply(strings.NewReader(plainReply), 5<<20, false, testLogger())
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

func TestParseReplyRequireAuth(t *testing.T) {
	unauth := strings.Replace(plainReply, "Authentication-Results: mx.example.com; dkim=pass header.d=example.com; spf=pass\r\n", "", 1)
	r, err := parseReply(strings.NewReader(unauth), 5<<20, true, testLogger())
	if err != nil || r != nil {
		t.Fatalf("unauthenticated reply should be dropped in strict mode, got %v %v", r, err)
	}
	r, err = parseReply(strings.NewReader(unauth), 5<<20, false, testLogger())
	if err != nil || r == nil {
		t.Fatalf("unauthenticated reply should be accepted in lenient mode, got %v %v", r, err)
	}
	failed := strings.Replace(plainReply, "dkim=pass header.d=example.com; spf=pass", "dkim=fail; dmarc=fail", 1)
	r, err = parseReply(strings.NewReader(failed), 5<<20, false, testLogger())
	if err != nil || r != nil {
		t.Fatalf("dmarc=fail must always be rejected, got %v %v", r, err)
	}
}

func TestParseReplySkipsUnrelatedAndAutomatic(t *testing.T) {
	other := strings.Replace(plainReply, "<gmr-conv-msg-0001@garmin-messenger-relay.local>", "<something-else@example.com>", 1)
	if r, _ := parseReply(strings.NewReader(other), 5<<20, false, testLogger()); r != nil {
		t.Fatal("reply to a non-relay email should be ignored")
	}
	auto := strings.Replace(plainReply, "Message-ID:", "Auto-Submitted: auto-replied\r\nMessage-ID:", 1)
	if r, _ := parseReply(strings.NewReader(auto), 5<<20, false, testLogger()); r != nil {
		t.Fatal("auto-submitted mail must never become a satellite message")
	}
}

func TestParseReplyHTMLOnlyAndInlineImage(t *testing.T) {
	msg := "From: bob@example.com\r\n" +
		"Subject: Re: x\r\n" +
		"In-Reply-To: <gmr-1@garmin-messenger-relay.local>\r\n" +
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
	r, err := parseReply(strings.NewReader(msg), 5<<20, false, testLogger())
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
		"In-Reply-To: <gmr-1@garmin-messenger-relay.local>\r\n" +
		"Content-Type: multipart/mixed; boundary=B1\r\n" +
		"\r\n--B1\r\n" +
		"Content-Type: audio/mpeg\r\n" +
		"Content-Disposition: attachment; filename=\"clip.mp3\"\r\n" +
		"\r\n" + strings.Repeat("x", 100) + "\r\n" +
		"--B1--\r\n"
	r, err := parseReply(strings.NewReader(msg), 50, false, testLogger())
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
	if authenticationVerdict(nil) != authUnknown {
		t.Error("no header should be unknown")
	}
	if authenticationVerdict([]string{"mx; spf=pass"}) != authUnknown {
		t.Error("spf alone is not enough")
	}
	if authenticationVerdict([]string{"mx; dkim=pass"}) != authPass {
		t.Error("dkim=pass should pass")
	}
	if authenticationVerdict([]string{"mx; dkim=pass", "mx; dmarc=fail"}) != authFail {
		t.Error("dmarc=fail should win")
	}
}
