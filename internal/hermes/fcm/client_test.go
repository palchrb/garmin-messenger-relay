package fcm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/palchrb/garmin-messenger-relay/internal/hermes/internal/checkinpb"
	"github.com/palchrb/garmin-messenger-relay/internal/hermes/internal/mcspb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFCMCredentialPersistence(t *testing.T) {
	sessionDir := t.TempDir()
	client := NewClient(sessionDir)

	// Android-native: No PrivateKey/AuthSecret
	client.credentials = &Credentials{
		Raw:           json.RawMessage(`{"androidId":123,"securityToken":456}`),
		Token:         "fcm-token-123",
		PersistentIDs: []string{"pid-1", "pid-2"},
	}

	require.NoError(t, client.saveCredentials())

	loaded := NewClient(sessionDir)
	require.NoError(t, loaded.loadCredentials())
	require.NotNil(t, loaded.Credentials())

	assert.JSONEq(t, string(client.credentials.Raw), string(loaded.Credentials().Raw))
	assert.Equal(t, client.credentials.Token, loaded.Credentials().Token)
	assert.Equal(t, client.credentials.PersistentIDs, loaded.Credentials().PersistentIDs)
}

func TestFCMNewClient(t *testing.T) {
	sessionDir := t.TempDir()
	client := NewClient(sessionDir)

	assert.NotNil(t, client)
	assert.Equal(t, sessionDir, client.sessionDir)
	assert.Equal(t, http.DefaultClient, client.httpClient)
	assert.NotNil(t, client.logger)
	assert.Nil(t, client.credentials)
}

func TestFCMToken_Empty(t *testing.T) {
	client := NewClient(t.TempDir())
	assert.Empty(t, client.Token())
}

func TestFCMToken_AfterLoad(t *testing.T) {
	sessionDir := t.TempDir()
	writer := NewClient(sessionDir)
	writer.credentials = &Credentials{
		Raw:           json.RawMessage(`{"foo":"bar"}`),
		Token:         "persisted-token",
		PersistentIDs: []string{"one"},
	}
	require.NoError(t, writer.saveCredentials())

	reader := NewClient(sessionDir)
	require.NoError(t, reader.loadCredentials())
	assert.Equal(t, "persisted-token", reader.Token())
}

func TestFCMCredentialsCopy(t *testing.T) {
	sessionDir := t.TempDir()
	client := NewClient(sessionDir)
	client.credentials = &Credentials{
		Raw:           json.RawMessage(`{"androidId":123}`),
		Token:         "original-token",
		PersistentIDs: []string{"pid-1", "pid-2"},
	}

	creds := client.Credentials()
	require.NotNil(t, creds)

	// Mutating the copy should not affect the internal state
	creds.Token = "mutated"
	creds.PersistentIDs[0] = "mutated"
	creds.Raw = json.RawMessage(`{"mutated":true}`)

	internal := client.Credentials()
	assert.Equal(t, "original-token", internal.Token)
	assert.Equal(t, "pid-1", internal.PersistentIDs[0])
	assert.JSONEq(t, `{"androidId":123}`, string(internal.Raw))
}

func TestFCMRegisterExistingCredentials(t *testing.T) {
	sessionDir := t.TempDir()
	seed := NewClient(sessionDir)
	seed.credentials = &Credentials{
		Raw:           json.RawMessage(`{"foo":"bar"}`),
		Token:         "existing-token",
		PersistentIDs: []string{"pid-1"},
	}
	require.NoError(t, seed.saveCredentials())

	httpCalls := 0
	client := NewClient(sessionDir, WithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			httpCalls++
			return nil, errors.New("unexpected network call")
		}),
	}))

	token, err := client.Register(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "existing-token", token)
	assert.Equal(t, 0, httpCalls)
}

// Web push-specific tests removed (generateFID, fcmInstall, fcmRegister, key generation)
// Android-native FCM doesn't use Firebase Installation or FCM Registration endpoints

