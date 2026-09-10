package relay

import (
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Environment variables that override secrets in the config file, so that
// passwords can be injected via Docker/Kubernetes secrets instead of being
// stored in config.yaml.
const (
	EnvSMTPPassword = "GMR_SMTP_PASSWORD"
	EnvIMAPPassword = "GMR_IMAP_PASSWORD"
	// EnvConfigPath overrides the default config file path.
	EnvConfigPath = "GMR_CONFIG"
)

// e164Regexp matches E.164 phone numbers such as "+4712345678".
var e164Regexp = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// Config is the top-level configuration for garmin-messenger-relay.
type Config struct {
	Garmin     GarminConfig     `yaml:"garmin"`
	SMTP       SMTPConfig       `yaml:"smtp"`
	IMAP       IMAPConfig       `yaml:"imap"`
	Forwarding ForwardingConfig `yaml:"forwarding"`
	Alerts     AlertsConfig     `yaml:"alerts"`
	Log        LogConfig        `yaml:"log"`
}

// GarminConfig holds Garmin Messenger credentials and session storage.
type GarminConfig struct {
	// Phone is the E.164 phone number registered with Garmin Messenger, e.g. "+4712345678".
	Phone string `yaml:"phone"`
	// SessionDir is the directory where hermes_credentials.json and relay
	// state are stored. Defaults to "./sessions" next to the config file.
	SessionDir string `yaml:"session_dir"`
}

// SMTPConfig holds outgoing mail server settings.
type SMTPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// From is the sender address, e.g. "Garmin Forwarder <garmin@example.com>".
	From string `yaml:"from"`
	// TLS controls the connection security: "none", "starttls", or "ssl".
	// Defaults to "starttls". With "starttls" the relay refuses to continue
	// if the server does not offer STARTTLS.
	TLS string `yaml:"tls"`
}

// IMAPConfig holds inbound mail settings for receiving replies.
type IMAPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// ReplyWindowDays is how many days old a reply can be before it is ignored.
	// Defaults to 7.
	ReplyWindowDays int `yaml:"reply_window_days"`
	// MaxAttachmentMB is the maximum size in MB of an inbound media attachment.
	// Defaults to 5.
	MaxAttachmentMB int `yaml:"max_attachment_mb"`
	// RequireAuthenticatedReplies, when true, only accepts replies whose
	// Authentication-Results header (added by your mail provider) shows
	// dkim=pass or dmarc=pass. This defeats From-header spoofing. When false
	// (default) replies are still rejected if the header reports dmarc=fail.
	RequireAuthenticatedReplies bool `yaml:"require_authenticated_replies"`
}

// ForwardingConfig controls where and how messages are forwarded.
type ForwardingConfig struct {
	// DefaultRecipients is the list of email addresses that receive all messages.
	DefaultRecipients []string `yaml:"default_recipients"`
	// AllowedSenders restricts forwarding to Garmin messages from these E.164
	// phone numbers. Empty means messages from anyone are forwarded.
	AllowedSenders []string `yaml:"allowed_senders"`
	// CaptionRouting enables parsing email addresses from media captions.
	// When true, email addresses found in a caption are added as recipients.
	CaptionRouting bool `yaml:"caption_routing"`
	// CaptionRoutingAllowlist restricts which addresses caption routing may
	// deliver to. Entries are full addresses ("bob@example.com") or domains
	// ("@example.com"). Empty means any address found in a caption is used.
	CaptionRoutingAllowlist []string `yaml:"caption_routing_allowlist"`
	// CaptionRoutingReplacesDefault, when true, sends only to caption-parsed
	// addresses instead of merging with DefaultRecipients.
	CaptionRoutingReplacesDefault bool `yaml:"caption_routing_replaces_default"`
	// ForwardText enables forwarding plain text messages.
	ForwardText bool `yaml:"forward_text"`
	// ForwardMedia enables forwarding media attachments (images, audio).
	ForwardMedia bool `yaml:"forward_media"`
	// ForwardLocation enables forwarding location data.
	ForwardLocation bool `yaml:"forward_location"`
	// Contacts maps E.164 phone numbers to display names used in emails.
	Contacts map[string]string `yaml:"contacts"`
	// CatchupIntervalMinutes is how often the relay polls Garmin for messages
	// it may have missed (in addition to the catch-up run on every
	// reconnect). 0 disables the periodic poll. Defaults to 10.
	CatchupIntervalMinutes int `yaml:"catchup_interval_minutes"`
}

