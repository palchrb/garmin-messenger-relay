package relay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExampleConfigIsValid(t *testing.T) {
	path := writeConfig(t, ExampleConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("example config should validate: %v", err)
	}
	if cfg.Garmin.SessionDir != filepath.Join(filepath.Dir(path), "sessions") {
		t.Errorf("session_dir not resolved relative to config: %q", cfg.Garmin.SessionDir)
	}
	if cfg.Forwarding.CaptionRouting {
		t.Error("caption_routing should default to off in the example")
	}
}

func TestExampleConfigFileMatchesEmbedded(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Skipf("config.example.yaml not found: %v", err)
	}
	if string(data) != ExampleConfig {
		t.Fatal("config.example.yaml differs from relay.ExampleConfig; regenerate it")
	}
}

func TestLoadEnvOverridesAndDefaults(t *testing.T) {
	t.Setenv(EnvSMTPPassword, "smtp-secret")
	t.Setenv(EnvIMAPPassword, "imap-secret")
	path := writeConfig(t, `
garmin:
  phone: "+4712345678"
smtp:
  host: smtp.example.com
  from: relay@example.com
  username: relay@example.com
  password: from-file
imap:
  host: imap.example.com
  username: relay@example.com
forwarding:
  default_recipients: ["a@example.com"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.Password != "smtp-secret" || cfg.IMAP.Password != "imap-secret" {
		t.Errorf("env overrides not applied: %+v %+v", cfg.SMTP, cfg.IMAP)
	}
	if cfg.SMTP.Port != 587 || cfg.IMAP.Port != 993 || cfg.IMAP.ReplyWindowDays != 7 {
		t.Errorf("defaults not applied: smtp=%d imap=%d window=%d", cfg.SMTP.Port, cfg.IMAP.Port, cfg.IMAP.ReplyWindowDays)
	}
	if cfg.Forwarding.CatchupIntervalMinutes != 10 {
		t.Errorf("catchup interval default = %d", cfg.Forwarding.CatchupIntervalMinutes)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := writeConfig(t, "garmin:\n  phone: \"+4712345678\"\n  phonee: x\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "phonee") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestValidateErrors(t *testing.T) {
	base := func() *Config {
		cfg := DefaultConfig()
		cfg.Garmin.Phone = "+4712345678"
		cfg.SMTP.Host = "smtp.example.com"
		cfg.SMTP.From = "Relay <relay@example.com>"
		cfg.Forwarding.DefaultRecipients = []string{"a@example.com"}
		return &cfg
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"ok", func(*Config) {}, ""},
		{"bad phone", func(c *Config) { c.Garmin.Phone = "4712345678" }, "E.164"},
		{"bad port", func(c *Config) { c.SMTP.Port = 70000 }, "smtp.port"},
		{"bad from", func(c *Config) { c.SMTP.From = "not an address" }, "smtp.from"},
		{"bad tls", func(c *Config) { c.SMTP.TLS = "tls" }, "smtp.tls"},
		{"plain auth", func(c *Config) { c.SMTP.TLS = "none"; c.SMTP.Username = "u" }, "unencrypted"},
		{"plain auth localhost ok", func(c *Config) { c.SMTP.TLS = "none"; c.SMTP.Username = "u"; c.SMTP.Host = "localhost" }, ""},
		{"no recipients", func(c *Config) { c.Forwarding.DefaultRecipients = nil }, "default_recipients"},
		{"caption routing without recipients ok", func(c *Config) { c.Forwarding.DefaultRecipients = nil; c.Forwarding.CaptionRouting = true }, ""},
		{"bad recipient", func(c *Config) { c.Forwarding.DefaultRecipients = []string{"nope"} }, "default_recipients"},
		{"bad allowed sender", func(c *Config) { c.Forwarding.AllowedSenders = []string{"12345"} }, "allowed_senders"},
		{"bad allowlist", func(c *Config) { c.Forwarding.CaptionRoutingAllowlist = []string{"@x"} }, "caption_routing_allowlist"},
		{"good allowlist", func(c *Config) { c.Forwarding.CaptionRoutingAllowlist = []string{"@example.com", "b@example.org"} }, ""},
		{"bad contact", func(c *Config) { c.Forwarding.Contacts = map[string]string{"ola": "Ola"} }, "contacts"},
		{"negative catchup", func(c *Config) { c.Forwarding.CatchupIntervalMinutes = -1 }, "catchup_interval_minutes"},
		{"imap missing password", func(c *Config) { c.IMAP.Host = "imap.example.com"; c.IMAP.Username = "u" }, "imap.password"},
		{"imap bad window", func(c *Config) {
			c.IMAP.Host = "imap.example.com"
			c.IMAP.Username = "u"
			c.IMAP.Password = "p"
			c.IMAP.ReplyWindowDays = 0
		}, "reply_window_days"},
		{"bad alert email", func(c *Config) { c.Alerts.Email = "x" }, "alerts.email"},
		{"bad log level", func(c *Config) { c.Log.Level = "verbose" }, "log.level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestForwardingHelpers(t *testing.T) {
	f := ForwardingConfig{
		AllowedSenders:          []string{"+4711111111"},
		CaptionRoutingAllowlist: []string{"@example.com", "friend@other.org"},
		Contacts:                map[string]string{"+4711111111": "Ola"},
	}
	if !f.SenderAllowed("+4711111111") || f.SenderAllowed("+4722222222") {
		t.Error("SenderAllowed wrong")
	}
	if empty := (ForwardingConfig{}); !empty.SenderAllowed("+4799999999") {
		t.Error("empty allowed_senders should allow everyone")
	}
	if !f.CaptionAddressAllowed("Bob@Example.com") || !f.CaptionAddressAllowed("friend@other.org") || f.CaptionAddressAllowed("x@evil.com") {
		t.Error("CaptionAddressAllowed wrong")
	}
	if f.DisplayName("+4711111111") != "Ola" || f.DisplayName("+4722222222") != "+4722222222" {
		t.Error("DisplayName wrong")
	}
}

func TestWriteExamplePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteExample(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("config should be owner-only, got %o", info.Mode().Perm())
	}
}
