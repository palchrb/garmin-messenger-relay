// Package garminmessenger provides a Go client for the Garmin Messenger (Hermes) protocol.
//
// It includes models for all wire-format types, SMS OTP authentication,
// a REST API client for conversations/messages/media/status, and a SignalR
// WebSocket client for real-time events.
package garminmessenger

import "log/slog"

// LevelTrace is a log level below Debug for verbose HTTP request/response
// details. Set log level to "trace" to see these messages.
const LevelTrace = slog.Level(-8)
