package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	gm "github.com/palchrb/garmin-messenger-relay/internal/hermes"
	"github.com/palchrb/garmin-messenger-relay/internal/relay"
)

// Version variables set at build time via -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

const defaultConfig = "config.yaml"

// healthMaxAge is how stale the health file may be before "health" reports
// the relay as unhealthy. The relay refreshes it every minute.
const healthMaxAge = 3 * time.Minute

func main() {
	if len(os.Args) < 2 {
		cmdRun()
		return
	}

	switch os.Args[1] {
	case "login":
		cmdLogin()
	case "logout":
		cmdLogout()
	case "run":
		cmdRun()
	case "status":
		cmdStatus()
	case "health":
		cmdHealth()
	case "test-smtp":
		cmdTestSMTP()
	case "init":
		cmdInit()
	case "version":
		fmt.Printf("garmin-messenger-relay %s (commit %s, built %s)\n", Version, Commit, BuildTime)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Print(`garmin-messenger-relay — forward Garmin Messenger messages to email

Usage:
  garmin-messenger-relay <command> [flags]

Commands:
  init        Write an example config.yaml to the current directory
  login       Authenticate with Garmin Messenger via SMS OTP
  logout      Remove saved Garmin session
  run         Start the email forwarder (default command)
  status      Check Garmin session, SMTP and IMAP connectivity
  health      Exit 0 if a running relay is connected (for Docker HEALTHCHECK)
  test-smtp   Send a test email to verify SMTP settings
  version     Print version information

Flags:
  -config <path>   Path to config file (default: config.yaml, or $GMR_CONFIG)

Environment:
  GMR_CONFIG          Config file path (overridden by -config)
  GMR_SMTP_PASSWORD   SMTP password (overrides smtp.password)
  GMR_IMAP_PASSWORD   IMAP password (overrides imap.password)

Examples:
  garmin-messenger-relay init
  garmin-messenger-relay login
  garmin-messenger-relay run
  garmin-messenger-relay run -config /etc/garmin/config.yaml
  garmin-messenger-relay status
  garmin-messenger-relay test-smtp
`)
}

// configFlag parses -config <path> from os.Args[2:], falling back to the
// GMR_CONFIG environment variable and then config.yaml.
func configFlag() string {
	args := os.Args[2:]
	for i, a := range args {
		if (a == "-config" || a == "--config") && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "-config=") || strings.HasPrefix(a, "--config=") {
			return a[strings.Index(a, "=")+1:]
		}
	}
	if v := os.Getenv(relay.EnvConfigPath); v != "" {
		return v
	}
	return defaultConfig
}

// loadConfig loads and optionally validates the config, exiting with a
// helpful message on failure.
func loadConfig(validate bool) *relay.Config {
	cfgPath := configFlag()
	cfg, err := relay.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n\nRun 'garmin-messenger-relay init' to create a config file.\n", err)
		os.Exit(1)
	}
	if validate {
		if err := cfg.Validate(); err != nil {
			fmt.Fprintf(os.Stderr, "Invalid config (%s): %v\n", cfgPath, err)
			os.Exit(1)
		}
	}
	return cfg
}

// setupLogger configures zerolog (used by main) and slog (used by the relay
// internals) with the same level and output format.
func setupLogger(cfg *relay.Config) {
	level, err := zerolog.ParseLevel(cfg.Log.Level)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	var slogLevel slog.Level
	switch cfg.Log.Level {
	case "trace":
		slogLevel = gm.LevelTrace // verbose HTTP request/response details
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: slogLevel}
	if cfg.Log.Pretty {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
	} else {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, opts)))
	}
}

// cmdInit writes an example config.yaml.
func cmdInit() {
	path := configFlag()
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(os.Stderr, "Error: %s already exists. Remove it first or specify a different path with -config.\n", path)
		os.Exit(1)
	}
	if err := relay.WriteExample(path); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Example config written to %s\n", path)
	fmt.Println("Edit it with your Garmin phone number and SMTP settings, then run:")
	fmt.Println("  garmin-messenger-relay login")
}

