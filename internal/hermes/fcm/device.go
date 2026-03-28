package fcm

// AndroidDeviceInfo contains device identity information required for
// Android-native GCM/FCM registration, mimicking a real Android device.
type AndroidDeviceInfo struct {
	// BuildFingerprint is the Android build fingerprint
	// Format: brand/product/device:version/build_id/build_number:user/release-keys
	BuildFingerprint string

	// SDKVersion is the Android SDK version (e.g., 33 for Android 13)
	SDKVersion int

	// GMSVersion is the Google Mobile Services (Google Play Services) version
	GMSVersion int

	// Device is the device codename (e.g., "panther" for Pixel 7)
	Device string

	// Model is the device model name (e.g., "Pixel 7")
	Model string

	// ChromeVersion is the Chrome browser version (for compatibility)
	ChromeVersion string

	// Hardware is the hardware name (Build.HARDWARE), usually same as Device
	Hardware string

	// Brand is the device brand (Build.BRAND), e.g. "google"
	Brand string

	// Manufacturer is the device manufacturer (Build.MANUFACTURER), e.g. "Google"
	Manufacturer string

	// Product is the product name (Build.PRODUCT), usually same as Device
	Product string

	// Bootloader is the bootloader version string
	Bootloader string

	// Radio is the radio firmware version (Build.getRadioVersion())
	Radio string

	// BuildTime is the build timestamp (Build.TIME / 1000, seconds since epoch)
	BuildTime int64
}

// DefaultAndroidDevice returns a credible Pixel 7 device configuration
// based on real Google factory images and Play Services versions.
//
// This configuration is used for Android-native GCM/FCM registration to
// match what a real Android device would send.
func DefaultAndroidDevice() AndroidDeviceInfo {
	return AndroidDeviceInfo{
		// Pixel 7 (panther) build fingerprint from Google factory image
		// Source: https://developers.google.com/android/images
		// Build: TQ3A.230805.001 (August 2023, Android 13)
		BuildFingerprint: "google/panther/panther:13/TQ3A.230805.001/10316531:user/release-keys",

		// Android 13 = SDK 33
		SDKVersion: 33,

		// Google Play Services version 241516037
		// Source: APKMirror (real version from ~August 2023)
		GMSVersion: 241516037,

		// Pixel 7 device info
		Device:   "panther",
		Model:    "Pixel 7",
		Hardware: "panther",

		// Brand and manufacturer
		Brand:        "google",
		Manufacturer: "Google",
		Product:      "panther",

		// Bootloader and radio versions from Pixel 7 factory image
		Bootloader: "slider-1.2-9819352",
		Radio:      "g5300g-230511-230925-B-10484716",

		// Build.TIME / 1000 for TQ3A.230805.001 (approx August 5, 2023)
		BuildTime: 1691193600,

		// Chrome 120.0.6099.144 (stable version from ~December 2023)
		ChromeVersion: "120.0.6099.144",
	}
}
