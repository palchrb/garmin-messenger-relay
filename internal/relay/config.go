package relay

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

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
	// SessionDir is the directory where hermes_credentials.json is stored.
	// Defaults to "./sessions".
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
	// Defaults to "starttls".
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
}

// ForwardingConfig controls where and how messages are forwarded.
type ForwardingConfig struct {
	// DefaultRecipients is the list of email addresses that receive all messages.
	DefaultRecipients []string `yaml:"default_recipients"`
	// CaptionRouting enables parsing email addresses from media captions.
	// When true, email addresses found in a caption are added as recipients.
	CaptionRouting bool `yaml:"caption_routing"`
	// CaptionRoutingReplacesDefault, when true, sends only to caption-parsed
	// addresses instead of merging with DefaultRecipients.
	CaptionRoutingReplacesDefault bool `yaml:"caption_routing_replaces_default"`
	// ForwardText enables forwarding plain text messages.
	ForwardText bool `yaml:"forward_text"`
	// ForwardMedia enables forwarding media attachments (images, audio).
	ForwardMedia bool `yaml:"forward_media"`
	// ForwardLocation enables forwarding location data.
	ForwardLocation bool `yaml:"forward_location"`
}

// LogConfig controls logging output.
type LogConfig struct {
	// Level is the minimum log level: "debug", "info", "warn", "error".
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
			CaptionRouting:                true,
			CaptionRoutingReplacesDefault: false,
			ForwardText:                   true,
			ForwardMedia:                  true,
			ForwardLocation:               true,
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

// Load reads a YAML config file and merges it with defaults.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	// Resolve relative session_dir relative to the config file's directory,
	// so that "./sessions" always lands next to the config file regardless of CWD.
	if cfg.Garmin.SessionDir != "" && !filepath.IsAbs(cfg.Garmin.SessionDir) {
		cfgDir := filepath.Dir(path)
		cfg.Garmin.SessionDir = filepath.Join(cfgDir, cfg.Garmin.SessionDir)
	}

	return &cfg, nil
}

// Validate checks that required config fields are present.
func (c *Config) Validate() error {
	if c.Garmin.Phone == "" {
		return fmt.Errorf("garmin.phone is required")
	}
	if c.SMTP.Host == "" {
		return fmt.Errorf("smtp.host is required")
	}
	if c.SMTP.From == "" {
		return fmt.Errorf("smtp.from is required")
	}
	if c.SMTP.Port == 0 {
		return fmt.Errorf("smtp.port is required")
	}
	if len(c.Forwarding.DefaultRecipients) == 0 && !c.Forwarding.CaptionRouting {
		return fmt.Errorf("forwarding.default_recipients must have at least one entry when caption_routing is false")
	}
	switch c.SMTP.TLS {
	case "none", "starttls", "ssl":
	default:
		return fmt.Errorf("smtp.tls must be one of: none, starttls, ssl (got %q)", c.SMTP.TLS)
	}
	if c.IMAP.Host != "" {
		if c.IMAP.Port == 0 {
			return fmt.Errorf("imap.port is required when imap.host is set")
		}
		if c.IMAP.Username == "" {
			return fmt.Errorf("imap.username is required when imap.host is set")
		}
	}
	return nil
}

// IMAPEnabled reports whether inbound reply handling is configured.
func (c *Config) IMAPEnabled() bool {
	return c.IMAP.Host != ""
}

// WriteExample writes an example config file to path.
func WriteExample(path string) error {
	example := `# garmin-messenger-relay configuration
# Run "garmin-messenger-relay login" to authenticate after editing this file.

garmin:
  # E.164 phone number registered with the Garmin Messenger app.
  phone: "+4712345678"
  # Directory where the Garmin session token is stored.
  session_dir: "./sessions"

smtp:
  host: "smtp.gmail.com"
  port: 587
  username: "you@gmail.com"
  # For Gmail: use an App Password (https://myaccount.google.com/apppasswords).
  password: "your-app-password"
  from: "Garmin Forwarder <you@gmail.com>"
  # tls: none | starttls | ssl
  tls: "starttls"

# IMAP settings for receiving replies (optional but recommended for two-way messaging).
# Use the same account as SMTP. For Gmail, generate an App Password.
imap:
  host: "imap.gmail.com"
  port: 993
  username: "you@gmail.com"
  password: "your-app-password"
  reply_window_days: 7     # ignore replies older than this
  max_attachment_mb: 5     # reject inbound attachments larger than this

forwarding:
  # All incoming messages are forwarded to these addresses.
  default_recipients:
    - "you@example.com"

  # If true, email addresses found in a media caption are also used as recipients.
  # Example: send a photo with caption "check this out admin@example.com" to route
  # that specific message to admin@example.com in addition to default_recipients.
  caption_routing: true

  # If true, caption-parsed addresses REPLACE default_recipients for that message.
  # If false (default), caption addresses are ADDED to default_recipients.
  caption_routing_replaces_default: false

  # Control which message types are forwarded.
  forward_text: true
  forward_media: true
  forward_location: true

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
	return os.WriteFile(path, []byte(example), 0o644)
}
