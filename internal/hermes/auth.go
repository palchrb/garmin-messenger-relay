package garminmessenger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultHermesBase is the default Hermes API base URL.
	DefaultHermesBase = "https://hermes.inreachapp.com"

	// RegistrationAPIKey is the registration API key used by the Android app.
	RegistrationAPIKey = "?E2PFAzUzKx!S&1k1D"

	// DefaultPnsHandle is a placeholder FCM registration token used during
	// OTP registration when no real FCM token is available. It is overridden
	// by WithPnsHandle when the client has registered with FCM.
	DefaultPnsHandle = "cXr1bp_PSqaKHFG8W4vLHi:APA91bH8kL2xNmQpZ9vYtD5n3R7fUwXoEjKm4sCgBpV6qI0hA1dWzOyFuN8rT3lMxJvQ2bGnYk9wRcHiP7eDsUaZoL5fXtW4mBjK0vNq6SyRgCpAhD1iOuE3wTlMx"

	// PnsEnvironment is the push notification environment.
	PnsEnvironment = "Production"

	// tokenExpiryBuffer is the number of seconds before expiry to trigger refresh.
	tokenExpiryBuffer = 60
)

// APIError represents an HTTP error from the Hermes API.
type APIError struct {
	StatusCode int
	Status     string
	Body       string
	URL        string
	Method     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: %d %s: %s", e.Method, e.URL, e.StatusCode, e.Status, e.Body)
}

// HermesAuthOption configures HermesAuth.
type HermesAuthOption func(*HermesAuth)

// WithHermesBase sets the base URL for the Hermes API.
func WithHermesBase(base string) HermesAuthOption {
	return func(a *HermesAuth) {
		a.HermesBase = strings.TrimRight(base, "/")
	}
}

