package fcm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palchrb/garmin-messenger-relay/internal/hermes/internal/checkinpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestGCMCheckin(t *testing.T) {
	var receivedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/x-protobuf", r.Header.Get("Content-Type"))

		var err error
		receivedBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)

		resp := &checkinpb.AndroidCheckinResponse{
			StatsOk:       proto.Bool(true),
			AndroidId:     proto.Uint64(123456789),
			SecurityToken: proto.Uint64(987654321),
		}
		data, err := proto.Marshal(resp)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	origURL := gcmCheckinURL
	gcmCheckinURL = srv.URL
	defer func() { gcmCheckinURL = origURL }()

	device := DefaultAndroidDevice()
	androidID, securityToken, err := gcmCheckin(context.Background(), srv.Client(), 0, 0, device)
	require.NoError(t, err)
	assert.Equal(t, uint64(123456789), androidID)
	assert.Equal(t, uint64(987654321), securityToken)

	// Verify the request is Android-native style checkin (not Chrome).
	var req checkinpb.AndroidCheckinRequest
	require.NoError(t, proto.Unmarshal(receivedBody, &req))
	assert.Equal(t, checkinpb.DeviceType_DEVICE_ANDROID_OS, req.GetCheckin().GetType())
	assert.Nil(t, req.GetCheckin().GetChromeBuild(), "Android-native should not have ChromeBuild")
	assert.Nil(t, req.GetCheckin().UserNumber)
	assert.Equal(t, int32(3), req.GetVersion())
	require.NotNil(t, req.Fragment)
	assert.Equal(t, int32(0), req.GetFragment())

	// Verify Android build info is populated.
	build := req.GetCheckin().GetBuild()
	require.NotNil(t, build, "Android-native checkin must include AndroidBuildProto")
	assert.Equal(t, device.BuildFingerprint, build.GetFingerprint())
	assert.Equal(t, device.Hardware, build.GetHardware())
	assert.Equal(t, device.Brand, build.GetBrand())
	assert.Equal(t, device.Device, build.GetDevice())
	assert.Equal(t, device.Model, build.GetModel())
	assert.Equal(t, device.Manufacturer, build.GetManufacturer())
	assert.Equal(t, device.Product, build.GetProduct())
	assert.Equal(t, int32(device.SDKVersion), build.GetSdkVersion())
	assert.Equal(t, int32(device.GMSVersion), build.GetPackageVersionCode())
	assert.Equal(t, "android-google", build.GetClientId())

	// Verify locale and timezone are set.
	assert.Equal(t, "en_US", req.GetLocale())
	assert.Equal(t, "America/New_York", req.GetTimeZone())
}

func TestGCMCheckin_Recheckin(t *testing.T) {
	var receivedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)

		resp := &checkinpb.AndroidCheckinResponse{
			StatsOk:       proto.Bool(true),
			AndroidId:     proto.Uint64(111),
			SecurityToken: proto.Uint64(222),
		}
		data, _ := proto.Marshal(resp)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	origURL := gcmCheckinURL
	gcmCheckinURL = srv.URL
	defer func() { gcmCheckinURL = origURL }()

	device := DefaultAndroidDevice()
	androidID, securityToken, err := gcmCheckin(context.Background(), srv.Client(), 111, 222, device)
	require.NoError(t, err)
	assert.Equal(t, uint64(111), androidID)
	assert.Equal(t, uint64(222), securityToken)

	var req checkinpb.AndroidCheckinRequest
	require.NoError(t, proto.Unmarshal(receivedBody, &req))
	assert.Equal(t, int64(111), req.GetId())
	assert.Equal(t, uint64(222), req.GetSecurityToken())
}

func TestGCMCheckin_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	origURL := gcmCheckinURL
	gcmCheckinURL = srv.URL
	defer func() { gcmCheckinURL = origURL }()

	device := DefaultAndroidDevice()
	_, _, err := gcmCheckin(context.Background(), srv.Client(), 0, 0, device)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestGCMRegister(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "AidLogin 123:456", r.Header.Get("Authorization"))

		// Verify Android-native HTTP headers (required by Google's servers).
		assert.Equal(t, garminAppPackage, r.Header.Get("app"), "app header must match package name")
		assert.Contains(t, r.Header.Get("User-Agent"), "Android-GCM/1.5", "User-Agent must identify as Android GCM client")

		require.NoError(t, r.ParseForm())
		// Verify Android-native registration fields
		assert.Equal(t, garminAppPackage, r.PostForm.Get("app"))
		assert.Equal(t, GarminSenderID, r.PostForm.Get("sender"))
		assert.Equal(t, "123", r.PostForm.Get("device"))
		assert.Equal(t, GarminAPKCertSHA1, r.PostForm.Get("cert"))

		// Verify 11 Android-specific fields are present
		assert.NotEmpty(t, r.PostForm.Get("app_ver"))
		assert.NotEmpty(t, r.PostForm.Get("gcm_ver"))
		assert.NotEmpty(t, r.PostForm.Get("X-scope"))
		assert.NotEmpty(t, r.PostForm.Get("X-appid"))
		assert.NotEmpty(t, r.PostForm.Get("X-osv"))
		assert.NotEmpty(t, r.PostForm.Get("X-gmsv"))
		assert.NotEmpty(t, r.PostForm.Get("X-cliv"))

		// Verify instance ID format (11-char hex)
		appID := r.PostForm.Get("X-appid")
		assert.Regexp(t, "^[0-9a-f]{11}$", appID)

		fmt.Fprint(w, "token=test-fcm-token-xyz \n")
	}))
	defer srv.Close()

	origURL := gcmRegisterURL
	gcmRegisterURL = srv.URL
	defer func() { gcmRegisterURL = origURL }()

	device := DefaultAndroidDevice()
	token, err := gcmRegister(context.Background(), srv.Client(), 123, 456, device)
	require.NoError(t, err)
	assert.Equal(t, "test-fcm-token-xyz", token)
}

func TestGCMRegister_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Error=PHONE_REGISTRATION_ERROR")
	}))
	defer srv.Close()

	origURL := gcmRegisterURL
	gcmRegisterURL = srv.URL
	defer func() { gcmRegisterURL = origURL }()

	device := DefaultAndroidDevice()
	_, err := gcmRegister(context.Background(), srv.Client(), 123, 456, device)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PHONE_REGISTRATION_ERROR")
}

func TestGCMRegister_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable"))
	}))
	defer srv.Close()

	origURL := gcmRegisterURL
	gcmRegisterURL = srv.URL
	defer func() { gcmRegisterURL = origURL }()

	device := DefaultAndroidDevice()
	_, err := gcmRegister(context.Background(), srv.Client(), 123, 456, device)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "register"))
}
