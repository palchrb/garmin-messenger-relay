package fcm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/palchrb/garmin-messenger-relay/internal/hermes/internal/checkinpb"
	"google.golang.org/protobuf/proto"
)

// gcmCheckinURL and gcmRegisterURL are package-level vars so tests can override them.
var (
	gcmCheckinURL  = "https://android.clients.google.com/checkin"
	gcmRegisterURL = "https://android.clients.google.com/c2dm/register3"
)

// gcmCredentials holds the Android GCM device credentials.
type gcmCredentials struct {
	AndroidID     uint64 `json:"androidId"`
	SecurityToken uint64 `json:"securityToken"`
}

// gcmCheckin performs an Android-native GCM checkin. If androidID and
// securityToken are non-zero, this is a re-checkin with existing credentials.
func gcmCheckin(ctx context.Context, httpClient *http.Client, androidID, securityToken uint64, device AndroidDeviceInfo) (uint64, uint64, error) {
	clientID := "android-google"
	checkin := &checkinpb.AndroidCheckinProto{
		Build: &checkinpb.AndroidBuildProto{
			Fingerprint:        proto.String(device.BuildFingerprint),
			Hardware:           proto.String(device.Hardware),
			Brand:              proto.String(device.Brand),
			Radio:              proto.String(device.Radio),
			Bootloader:         proto.String(device.Bootloader),
			ClientId:           proto.String(clientID),
			Time:               proto.Int64(device.BuildTime),
			PackageVersionCode: proto.Int32(int32(device.GMSVersion)),
			Device:             proto.String(device.Device),
			SdkVersion:         proto.Int32(int32(device.SDKVersion)),
			Model:              proto.String(device.Model),
			Manufacturer:       proto.String(device.Manufacturer),
			Product:            proto.String(device.Product),
			OtaInstalled:       proto.Bool(false),
		},
		Type: checkinpb.DeviceType_DEVICE_ANDROID_OS.Enum(),
	}

	req := &checkinpb.AndroidCheckinRequest{
		Checkin:          checkin,
		Version:          proto.Int32(3),
		Fragment:         proto.Int32(0),
		Locale:           proto.String("en_US"),
		TimeZone:         proto.String("America/New_York"),
		UserSerialNumber: proto.Int32(0),
	}

	if androidID != 0 {
		req.Id = proto.Int64(int64(androidID))
		req.SecurityToken = proto.Uint64(securityToken)
	}

	body, err := proto.Marshal(req)
	if err != nil {
		return 0, 0, fmt.Errorf("gcm checkin: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, gcmCheckinURL, bytes.NewReader(body))
	if err != nil {
		return 0, 0, fmt.Errorf("gcm checkin: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-protobuf")

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return 0, 0, fmt.Errorf("gcm checkin: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, fmt.Errorf("gcm checkin: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("gcm checkin: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var checkinResp checkinpb.AndroidCheckinResponse
	if err := proto.Unmarshal(respBody, &checkinResp); err != nil {
		return 0, 0, fmt.Errorf("gcm checkin: unmarshal response: %w", err)
	}

	return checkinResp.GetAndroidId(), checkinResp.GetSecurityToken(), nil
}

// generateInstanceID generates a random 11-character hex string for the GCM instance ID.
// This mimics Android's GCM instance ID generation.
func generateInstanceID() (string, error) {
	b := make([]byte, 6) // 6 bytes = 12 hex chars, we'll take first 11
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate instance ID: %w", err)
	}
	return hex.EncodeToString(b)[:11], nil
}

// gcmRegister registers with GCM c2dm/register3 endpoint using Android-native style.
// This sends all required Android fields including APK certificate, device info, and
// GMS version to receive a valid Android FCM token.
func gcmRegister(ctx context.Context, httpClient *http.Client, androidID, securityToken uint64, device AndroidDeviceInfo) (string, error) {
	instanceID, err := generateInstanceID()
	if err != nil {
		return "", err
	}

	form := url.Values{
		// Core Android app identification
		"app":    {garminAppPackage},
		"sender": {GarminSenderID},
		"device": {strconv.FormatUint(androidID, 10)},
		"cert":   {GarminAPKCertSHA1}, // APK signing certificate SHA1

		// App and GCM version info
		"app_ver":    {"160500"},                                // App version code (example)
		"gcm_ver":    {strconv.Itoa(device.GMSVersion)},         // Google Mobile Services version
		"X-scope":    {"GCM"},                                   // Scope for this registration
		"X-appid":    {instanceID},                              // Instance ID (11-char hex)
		"X-osv":      {strconv.Itoa(device.SDKVersion)},         // Android SDK version
		"X-gmsv":     {strconv.Itoa(device.GMSVersion)},         // GMS version (duplicate for compatibility)
		"X-cliv":     {"iid-" + device.ChromeVersion},           // Client version (IID library)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, gcmRegisterURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("gcm register: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Authorization", fmt.Sprintf("AidLogin %d:%d", androidID, securityToken))
	httpReq.Header.Set("User-Agent", fmt.Sprintf("Android-GCM/1.5 (%s %s)", device.Device, device.Model))
	httpReq.Header.Set("app", garminAppPackage)

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("gcm register: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("gcm register: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gcm register: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	body := string(respBody)
	if token, found := strings.CutPrefix(body, "token="); found {
		return strings.TrimSpace(token), nil
	}

	return "", fmt.Errorf("gcm register: unexpected response: %s", body)
}