func TestFCMRegisterAndroidNative(t *testing.T) {
	// Mock the GCM checkin endpoint
	checkinServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)

		var req checkinpb.AndroidCheckinRequest
		require.NoError(t, proto.Unmarshal(body, &req))
		// Android-native: DEVICE_ANDROID_OS, no ChromeBuild
		assert.Equal(t, checkinpb.DeviceType_DEVICE_ANDROID_OS, req.GetCheckin().GetType())
		assert.Nil(t, req.GetCheckin().GetChromeBuild())

		resp := &checkinpb.AndroidCheckinResponse{
			StatsOk:       proto.Bool(true),
			AndroidId:     proto.Uint64(999),
			SecurityToken: proto.Uint64(888),
		}
		out, _ := proto.Marshal(resp)
		_, _ = w.Write(out)
	}))
	defer checkinServer.Close()

	// Mock the GCM register endpoint
	registerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.Header.Get("Authorization"), "AidLogin")
		require.NoError(t, r.ParseForm())
		// Android-native: Garmin app package and sender ID
		assert.Equal(t, garminAppPackage, r.PostForm.Get("app"))
		assert.Equal(t, GarminSenderID, r.PostForm.Get("sender"))
		assert.Equal(t, GarminAPKCertSHA1, r.PostForm.Get("cert"))
		// Verify 11 Android fields
		assert.NotEmpty(t, r.PostForm.Get("app_ver"))
		assert.NotEmpty(t, r.PostForm.Get("gcm_ver"))
		assert.NotEmpty(t, r.PostForm.Get("X-scope"))
		assert.NotEmpty(t, r.PostForm.Get("X-appid"))
		assert.NotEmpty(t, r.PostForm.Get("X-osv"))
		assert.NotEmpty(t, r.PostForm.Get("X-gmsv"))
		assert.NotEmpty(t, r.PostForm.Get("X-cliv"))
		fmt.Fprint(w, "token=mock-fcm-token-android")
	}))
	defer registerServer.Close()

	// Override package-level URLs
	origCheckin := gcmCheckinURL
	origRegister := gcmRegisterURL
	gcmCheckinURL = checkinServer.URL
	gcmRegisterURL = registerServer.URL
	defer func() {
		gcmCheckinURL = origCheckin
		gcmRegisterURL = origRegister
	}()

	sessionDir := t.TempDir()
	client := NewClient(sessionDir)

	token, err := client.Register(context.Background())
	require.NoError(t, err)
	// Android-native: GCM token IS the FCM token
	assert.Equal(t, "mock-fcm-token-android", token)

	// Verify credentials were saved (Android-native: no encryption keys)
	reloaded := NewClient(sessionDir)
	require.NoError(t, reloaded.loadCredentials())
	assert.Equal(t, "mock-fcm-token-android", reloaded.Token())

	var gcmCreds gcmCredentials
	require.NoError(t, json.Unmarshal(reloaded.Credentials().Raw, &gcmCreds))
	assert.Equal(t, uint64(999), gcmCreds.AndroidID)
	assert.Equal(t, uint64(888), gcmCreds.SecurityToken)
	// Android-native: No PrivateKey or AuthSecret
}

// NOTE: Registration against Google FCM endpoints requires live network access
// and real device credentials. This is covered by integration tests instead.
func TestFCMRegisterNewClient_Integration(t *testing.T) {
	t.Skip("requires live Google FCM endpoints")
}

func TestFCMParseNewMessage(t *testing.T) {
	payload := `{"newMessage": {"messageId": "550e8400-e29b-41d4-a716-446655440000", "conversationId": "660e8400-e29b-41d4-a716-446655440000", "messageBody": "Hello from FCM"}}`
	ev, err := parseDataMessage([]byte(payload))
	require.NoError(t, err)

	msg, ok := ev.(NewMessage)
	require.True(t, ok)
	require.NotNil(t, msg.MessageBody)
	assert.Equal(t, "Hello from FCM", *msg.MessageBody)
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440000", msg.MessageID.String())
}

func TestFCMParseNonconversationalMessage(t *testing.T) {
	payload := `{"nonconversationalMessageExists": {"imei": "300434038675370"}}`
	ev, err := parseDataMessage([]byte(payload))
	require.NoError(t, err)

	nc, ok := ev.(NonconversationalMessage)
	require.True(t, ok)
	assert.Equal(t, "300434038675370", nc.IMEI)
}

func TestFCMParseNonconversationalMessageNumericIMEI(t *testing.T) {
	payload := `{"nonconversationalMessageExists": {"imei": 300434038675370}}`
	ev, err := parseDataMessage([]byte(payload))
	require.NoError(t, err)

	nc, ok := ev.(NonconversationalMessage)
	require.True(t, ok)
	assert.Equal(t, "300434038675370", nc.IMEI)
}

func TestFCMParseDeviceAccountUpdate(t *testing.T) {
	payload := `{"deviceAccountUpdate": {"foo": "bar"}}`
	ev, err := parseDataMessage([]byte(payload))
	require.NoError(t, err)

	dau, ok := ev.(DeviceAccountUpdate)
	require.True(t, ok)
	assert.JSONEq(t, `{"foo": "bar"}`, string(dau.Data))
}

func TestFCMParseUnknownPayload(t *testing.T) {
	payload := `{"unknownType": {}}`
	_, err := parseDataMessage([]byte(payload))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown FCM payload type")
}

func TestFCMPersistentIDTracking(t *testing.T) {
	sessionDir := t.TempDir()
	client := NewClient(sessionDir)
	client.credentials = &Credentials{
		Raw:           json.RawMessage(`{}`),
		Token:         "test-token",
		PersistentIDs: []string{},
	}

	client.addPersistentID("pid-1")
	client.addPersistentID("pid-2")
	client.addPersistentID("")

	assert.Equal(t, []string{"pid-1", "pid-2"}, client.PersistentIDs())

	reloaded := NewClient(sessionDir)
	require.NoError(t, reloaded.loadCredentials())
	assert.Equal(t, []string{"pid-1", "pid-2"}, reloaded.PersistentIDs())
}