// WithSessionDir sets the directory for credential storage.
func WithSessionDir(dir string) HermesAuthOption {
	return func(a *HermesAuth) {
		a.SessionDir = dir
	}
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(client *http.Client) HermesAuthOption {
	return func(a *HermesAuth) {
		a.httpClient = client
	}
}

// WithLogger sets a custom logger.
func WithLogger(logger *slog.Logger) HermesAuthOption {
	return func(a *HermesAuth) {
		a.logger = logger
	}
}

// WithPnsHandle overrides the FCM token used during registration.
func WithPnsHandle(handle string) HermesAuthOption {
	return func(a *HermesAuth) {
		a.pnsHandle = handle
	}
}

// HermesAuth manages the full authentication lifecycle for Hermes messaging API.
type HermesAuth struct {
	HermesBase   string
	SessionDir   string
	AccessToken  string
	RefreshToken string
	InstanceID   string
	ExpiresAt    float64 // Unix timestamp
	pnsHandle    string

	httpClient *http.Client
	logger     *slog.Logger
	mu         sync.Mutex
}

// NewHermesAuth creates a new HermesAuth with the given options.
func NewHermesAuth(opts ...HermesAuthOption) *HermesAuth {
	a := &HermesAuth{
		HermesBase: DefaultHermesBase,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     slog.Default(),
		pnsHandle:  DefaultPnsHandle,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// RequestOTP requests an SMS OTP code for the given phone number.
func (a *HermesAuth) RequestOTP(ctx context.Context, phone, deviceName string) (*OtpRequest, error) {
	a.logger.Debug("Requesting SMS OTP", "phone", phone)

	url := a.HermesBase + "/Registration/App"
	body := NewAppRegistrationBody{
		SmsNumber: phone,
		Platform:  "android", // Android-native FCM
	}
	bodyBytes, _ := json.Marshal(body)

	headers := http.Header{
		"RegistrationApiKey": {RegistrationAPIKey},
		"Api-Version":        {"1.0"},
		"Content-Type":       {"application/json"},
	}

	resp, err := a.doRequest(ctx, "POST", url, headers, bodyBytes)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 409 {
		a.logger.Warn("Previous OTP request still active, waiting 30s")
		select {
		case <-time.After(30 * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		resp.Body.Close()
		resp, err = a.doRequest(ctx, "POST", url, headers, bodyBytes)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
	}

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var otpResp NewAppRegistrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&otpResp); err != nil {
		return nil, fmt.Errorf("decoding OTP response: %w", err)
	}

	a.logger.Debug("OTP requested", "requestId", otpResp.RequestID)

	return &OtpRequest{
		RequestID:         otpResp.RequestID,
		PhoneNumber:       phone,
		DeviceName:        deviceName,
		ValidUntil:        otpResp.ValidUntil,
		AttemptsRemaining: otpResp.AttemptsRemaining,
	}, nil
}

// ConfirmOTP confirms registration with the SMS OTP code.
func (a *HermesAuth) ConfirmOTP(ctx context.Context, otpReq *OtpRequest, otpCode string) error {
	a.logger.Debug("Confirming OTP")

	url := a.HermesBase + "/Registration/App/Confirm"
	body := ConfirmAppRegistrationBody{
		RequestID:        otpReq.RequestID,
		SmsNumber:        otpReq.PhoneNumber,
		VerificationCode: otpCode,
		Platform:         "android", // Android-native FCM
		PnsHandle:        a.pnsHandle,
		PnsEnvironment:   PnsEnvironment,
		AppDescription:   otpReq.DeviceName,
		OptInForSms:      true,
	}
	bodyBytes, _ := json.Marshal(body)

	headers := http.Header{
		"RegistrationApiKey": {RegistrationAPIKey},
		"Api-Version":        {"1.0"},
		"Content-Type":       {"application/json"},
	}

	resp, err := a.doRequest(ctx, "POST", url, headers, bodyBytes)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return err
	}

	var reg AppRegistrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		return fmt.Errorf("decoding registration response: %w", err)
	}

	a.logger.Debug("Registration successful", "instanceId", reg.InstanceID)
	a.storeCredentials(reg.InstanceID, &reg.AccessAndRefreshToken)

	return nil
}

// PnsHandle returns the current PNS handle value.
func (a *HermesAuth) PnsHandle() string {
	return a.pnsHandle
}

// GetRegistrations lists all registered devices/apps for the current account.
func (a *HermesAuth) GetRegistrations(ctx context.Context) (map[string]interface{}, error) {
	url := a.HermesBase + "/Registration"

	headers, err := a.Headers(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting auth headers: %w", err)
	}
	headers.Set("Api-Version", "1.0")

	resp, err := a.doRequest(ctx, "GET", url, headers, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, fmt.Errorf("getting registrations: %w", err)
	}

	var registrations map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&registrations); err != nil {
		return nil, fmt.Errorf("decoding registrations: %w", err)
	}

	return registrations, nil
}

// DeleteAppRegistration deletes a specific app registration by instance ID.
func (a *HermesAuth) DeleteAppRegistration(ctx context.Context, instanceID string) error {
	a.logger.Debug("Deleting app registration", "instanceId", instanceID)

	reqURL := a.HermesBase + "/Registration/App/" + url.PathEscape(instanceID)

	headers, err := a.Headers(ctx)
	if err != nil {
		return fmt.Errorf("getting auth headers: %w", err)
	}
	headers.Set("Api-Version", "1.0")

	resp, err := a.doRequest(ctx, "DELETE", reqURL, headers, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete app registration: HTTP %d: %s", resp.StatusCode, string(body))
	}

	a.logger.Debug("App registration deleted successfully")
	return nil
}

// DeleteUserRegistration deletes the entire user registration (all devices/apps).
func (a *HermesAuth) DeleteUserRegistration(ctx context.Context) error {
	a.logger.Warn("Deleting entire user registration - this will remove ALL devices/apps")

	url := a.HermesBase + "/Registration/User"

	headers, err := a.Headers(ctx)
	if err != nil {
		return fmt.Errorf("getting auth headers: %w", err)
	}
	headers.Set("Api-Version", "1.0")

	resp, err := a.doRequest(ctx, "DELETE", url, headers, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete user registration: HTTP %d: %s", resp.StatusCode, string(body))
	}

	a.logger.Debug("User registration deleted successfully")
	return nil
}

// UpdatePnsHandle sends a PATCH to Registration/App to update the FCM push notification token.
func (a *HermesAuth) UpdatePnsHandle(ctx context.Context, pnsHandle string) error {
	a.logger.Debug("Updating PNS handle")

	url := a.HermesBase + "/Registration/App"
	body := UpdateAppPnsHandleBody{
		PnsHandle:      pnsHandle,
		PnsEnvironment: PnsEnvironment,
		AppDescription: "garmin-messenger",
	}
	bodyBytes, _ := json.Marshal(body)

	headers, err := a.Headers(ctx)
	if err != nil {
		return fmt.Errorf("getting auth headers: %w", err)
	}
	headers.Set("Api-Version", "1.0")
	headers.Set("Content-Type", "application/json")

	resp, err := a.doRequest(ctx, "PATCH", url, headers, bodyBytes)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return fmt.Errorf("updating PNS handle: %w", err)
	}

	a.logger.Debug("PNS handle updated successfully")
	return nil
}

