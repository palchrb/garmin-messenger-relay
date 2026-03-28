package garminmessenger

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestAuth(serverURL string) *HermesAuth {
	a := NewHermesAuth(WithHermesBase(serverURL))
	a.AccessToken = "test-token"
	a.RefreshToken = "test-refresh"
	a.InstanceID = testInstanceID
	a.ExpiresAt = float64(time.Now().Unix()) + 3600
	return a
}

// ---------------------------------------------------------------------------
// GetConversations
// ---------------------------------------------------------------------------

func TestGetConversations(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/Conversation/Updated", r.URL.Path)
		assert.Equal(t, "1.0", r.Header.Get("Api-Version"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, "20", r.URL.Query().Get("Limit"))

		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "get_conversations.json"))
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	result, err := api.GetConversations(context.Background(), WithLimit(20))
	require.NoError(t, err)
	require.Len(t, result.Conversations, 1)
	assert.Equal(t, uuid.MustParse(testConvID), result.Conversations[0].ConversationID)
}

// ---------------------------------------------------------------------------
// GetConversationDetail
// ---------------------------------------------------------------------------

func TestGetConversationDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Contains(t, r.URL.Path, "/Conversation/Details/")
		assert.Equal(t, "2.0", r.Header.Get("Api-Version"))

		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "conversation_detail.json"))
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	result, err := api.GetConversationDetail(context.Background(), uuid.MustParse(testConvID))
	require.NoError(t, err)
	assert.Len(t, result.Messages, 2)
	assert.Equal(t, 50, result.Limit)
}

// ---------------------------------------------------------------------------
// MuteConversation
// ---------------------------------------------------------------------------

func TestMuteConversation_Mute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Contains(t, r.URL.Path, "/Mute")
		assert.Equal(t, "1.0", r.Header.Get("Api-Version"))

		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), `"isMuted":true`)
		w.WriteHeader(200)
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	err := api.MuteConversation(context.Background(), uuid.MustParse(testConvID), true)
	require.NoError(t, err)
}

func TestMuteConversation_Unmute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Contains(t, r.URL.Path, "/Unmute")
		w.WriteHeader(200)
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	err := api.MuteConversation(context.Background(), uuid.MustParse(testConvID), false)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// GetConversationMembers
// ---------------------------------------------------------------------------

func TestGetConversationMembers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Contains(t, r.URL.Path, "/Conversation/Members/")
		assert.Equal(t, "1.0", r.Header.Get("Api-Version"))
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "conversation_members.json"))
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	result, err := api.GetConversationMembers(context.Background(), uuid.MustParse(testConvID))
	require.NoError(t, err)
	require.Len(t, result, 2)
	assert.Equal(t, ptr("Alice"), result[0].FriendlyName)
}

// ---------------------------------------------------------------------------
// GetMutedConversations
// ---------------------------------------------------------------------------

func TestGetMutedConversations(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/Conversation/Muted", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "muted_conversations.json"))
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	result, err := api.GetMutedConversations(context.Background())
	require.NoError(t, err)
	require.Len(t, result, 2)
}

// ---------------------------------------------------------------------------
// SendMessage
// ---------------------------------------------------------------------------

func TestSendMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/Message/Send", r.URL.Path)
		assert.Equal(t, "2.0", r.Header.Get("Api-Version"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, []any{testRecipientID}, body["to"])
		assert.Equal(t, "Hello!", body["messageBody"])
		// uuid and otaUuid should be present
		assert.NotNil(t, body["uuid"])
		assert.NotNil(t, body["otaUuid"])
		// userLocation and referencePoint should be null
		assert.Nil(t, body["userLocation"])
		assert.Nil(t, body["referencePoint"])

		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "send_message_response.json"))
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	result, err := api.SendMessage(context.Background(), []string{testRecipientID}, "Hello!")
	require.NoError(t, err)
	assert.Equal(t, uuid.MustParse(testMsgID), result.MessageID)
	assert.Equal(t, uuid.MustParse(testConvID), result.ConversationID)
}

// ---------------------------------------------------------------------------
// GetMessageDeviceMetadata
// ---------------------------------------------------------------------------

func TestGetMessageDeviceMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/Message/DeviceMetadata", r.URL.Path)
		assert.Equal(t, "2.0", r.Header.Get("Api-Version"))

		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "device_metadata.json"))
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	ids := []SimpleCompoundMessageId{{
		MessageID:      uuid.MustParse(testMsgID),
		ConversationID: uuid.MustParse(testConvID),
	}}
	result, err := api.GetMessageDeviceMetadata(context.Background(), ids)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.True(t, result[0].HasAllMtDeviceMetadata)
}

// ---------------------------------------------------------------------------
// UploadMedia
// ---------------------------------------------------------------------------

func TestUploadMedia(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		ct := r.Header.Get("Content-Type")
		assert.True(t, strings.HasPrefix(ct, "multipart/form-data"))

		err := r.ParseMultipartForm(10 << 20)
		require.NoError(t, err)

		// Verify form fields
		assert.Equal(t, testS3Key, r.FormValue("key"))
		assert.Equal(t, "STANDARD", r.FormValue("x-amz-storage-class"))
		assert.Equal(t, "AWS4-HMAC-SHA256", r.FormValue("x-amz-algorithm"))
		assert.Equal(t, "image/avif", r.FormValue("Content-Type"))

		// Verify file part
		file, _, err := r.FormFile("file")
		require.NoError(t, err)
		data, _ := io.ReadAll(file)
		assert.Equal(t, []byte("fake-image-data"), data)

		w.WriteHeader(204)
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	su := &SignedUploadUrl{
		UploadUrl:        server.URL,
		Key:              ptr(testS3Key),
		XAmzStorageClass: ptr("STANDARD"),
		XAmzDate:         ptr("20250115T103000Z"),
		XAmzSignature:    ptr("abcdef1234567890"),
		XAmzAlgorithm:    ptr("AWS4-HMAC-SHA256"),
		XAmzCredential:   ptr("AKIATEST/20250115/us-east-1/s3/aws4_request"),
		Policy:           ptr("base64policy"),
		ContentType:      ptr("image/avif"),
	}
	err := api.UploadMedia(context.Background(), su, []byte("fake-image-data"))
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// MarkAsRead / MarkAsDelivered
// ---------------------------------------------------------------------------

func TestMarkAsRead(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "PUT", r.Method)
		assert.Contains(t, r.URL.Path, "/Status/Read/")
		assert.Equal(t, "1.0", r.Header.Get("Api-Version"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(UpdateMessageStatusResponse{
			MessageID:      ptr(uuid.MustParse(testMsgID)),
			ConversationID: ptr(uuid.MustParse(testConvID)),
			Status:         ptr(MessageStatusRead),
		})
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	result, err := api.MarkAsRead(context.Background(), uuid.MustParse(testConvID), uuid.MustParse(testMsgID))
	require.NoError(t, err)
	assert.Equal(t, ptr(MessageStatusRead), result.Status)
}

func TestMarkAsDelivered(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "PUT", r.Method)
		assert.Contains(t, r.URL.Path, "/Status/Delivered/")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(UpdateMessageStatusResponse{
			MessageID: ptr(uuid.MustParse(testMsgID)),
			Status:    ptr(MessageStatusDelivered),
		})
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	result, err := api.MarkAsDelivered(context.Background(), uuid.MustParse(testConvID), uuid.MustParse(testMsgID))
	require.NoError(t, err)
	assert.Equal(t, ptr(MessageStatusDelivered), result.Status)
}

// ---------------------------------------------------------------------------
// UpdateMessageStatuses (batch)
// ---------------------------------------------------------------------------

func TestUpdateMessageStatuses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "PUT", r.Method)
		assert.Equal(t, "/Status/UpdateMessageStatuses", r.URL.Path)
		assert.Equal(t, "1.0", r.Header.Get("Api-Version"))

		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "batch_status_update_response.json"))
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	updates := []UpdateMessageStatusRequest{{
		MessageID:      uuid.MustParse(testMsgID),
		ConversationID: uuid.MustParse(testConvID),
		MessageStatus:  MessageStatusRead,
	}}
	result, err := api.UpdateMessageStatuses(context.Background(), updates)
	require.NoError(t, err)
	require.Len(t, result, 2)
}

// ---------------------------------------------------------------------------
// GetUpdatedStatuses
// ---------------------------------------------------------------------------