// cmdLogin runs the interactive Garmin OTP login flow.
func cmdLogin() {
	cfg := loadConfig(false)
	setupLogger(cfg)

	ctx := context.Background()
	scanner := bufio.NewScanner(os.Stdin)

	// Use phone from config or prompt.
	phone := cfg.Garmin.Phone
	if phone == "" {
		fmt.Print("Phone number (E.164, e.g. +4712345678): ")
		if !scanner.Scan() {
			os.Exit(1)
		}
		phone = strings.TrimSpace(scanner.Text())
	} else {
		fmt.Printf("Logging in as %s (from config)\n", phone)
	}

	sessDir := cfg.Garmin.SessionDir
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating session directory: %v\n", err)
		os.Exit(1)
	}

	auth := gm.NewHermesAuth(gm.WithSessionDir(sessDir))

	fmt.Printf("Requesting SMS code for %s...\n", phone)
	otpReq, err := auth.RequestOTP(ctx, phone, "Garmin Messenger Relay")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error requesting OTP: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("A 6-digit code has been sent to %s via SMS.\n", phone)
	fmt.Print("Enter code: ")
	if !scanner.Scan() {
		os.Exit(1)
	}
	code := strings.TrimSpace(scanner.Text())

	if err := auth.ConfirmOTP(ctx, otpReq, code); err != nil {
		fmt.Fprintf(os.Stderr, "Error confirming OTP: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Login successful! Session saved to", sessDir)
	fmt.Println("Run 'garmin-messenger-relay run' to start forwarding.")
}

// cmdLogout removes the saved Garmin session.
func cmdLogout() {
	cfg := loadConfig(false)
	credsPath := filepath.Join(cfg.Garmin.SessionDir, "hermes_credentials.json")

	if err := os.Remove(credsPath); err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No active session found.")
		} else {
			fmt.Fprintf(os.Stderr, "Error removing session: %v\n", err)
			os.Exit(1)
		}
		return
	}
	fmt.Println("Session removed. Run 'garmin-messenger-relay login' to re-authenticate.")
}

// cmdStatus checks the config, the Garmin session and the mail servers.
func cmdStatus() {
	cfg := loadConfig(false)
	setupLogger(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ok := true

	fmt.Print("Config ... ")
	if err := cfg.Validate(); err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		ok = false
	} else {
		fmt.Println("OK")
	}

	// Check Garmin session
	fmt.Print("Garmin session ... ")
	auth := gm.NewHermesAuth(gm.WithSessionDir(cfg.Garmin.SessionDir))
	if err := auth.Resume(ctx); err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		ok = false
	} else {
		api := gm.NewHermesAPI(auth)
		if _, err := api.GetConversations(ctx, gm.WithLimit(1)); err != nil {
			fmt.Printf("FAIL\n  %v\n", err)
			ok = false
		} else {
			fmt.Println("OK")
		}
	}

	// Check SMTP
	fmt.Print("SMTP connectivity ... ")
	mailer := relay.NewMailer(cfg.SMTP)
	if err := mailer.Ping(); err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		ok = false
	} else {
		fmt.Println("OK")
	}

	// Check IMAP
	fmt.Print("IMAP connectivity ... ")
	if !cfg.IMAPEnabled() {
		fmt.Println("disabled (one-way relay)")
	} else if err := relay.NewIMAPClient(cfg.IMAP, slog.Default(), nil).Check(); err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		ok = false
	} else {
		fmt.Println("OK")
	}

	// Running relay, if any
	fmt.Print("Relay process ... ")
	if h, err := relay.ReadHealth(cfg.Garmin.SessionDir); err != nil {
		fmt.Println("not running (no health file)")
	} else {
		age := time.Since(h.UpdatedAt).Round(time.Second)
		switch {
		case age > healthMaxAge:
			fmt.Printf("stale (last update %s ago)\n", age)
		case h.Connected:
			fmt.Printf("running, connected (updated %s ago)\n", age)
		default:
			fmt.Printf("running, DISCONNECTED (updated %s ago)\n", age)
		}
	}

	// Print forwarding summary
	fmt.Printf("\nForwarding config:\n")
	fmt.Printf("  Default recipients: %v\n", cfg.Forwarding.DefaultRecipients)
	if len(cfg.Forwarding.AllowedSenders) > 0 {
		fmt.Printf("  Allowed senders:    %v\n", cfg.Forwarding.AllowedSenders)
	} else {
		fmt.Printf("  Allowed senders:    anyone (set forwarding.allowed_senders to restrict)\n")
	}
	fmt.Printf("  Caption routing:    %v\n", cfg.Forwarding.CaptionRouting)

	if !ok {
		os.Exit(1)
	}
}

