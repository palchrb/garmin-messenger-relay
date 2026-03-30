package relay

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// AlertType identifies a category of operational alert.
type AlertType string

const (
	AlertSignalRDisconnect AlertType = "signalr_disconnect"
	AlertIMAPAuthFailure   AlertType = "imap_auth_failure"
	AlertSessionExpired    AlertType = "session_expired"
)

// Alerter sends email notifications for operational issues with cooldown
// to avoid spamming the recipient.
type Alerter struct {
	mailer   *Mailer
	to       string
	cooldown time.Duration
	log      *slog.Logger

	mu       sync.Mutex
	lastSent map[AlertType]time.Time
}

// NewAlerter creates a new Alerter. Returns nil if alerts are not configured.
func NewAlerter(cfg AlertsConfig, mailer *Mailer, log *slog.Logger) *Alerter {
	if cfg.Email == "" {
		return nil
	}
	cooldown := time.Duration(cfg.CooldownMinutes) * time.Minute
	if cooldown == 0 {
		cooldown = 30 * time.Minute
	}
	return &Alerter{
		mailer:   mailer,
		to:       cfg.Email,
		cooldown: cooldown,
		log:      log,
		lastSent: make(map[AlertType]time.Time),
	}
}

// Send sends an alert email if the cooldown period for this alert type has elapsed.
func (a *Alerter) Send(alertType AlertType, subject, body string) {
	if a == nil {
		return
	}

	a.mu.Lock()
	last, ok := a.lastSent[alertType]
	if ok && time.Since(last) < a.cooldown {
		a.mu.Unlock()
		a.log.Debug("Alert suppressed (cooldown)", "type", alertType)
		return
	}
	a.lastSent[alertType] = time.Now()
	a.mu.Unlock()

	msg := Message{
		To:      []string{a.to},
		Subject: fmt.Sprintf("[garmin-relay alert] %s", subject),
		Body:    body,
	}

	if err := a.mailer.Send(msg); err != nil {
		a.log.Error("Failed to send alert email", "type", alertType, "err", err)
		return
	}
	a.log.Info("Alert email sent", "type", alertType, "to", a.to)
}