func TestFCMPersistentIDCap(t *testing.T) {
	sessionDir := t.TempDir()
	client := NewClient(sessionDir)
	client.credentials = &Credentials{
		Raw:           json.RawMessage(`{}`),
		Token:         "test-token",
		PersistentIDs: []string{},
	}

	// Add more than maxPersistentIDs
	for i := 0; i < maxPersistentIDs+50; i++ {
		client.addPersistentID(fmt.Sprintf("pid-%d", i))
	}

	ids := client.PersistentIDs()
	assert.Len(t, ids, maxPersistentIDs)
	// Oldest IDs should be pruned; the last entry should be the most recent
	assert.Equal(t, fmt.Sprintf("pid-%d", maxPersistentIDs+49), ids[len(ids)-1])
	// First entry should be pid-50 (the 51st item, since 0..49 got pruned)
	assert.Equal(t, "pid-50", ids[0])
}

func TestFCMHandleMCSMessage_NewMessage(t *testing.T) {
	sessionDir := t.TempDir()
	client := NewClient(sessionDir)
	client.credentials = &Credentials{
		Raw:           json.RawMessage(`{}`),
		Token:         "test-token",
		PersistentIDs: []string{},
	}

	var received NewMessage
	client.OnMessage(func(msg NewMessage) {
		received = msg
	})

	appData := []*mcspb.AppData{
		{
			Key:   proto.String("newMessage"),
			Value: proto.String(`{"messageId":"550e8400-e29b-41d4-a716-446655440000","conversationId":"660e8400-e29b-41d4-a716-446655440000","messageBody":"Hello MCS"}`),
		},
	}

	client.handleMCSMessage("persistent-abc", nil, appData)

	require.NotNil(t, received.MessageBody)
	assert.Equal(t, "Hello MCS", *received.MessageBody)
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440000", received.MessageID.String())
	assert.Contains(t, client.PersistentIDs(), "persistent-abc")
}

func TestFCMHandleMCSMessage_Nonconversational(t *testing.T) {
	sessionDir := t.TempDir()
	client := NewClient(sessionDir)
	client.credentials = &Credentials{
		Raw:           json.RawMessage(`{}`),
		Token:         "test-token",
		PersistentIDs: []string{},
	}

	var received NonconversationalMessage
	client.OnNonconversationalMessage(func(msg NonconversationalMessage) {
		received = msg
	})

	appData := []*mcspb.AppData{
		{
			Key:   proto.String("nonconversationalMessageExists"),
			Value: proto.String(`{"imei":"300434038675370"}`),
		},
	}

	client.handleMCSMessage("persistent-xyz", nil, appData)

	assert.Equal(t, "300434038675370", received.IMEI)
	assert.Contains(t, client.PersistentIDs(), "persistent-xyz")
}

func TestFCMHandleMCSMessage_DeviceAccountUpdate(t *testing.T) {
	sessionDir := t.TempDir()
	client := NewClient(sessionDir)
	client.credentials = &Credentials{
		Raw:           json.RawMessage(`{}`),
		Token:         "test-token",
		PersistentIDs: []string{},
	}

	var received DeviceAccountUpdate
	client.OnDeviceAccountUpdate(func(msg DeviceAccountUpdate) {
		received = msg
	})

	appData := []*mcspb.AppData{
		{
			Key:   proto.String("deviceAccountUpdate"),
			Value: proto.String(`{"foo":"bar"}`),
		},
	}

	client.handleMCSMessage("persistent-dau", nil, appData)

	assert.JSONEq(t, `{"foo":"bar"}`, string(received.Data))
}

func TestFCMHandleMCSMessage_PlaintextPayload(t *testing.T) {
	sessionDir := t.TempDir()
	client := NewClient(sessionDir)
	client.credentials = &Credentials{
		Raw:           json.RawMessage(`{}`),
		Token:         "test-token",
		PersistentIDs: []string{},
	}

	var received NewMessage
	client.OnMessage(func(msg NewMessage) {
		received = msg
	})

	// Android-native: Payload is plaintext JSON (not encrypted)
	payload := []byte(`{"newMessage":{"messageId":"550e8400-e29b-41d4-a716-446655440001","conversationId":"660e8400-e29b-41d4-a716-446655440000","messageBody":"Hello plaintext"}}`)
	client.handleMCSMessage("persistent-plain", payload, nil)

	require.NotNil(t, received.MessageBody)
	assert.Equal(t, "Hello plaintext", *received.MessageBody)
	assert.Equal(t, "550e8400-e29b-41d4-a716-446655440001", received.MessageID.String())
	assert.Contains(t, client.PersistentIDs(), "persistent-plain")
}