// cmdHealth exits 0 when a running relay reported itself connected recently,
// 1 otherwise. It makes no network calls, so it is cheap enough for a
// Docker HEALTHCHECK or a systemd watchdog script.
func cmdHealth() {
	cfg := loadConfig(false)
	h, err := relay.ReadHealth(cfg.Garmin.SessionDir)
	if err != nil {
		fmt.Printf("unhealthy: %v\n", err)
		os.Exit(1)
	}
	age := time.Since(h.UpdatedAt)
	if age > healthMaxAge {
		fmt.Printf("unhealthy: health file is %s old\n", age.Round(time.Second))
		os.Exit(1)
	}
	if !h.Connected {
		fmt.Println("unhealthy: relay reports SignalR disconnected")
		os.Exit(1)
	}
	fmt.Println("healthy")
}

// cmdTestSMTP sends a test email to verify SMTP settings.
func cmdTestSMTP() {
	cfg := loadConfig(true)
	setupLogger(cfg)

	if len(cfg.Forwarding.DefaultRecipients) == 0 {
		fmt.Fprintln(os.Stderr, "Error: no default_recipients configured")
		os.Exit(1)
	}

	mailer := relay.NewMailer(cfg.SMTP)
	msg := relay.Message{
		To:      cfg.Forwarding.DefaultRecipients,
		Subject: "Garmin Messenger Relay — test email",
		Body: fmt.Sprintf(
			"This is a test email from garmin-messenger-relay %s.\n\n"+
				"If you received this, your SMTP settings are working correctly.\n\n"+
				"Forwarding is configured to send messages from:\n  %s\n\nTo:\n  %s\n",
			Version,
			cfg.Garmin.Phone,
			strings.Join(cfg.Forwarding.DefaultRecipients, "\n  "),
		),
	}

	fmt.Printf("Sending test email to %v ... ", cfg.Forwarding.DefaultRecipients)
	if err := mailer.Send(msg); err != nil {
		fmt.Printf("FAIL\n  %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK")
}

// cmdRun starts the email forwarder.
func cmdRun() {
	cfg := loadConfig(true)
	setupLogger(cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	fwd, err := relay.New(cfg, slog.Default())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating forwarder: %v\n", err)
		os.Exit(1)
	}

	if err := fwd.Resume(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error loading session: %v\n\nRun 'garmin-messenger-relay login' to authenticate.\n", err)
		os.Exit(1)
	}

	log.Info().
		Str("version", Version).
		Str("phone", cfg.Garmin.Phone).
		Strs("recipients", cfg.Forwarding.DefaultRecipients).
		Msg("Starting garmin-messenger-relay")

	if err := fwd.Run(ctx); err != nil {
		// A non-zero exit lets systemd/Docker restart the relay after the
		// Garmin connection has been lost permanently.
		log.Error().Err(err).Msg("Relay exited with error")
		os.Exit(1)
	}

	log.Info().Msg("Shutdown complete")
}