// Resume restores credentials from disk. If the token is expired, it refreshes.
func (a *HermesAuth) Resume(ctx context.Context) error {
	credsPath := a.credsPath()
	if credsPath == "" {
		return fmt.Errorf("no session directory configured")
	}

	data, err := os.ReadFile(credsPath)
	if err != nil {
		return fmt.Errorf("no saved credentials found at %s: call RequestOTP() + ConfirmOTP() first", credsPath)
	}

	var creds struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		InstanceID   string  `json:"instance_id"`
		ExpiresAt    float64 `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return fmt.Errorf("parsing credentials: %w", err)
	}

	a.AccessToken = creds.AccessToken
	a.RefreshToken = creds.RefreshToken
	a.InstanceID = creds.InstanceID
	a.ExpiresAt = creds.ExpiresAt

	a.logger.Debug("Resumed credentials",
		"instanceId", a.InstanceID,
		"expiresAt", time.Unix(int64(a.ExpiresAt), 0).UTC().Format(time.RFC3339),
		"expiresIn", fmt.Sprintf("%.0fs", a.ExpiresAt-float64(time.Now().Unix())),
	)

	if a.TokenExpired() {
		return a.RefreshHermesToken(ctx)
	}

	return nil
}

// Headers returns auth headers for Hermes REST API requests.
// It automatically refreshes expired tokens.
func (a *HermesAuth) Headers(ctx context.Context) (http.Header, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.TokenExpired() {
		if err := a.refreshHermesTokenLocked(ctx); err != nil {
			return nil, err
		}
	}

	h := http.Header{}
	h.Set("Authorization", "Bearer "+a.AccessToken)
	h.Set("Api-Version", "2.0")
	return h, nil
}

// TokenExpired returns true if the access token is expired or missing.
func (a *HermesAuth) TokenExpired() bool {
	if a.AccessToken == "" {
		return true
	}
	return float64(time.Now().Unix()) >= a.ExpiresAt-tokenExpiryBuffer
}

// RefreshHermesToken refreshes the Hermes access token.
func (a *HermesAuth) RefreshHermesToken(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshHermesTokenLocked(ctx)
}

func (a *HermesAuth) refreshHermesTokenLocked(ctx context.Context) error {
	if a.RefreshToken == "" || a.InstanceID == "" {
		return fmt.Errorf("no refresh token / instance ID: call RequestOTP() + ConfirmOTP() first")
	}

	a.logger.Debug("Refreshing Hermes token")

	url := a.HermesBase + "/Registration/App/Refresh"
	body := RefreshAuthBody{
		RefreshToken: a.RefreshToken,
		InstanceID:   a.InstanceID,
	}
	bodyBytes, _ := json.Marshal(body)

	headers := http.Header{
		"Api-Version":  {"1.0"},
		"Content-Type": {"application/json"},
	}

	resp, err := a.doRequest(ctx, "POST", url, headers, bodyBytes)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return err
	}

	var reg AppRegistrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		return fmt.Errorf("decoding refresh response: %w", err)
	}

	a.storeCredentials(reg.InstanceID, &reg.AccessAndRefreshToken)
	return nil
}

// AccessTokenFactory returns the current access token, refreshing if needed.
// Used by the SignalR client.
func (a *HermesAuth) AccessTokenFactory(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.TokenExpired() {
		if err := a.refreshHermesTokenLocked(ctx); err != nil {
			return "", err
		}
	}
	return a.AccessToken, nil
}

// storeCredentials saves the access/refresh tokens to memory and disk.
func (a *HermesAuth) storeCredentials(instanceID string, tokens *AccessAndRefreshToken) {
	a.AccessToken = tokens.AccessToken
	a.RefreshToken = tokens.RefreshToken
	a.InstanceID = instanceID
	a.ExpiresAt = float64(time.Now().Unix()) + float64(tokens.ExpiresIn)

	credsPath := a.credsPath()
	if credsPath == "" {
		return
	}

	if err := os.MkdirAll(filepath.Dir(credsPath), 0o755); err != nil {
		a.logger.Error("Failed to create session directory", "error", err)
		return
	}

	data, _ := json.MarshalIndent(map[string]interface{}{
		"access_token":  a.AccessToken,
		"refresh_token": a.RefreshToken,
		"instance_id":   a.InstanceID,
		"expires_at":    a.ExpiresAt,
	}, "", "  ")

	if err := os.WriteFile(credsPath, data, 0o600); err != nil {
		a.logger.Error("Failed to save credentials", "error", err)
	} else {
		a.logger.Debug("Saved credentials", "path", credsPath)
	}
}

func (a *HermesAuth) credsPath() string {
	if a.SessionDir == "" {
		return ""
	}
	return filepath.Join(a.SessionDir, "hermes_credentials.json")
}

func (a *HermesAuth) doRequest(ctx context.Context, method, url string, headers http.Header, body []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	for k, v := range headers {
		req.Header[k] = v
	}

	a.logRequest(method, url, headers, body)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}

	// Read body for logging, then replace it so callers can still read it.
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	a.logResponse(resp)
	a.logger.Debug("  Response body", "length", len(respBody), "json", truncate(string(respBody), 2000))
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	return resp, nil
}

func (a *HermesAuth) logRequest(method, url string, headers http.Header, body []byte) {
	a.logger.Debug(">>> "+method, "url", url)
	for k, v := range headers {
		val := strings.Join(v, ", ")
		if len(val) > 120 {
			a.logger.Debug("  Request header", "key", k, "value", val[:60]+"..."+val[len(val)-20:])
		} else {
			a.logger.Debug("  Request header", "key", k, "value", val)
		}
	}
	if body != nil {
		a.logger.Debug("  Request body", "json", string(body))
	}
}

func (a *HermesAuth) logResponse(resp *http.Response) {
	a.logger.Debug("<<< Response", "status", resp.StatusCode, "url", resp.Request.URL.String())
	for k, v := range resp.Header {
		a.logger.Debug("  Response header", "key", k, "value", strings.Join(v, ", "))
	}
}

func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return &APIError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       string(body),
		URL:        resp.Request.URL.String(),
		Method:     resp.Request.Method,
	}
}
