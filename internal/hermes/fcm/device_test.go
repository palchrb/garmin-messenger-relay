package fcm

import (
	"regexp"
	"testing"
)

func TestDefaultAndroidDevice(t *testing.T) {
	device := DefaultAndroidDevice()

	// Verify build fingerprint is non-empty and matches Android format
	if device.BuildFingerprint == "" {
		t.Fatal("BuildFingerprint should not be empty")
	}
	// Format: brand/product/device:version/build_id/build_number:user/release-keys
	fingerprintPattern := regexp.MustCompile(`^[^/]+/[^/]+/[^:]+:[0-9]+/[^/]+/[^:]+:(user|userdebug)/(release-keys|dev-keys)$`)
	if !fingerprintPattern.MatchString(device.BuildFingerprint) {
		t.Errorf("BuildFingerprint has invalid format: %s", device.BuildFingerprint)
	}

	// Verify SDK version is reasonable (Android 7+ = SDK 24+)
	if device.SDKVersion < 24 || device.SDKVersion > 40 {
		t.Errorf("SDKVersion should be between 24 and 40, got: %d", device.SDKVersion)
	}

	// Verify GMS version is non-zero
	if device.GMSVersion == 0 {
		t.Error("GMSVersion should not be zero")
	}

	// Verify device and model are non-empty
	if device.Device == "" {
		t.Error("Device should not be empty")
	}
	if device.Model == "" {
		t.Error("Model should not be empty")
	}

	// Verify Chrome version format (e.g., "120.0.6099.144")
	chromePattern := regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)
	if !chromePattern.MatchString(device.ChromeVersion) {
		t.Errorf("ChromeVersion has invalid format: %s", device.ChromeVersion)
	}
}

func TestGarminAPKCertSHA1(t *testing.T) {
	// Verify constant is 40-character lowercase hex string
	sha1Pattern := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if !sha1Pattern.MatchString(GarminAPKCertSHA1) {
		t.Errorf("GarminAPKCertSHA1 has invalid format: %s (should be 40-char lowercase hex)", GarminAPKCertSHA1)
	}

	// Verify it's not a placeholder
	if GarminAPKCertSHA1 == "0000000000000000000000000000000000000000" {
		t.Error("GarminAPKCertSHA1 appears to be a placeholder, not a real certificate")
	}
}

func TestAndroidDeviceInfo_Fields(t *testing.T) {
	// Test that we can create a custom AndroidDeviceInfo
	custom := AndroidDeviceInfo{
		BuildFingerprint: "google/test/test:13/TEST/1:user/release-keys",
		SDKVersion:       33,
		GMSVersion:       240000000,
		Device:           "test_device",
		Model:            "Test Model",
		ChromeVersion:    "120.0.0.0",
	}

	if custom.BuildFingerprint != "google/test/test:13/TEST/1:user/release-keys" {
		t.Error("BuildFingerprint field not set correctly")
	}
	if custom.SDKVersion != 33 {
		t.Error("SDKVersion field not set correctly")
	}
	if custom.GMSVersion != 240000000 {
		t.Error("GMSVersion field not set correctly")
	}
	if custom.Device != "test_device" {
		t.Error("Device field not set correctly")
	}
	if custom.Model != "Test Model" {
		t.Error("Model field not set correctly")
	}
	if custom.ChromeVersion != "120.0.0.0" {
		t.Error("ChromeVersion field not set correctly")
	}
}