// LogConfig controls logging output.
type LogConfig struct {
	// Level is the minimum log level: "trace", "debug", "info", "warn", "error".
	// Defaults to "info".
	Level string `yaml:"level"`
	// Pretty enables human-readable log output instead of JSON.
	Pretty bool `yaml:"pretty"`
}

// AlertsConfig controls email notifications for operational issues.
type AlertsConfig struct {
	// Email is the address to send alert notifications to.
	// Leave empty to disable alerts.
	Email string `yaml:"email"`
	// CooldownMinutes is the minimum time between repeated alerts of the same type.
	// Defaults to 30.
	CooldownMinutes int `yaml:"cooldown_minutes"`
}

// AlertsEnabled reports whether alert notifications are configured.
func (c *Config) AlertsEnabled() bool {
	return c.Alerts.Email != ""
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Garmin: GarminConfig{
			SessionDir: "./sessions",
		},
		SMTP: SMTPConfig{
			Port: 587,
			TLS:  "starttls",
		},
		Forwarding: ForwardingConfig{
			CaptionRouting:                false,
			CaptionRoutingReplacesDefault: false,
			ForwardText:                   true,
			ForwardMedia:                  true,
			ForwardLocation:               true,
			CatchupIntervalMinutes:        10,
		},
		IMAP: IMAPConfig{
			Port:            993,
			ReplyWindowDays: 7,
			MaxAttachmentMB: 5,
		},
		Alerts: AlertsConfig{
			CooldownMinutes: 30,
		},
		Log: LogConfig{
			Level:  "info",
			Pretty: false,
		},
	}
}

// Load reads a YAML config file, merges it with defaults and applies
// environment variable overrides for secrets.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	if v := os.Getenv(EnvSMTPPassword); v != "" {
		cfg.SMTP.Password = v
	}
	if v := os.Getenv(EnvIMAPPassword); v != "" {
		cfg.IMAP.Password = v
	}

	// Resolve relative session_dir relative to the config file's directory,
	// so that "./sessions" always lands next to the config file regardless of CWD.
	if cfg.Garmin.SessionDir == "" {
		cfg.Garmin.SessionDir = "./sessions"
	}
	if !filepath.IsAbs(cfg.Garmin.SessionDir) {
		cfg.Garmin.SessionDir = filepath.Join(filepath.Dir(path), cfg.Garmin.SessionDir)
	}

	normalize(&cfg)
	return &cfg, nil
}

// normalize trims whitespace and lower-cases values that are compared
// case-insensitively elsewhere.
func normalize(cfg *Config) {
	cfg.Garmin.Phone = strings.TrimSpace(cfg.Garmin.Phone)
	cfg.SMTP.TLS = strings.ToLower(strings.TrimSpace(cfg.SMTP.TLS))
	cfg.Log.Level = strings.ToLower(strings.TrimSpace(cfg.Log.Level))
	for i, r := range cfg.Forwarding.DefaultRecipients {
		cfg.Forwarding.DefaultRecipients[i] = strings.TrimSpace(r)
	}
	for i, s := range cfg.Forwarding.AllowedSenders {
		cfg.Forwarding.AllowedSenders[i] = strings.TrimSpace(s)
	}
	for i, a := range cfg.Forwarding.CaptionRoutingAllowlist {
		cfg.Forwarding.CaptionRoutingAllowlist[i] = strings.ToLower(strings.TrimSpace(a))
	}
}