func TestGetUpdatedStatuses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/Status/Updated", r.URL.Path)
		assert.Equal(t, "1.0", r.Header.Get("Api-Version"))
		assert.NotEmpty(t, r.URL.Query().Get("AfterDate"))
		assert.NotEmpty(t, r.URL.Query().Get("Limit"))

		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "updated_statuses.json"))
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	afterDate := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	result, err := api.GetUpdatedStatuses(context.Background(), afterDate)
	require.NoError(t, err)
	require.Len(t, result.StatusReceiptsForMessages, 1)
}

// ---------------------------------------------------------------------------
// GetCapabilities
// ---------------------------------------------------------------------------

func TestGetCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/UserInfo/Capabilities", r.URL.Path)
		assert.Equal(t, "1.0", r.Header.Get("Api-Version"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"feature1": true}`))
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	result, err := api.GetCapabilities(context.Background())
	require.NoError(t, err)
	assert.Equal(t, true, result["feature1"])
}

// ---------------------------------------------------------------------------
// BlockUser / UnblockUser
// ---------------------------------------------------------------------------

func TestBlockUser(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/UserInfo/Block", r.URL.Path)
		assert.Equal(t, "1.0", r.Header.Get("Api-Version"))

		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "user-123", body["userId"])
		w.WriteHeader(200)
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	err := api.BlockUser(context.Background(), "user-123")
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// GetNetworkProperties
// ---------------------------------------------------------------------------

func TestGetNetworkProperties(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/NetworkInfo/Properties", r.URL.Path)
		assert.Equal(t, "1.0", r.Header.Get("Api-Version"))
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "network_properties.json"))
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	result, err := api.GetNetworkProperties(context.Background())
	require.NoError(t, err)
	assert.False(t, result.DataConstrained)
	assert.True(t, result.EnablesPremiumMessaging)
}

// ---------------------------------------------------------------------------
// GetMediaDownloadURL
// ---------------------------------------------------------------------------

func TestGetMediaDownloadURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/Message/Media/DownloadUrl", r.URL.Path)
		assert.Equal(t, "2.0", r.Header.Get("Api-Version"))
		assert.NotEmpty(t, r.URL.Query().Get("uuid"))
		assert.Equal(t, "ImageAvif", r.URL.Query().Get("mediaType"))

		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "media_download_url.json"))
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	result, err := api.GetMediaDownloadURL(
		context.Background(),
		uuid.MustParse(testMsgID),
		uuid.MustParse(testMediaID),
		uuid.MustParse(testMsgID),
		uuid.MustParse(testConvID),
		MediaTypeImageAvif,
	)
	require.NoError(t, err)
	assert.Equal(t, testS3DownloadURL, result.URL)
}

// ---------------------------------------------------------------------------
// Auto-refresh on expired token
// ---------------------------------------------------------------------------

func TestAutoRefreshOnExpiredToken(t *testing.T) {
	refreshCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Registration/App/Refresh" {
			refreshCalled = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(AppRegistrationResponse{
				InstanceID: testInstanceID,
				AccessAndRefreshToken: AccessAndRefreshToken{
					AccessToken:  "refreshed-token",
					RefreshToken: "new-refresh",
					ExpiresIn:    3600,
				},
			})
			return
		}
		// Verify the refreshed token is used
		assert.Equal(t, "Bearer refreshed-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Write(loadFixture(t, "network_properties.json"))
	}))
	defer server.Close()

	auth := newTestAuth(server.URL)
	auth.ExpiresAt = float64(time.Now().Unix()) - 100 // expired

	api := NewHermesAPI(auth)
	result, err := api.GetNetworkProperties(context.Background())
	require.NoError(t, err)
	assert.True(t, refreshCalled)
	assert.True(t, result.EnablesPremiumMessaging)
}

// ---------------------------------------------------------------------------
// API error handling
// ---------------------------------------------------------------------------

func TestAPIError_OnFailedRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte("not found"))
	}))
	defer server.Close()

	api := NewHermesAPI(newTestAuth(server.URL))
	_, err := api.GetNetworkProperties(context.Background())
	require.Error(t, err)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 404, apiErr.StatusCode)
	assert.Contains(t, apiErr.Body, "not found")
}