// Validate checks that the configuration is complete and well-formed.
// It returns the first problem found with a message that names the field.
func (c *Config) Validate() error {
	if c.Garmin.Phone == "" {
		return fmt.Errorf("garmin.phone is required")
	}
	if !e164Regexp.MatchString(c.Garmin.Phone) {
		return fmt.Errorf("garmin.phone must be in E.164 format, e.g. +4712345678 (got %q)", c.Garmin.Phone)
	}

	if c.SMTP.Host == "" {
		return fmt.Errorf("smtp.host is required")
	}
	if err := validPort("smtp.port", c.SMTP.Port); err != nil {
		return err
	}
	if c.SMTP.From == "" {
		return fmt.Errorf("smtp.from is required")
	}
	if _, err := mail.ParseAddress(c.SMTP.From); err != nil {
		return fmt.Errorf("smtp.from is not a valid address (got %q): %w", c.SMTP.From, err)
	}
	switch c.SMTP.TLS {
	case "none", "starttls", "ssl":
	default:
		return fmt.Errorf("smtp.tls must be one of: none, starttls, ssl (got %q)", c.SMTP.TLS)
	}
	if c.SMTP.TLS == "none" && c.SMTP.Username != "" && !isLocalhost(c.SMTP.Host) {
		return fmt.Errorf("smtp.tls=none cannot be combined with a username: passwords are never sent over an unencrypted connection (use starttls or ssl)")
	}

	if len(c.Forwarding.DefaultRecipients) == 0 && !c.Forwarding.CaptionRouting {
		return fmt.Errorf("forwarding.default_recipients must have at least one entry when caption_routing is false")
	}
	for _, r := range c.Forwarding.DefaultRecipients {
		if _, err := mail.ParseAddress(r); err != nil {
			return fmt.Errorf("forwarding.default_recipients contains an invalid address %q: %w", r, err)
		}
	}
	for _, s := range c.Forwarding.AllowedSenders {
		if !e164Regexp.MatchString(s) {
			return fmt.Errorf("forwarding.allowed_senders entry %q must be in E.164 format", s)
		}
	}
	for _, a := range c.Forwarding.CaptionRoutingAllowlist {
		if strings.HasPrefix(a, "@") {
			if len(a) < 4 || !strings.Contains(a[1:], ".") {
				return fmt.Errorf("forwarding.caption_routing_allowlist entry %q is not a valid domain", a)
			}
			continue
		}
		if _, err := mail.ParseAddress(a); err != nil {
			return fmt.Errorf("forwarding.caption_routing_allowlist entry %q must be an address or @domain: %w", a, err)
		}
	}
	if c.Forwarding.CatchupIntervalMinutes < 0 {
		return fmt.Errorf("forwarding.catchup_interval_minutes must not be negative")
	}
	for phone := range c.Forwarding.Contacts {
		if !e164Regexp.MatchString(phone) {
			return fmt.Errorf("forwarding.contacts key %q must be in E.164 format", phone)
		}
	}

	if c.IMAP.Host != "" {
		if err := validPort("imap.port", c.IMAP.Port); err != nil {
			return err
		}
		if c.IMAP.Username == "" {
			return fmt.Errorf("imap.username is required when imap.host is set")
		}
		if c.IMAP.Password == "" {
			return fmt.Errorf("imap.password is required when imap.host is set (or set %s)", EnvIMAPPassword)
		}
		if c.IMAP.ReplyWindowDays <= 0 {
			return fmt.Errorf("imap.reply_window_days must be greater than 0 (got %d)", c.IMAP.ReplyWindowDays)
		}
		if c.IMAP.MaxAttachmentMB <= 0 {
			return fmt.Errorf("imap.max_attachment_mb must be greater than 0 (got %d)", c.IMAP.MaxAttachmentMB)
		}
	}

	if c.Alerts.Email != "" {
		if _, err := mail.ParseAddress(c.Alerts.Email); err != nil {
			return fmt.Errorf("alerts.email is not a valid address (got %q): %w", c.Alerts.Email, err)
		}
		if c.Alerts.CooldownMinutes < 0 {
			return fmt.Errorf("alerts.cooldown_minutes must not be negative")
		}
	}

	switch c.Log.Level {
	case "trace", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level must be one of: trace, debug, info, warn, error (got %q)", c.Log.Level)
	}
	return nil
}

func validPort(field string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("%s must be between 1 and 65535 (got %d)", field, port)
	}
	return nil
}

func isLocalhost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// IMAPEnabled reports whether inbound reply handling is configured.
func (c *Config) IMAPEnabled() bool {
	return c.IMAP.Host != ""
}

// SenderAllowed reports whether messages from the given phone number should
// be forwarded.
func (c *ForwardingConfig) SenderAllowed(phone string) bool {
	if len(c.AllowedSenders) == 0 {
		return true
	}
	for _, s := range c.AllowedSenders {
		if s == phone {
			return true
		}
	}
	return false
}

// CaptionAddressAllowed reports whether caption routing may deliver to addr.
func (c *ForwardingConfig) CaptionAddressAllowed(addr string) bool {
	if len(c.CaptionRoutingAllowlist) == 0 {
		return true
	}
	addr = strings.ToLower(addr)
	for _, entry := range c.CaptionRoutingAllowlist {
		if strings.HasPrefix(entry, "@") {
			if strings.HasSuffix(addr, entry) {
				return true
			}
			continue
		}
		if entry == addr {
			return true
		}
	}
	return false
}

// DisplayName returns the configured contact name for a phone number, or the
// number itself when no contact is configured.
func (c *ForwardingConfig) DisplayName(phone string) string {
	if name, ok := c.Contacts[phone]; ok && name != "" {
		return name
	}
	return phone
}

// ExampleConfig is the annotated example configuration written by "init".
const ExampleConfig = `# garmin-messenger-relay configuration
# Run "garmin-messenger-relay login" to authenticate after editing this file.
#
# Secrets can be kept out of this file by setting the environment variables
# GMR_SMTP_PASSWORD and GMR_IMAP_PASSWORD instead.

garmin:
  # E.164 phone number registered with the Garmin Messenger app.
  phone: "+4712345678"
  # Directory where the Garmin session token and relay state are stored.
  # Relative paths are resolved next to this config file.
  session_dir: "./sessions"

smtp:
  host: "smtp.gmail.com"
  port: 587
  username: "you@gmail.com"
  # For Gmail: use an App Password (https://myaccount.google.com/apppasswords).
  password: "your-app-password"
  from: "Garmin Forwarder <you@gmail.com>"
  # tls: starttls | ssl | none. With starttls the relay refuses to send if the
  # server does not offer encryption.
  tls: "starttls"

# IMAP settings for receiving replies (optional but recommended for two-way messaging).
# Remove or leave host empty to run one-way (Garmin -> email only).
#
# The relay only touches emails that are replies to messages it sent itself;
# other mail in the inbox is left untouched and unread.
imap:
  host: "imap.gmail.com"
  port: 993
  username: "you@gmail.com"
  password: "your-app-password"
  reply_window_days: 7     # ignore replies to relay emails older than this
  max_attachment_mb: 5     # reject inbound attachments larger than this
  # Only accept replies your mail provider has verified (DKIM/DMARC pass).
  # Recommended when the reply address is not under your control.
  require_authenticated_replies: false

forwarding:
  # All incoming messages are forwarded to these addresses.
  default_recipients:
    - "you@example.com"

  # Only forward messages from these phone numbers (E.164). Anyone who knows
  # your Garmin number can message it, so restrict this if the relay should
  # only serve your own devices. Empty = forward from anyone.
  allowed_senders: []
  #  - "+4712345678"

  # Names shown in emails instead of raw phone numbers.
  contacts: {}
  #  "+4712345678": "Ola"

  # If true, email addresses found in a media caption are also used as recipients.
  # Example: send a photo with caption "check this out admin@example.com" to route
  # that specific message to admin@example.com in addition to default_recipients.
  # Anyone allowed to send to the relay can make it email these addresses, so
  # combine with allowed_senders and/or caption_routing_allowlist.
  caption_routing: false

  # Restrict caption routing to these addresses or domains. Empty = any address.
  caption_routing_allowlist: []
  #  - "@example.com"
  #  - "friend@other.com"

  # If true, caption-parsed addresses REPLACE default_recipients for that message.
  # If false (default), caption addresses are ADDED to default_recipients.
  caption_routing_replaces_default: false

  # Control which message types are forwarded.
  forward_text: true
  forward_media: true
  forward_location: true

  # How often (minutes) to poll Garmin for messages missed while offline, in
  # addition to the check done on every reconnect. 0 disables polling.
  catchup_interval_minutes: 10

log:
  # level: trace | debug | info | warn | error
  level: "info"
  # pretty: true for human-readable output, false for JSON (better for log aggregators).
  pretty: true

# Optional: receive email alerts when something goes wrong (Garmin disconnected,
# IMAP auth failure, session expired). Leave empty to disable.
# alerts:
#   email: "admin@example.com"
#   cooldown_minutes: 30
`

// WriteExample writes the example config file to path with owner-only
// permissions, since the file will hold passwords.
func WriteExample(path string) error {
	return os.WriteFile(path, []byte(ExampleConfig), 0o600)
}
