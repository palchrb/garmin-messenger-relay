package garminmessenger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func fixtureDir() string {
	return filepath.Join("..", "..", "tests", "fixtures")
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir(), name))
	require.NoError(t, err, "loading fixture %s", name)
	return data
}

// Test constants matching Python conftest.py
const (
	testConvID          = "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	testMsgID           = "11111111-2222-3333-4444-555555555555"
	testParentMsgID     = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	testLastMsgID       = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	testMediaID         = "99999999-8888-7777-6666-555544443333"
	testInstanceID      = "test-instance-id-12345"
	testUserID          = "+15551234567"
	testRecipientID     = "+15559876543"
	testOtaUUID         = "22222222-3333-4444-5555-666677778888"
	testStatusUserUUID  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testUploadURL       = "https://s3.amazonaws.com/certus-media-manager-prod/"
	testS3Key           = "media/uploads/test-object-key"
	testS3DownloadURL   = "https://s3.amazonaws.com/certus-media-manager-prod/media/test.avif?AWSAccessKeyId=AKIATEST&Signature=testsig&Expires=9999999999"
	testUserIdentifier1 = "308812345678901"
	testUserIdentifier2 = "308812345678902"
	testDeviceInstanceID = "dddddddd-1111-2222-3333-444455556666"
)

func ptr[T any](v T) *T { return &v }

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

func TestDeviceTypeValues(t *testing.T) {
	assert.Equal(t, DeviceType("MessengerApp"), DeviceTypeMessengerApp)
	assert.Equal(t, DeviceType("inReach"), DeviceTypeInReach)
	assert.Equal(t, DeviceType("Unknown"), DeviceTypeUnknown)
	assert.Equal(t, DeviceType("External"), DeviceTypeExternal)
	assert.Equal(t, DeviceType("GarminOSApp"), DeviceTypeGarminOSApp)
}

func TestMessageStatusValues(t *testing.T) {
	expected := map[string]MessageStatus{
		"Initialized":    MessageStatusInitialized,
		"Processing":     MessageStatusProcessing,
		"Sent":           MessageStatusSent,
		"Delivered":      MessageStatusDelivered,
		"Read":           MessageStatusRead,
		"Undeliverable":  MessageStatusUndeliverable,
		"RetryableError": MessageStatusRetryableError,
		"Deleted":        MessageStatusDeleted,
		"Expired":        MessageStatusExpired,
		"Uninitialized":  MessageStatusUninitialized,
	}
	for val, constant := range expected {
		assert.Equal(t, MessageStatus(val), constant)
	}
}

func TestHermesMessageTypeValues(t *testing.T) {
	assert.Equal(t, HermesMessageType("Unknown"), HermesMessageTypeUnknown)
	assert.Equal(t, HermesMessageType("MapShare"), HermesMessageTypeMapShare)
	assert.Equal(t, HermesMessageType("ReferencePoint"), HermesMessageTypeReferencePoint)
}

func TestMediaTypeValues(t *testing.T) {
	assert.Equal(t, MediaType("ImageAvif"), MediaTypeImageAvif)
	assert.Equal(t, MediaType("AudioOgg"), MediaTypeAudioOgg)
}

// ---------------------------------------------------------------------------
// PhoneToHermesUserID
// ---------------------------------------------------------------------------

func TestPhoneToHermesUserID_KnownVector(t *testing.T) {
	assert.Equal(t, "11153808-b0a5-5f9b-bbcf-b35be7e4359e", PhoneToHermesUserID("+15555550100"))
}

func TestPhoneToHermesUserID_IsUUIDv5(t *testing.T) {
	result := PhoneToHermesUserID("+15551234567")
	u, err := uuid.Parse(result)
	require.NoError(t, err)
	assert.Equal(t, uuid.Version(5), u.Version())
}

func TestPhoneToHermesUserID_Deterministic(t *testing.T) {
	a := PhoneToHermesUserID("+15551234567")
	b := PhoneToHermesUserID("+15551234567")
	assert.Equal(t, a, b)
}

func TestPhoneToHermesUserID_DifferentPhonesDiffer(t *testing.T) {
	a := PhoneToHermesUserID("+15551234567")
	b := PhoneToHermesUserID("+15559876543")
	assert.NotEqual(t, a, b)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: message_simple.json
// ---------------------------------------------------------------------------

func TestFixtureMessageSimple(t *testing.T) {
	data := loadFixture(t, "message_simple.json")

	// Parse as ConversationMessageModel (simple fixture has conversationId but
	// we test that MessageModel also works)
	var msg MessageModel
	require.NoError(t, json.Unmarshal(data, &msg))

	assert.Equal(t, uuid.MustParse(testMsgID), msg.MessageID)
	assert.Equal(t, uuid.MustParse(testConvID), msg.ConversationID)
	assert.Equal(t, ptr("Hello from satellite!"), msg.MessageBody)
	assert.Equal(t, ptr(testUserID), msg.From)
	require.NotNil(t, msg.SentAt)
	assert.Equal(t, 2025, msg.SentAt.Year())
	assert.Equal(t, ptr(DeviceTypeMessengerApp), msg.FromDeviceType)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: message_full.json
// ---------------------------------------------------------------------------

func TestFixtureMessageFull(t *testing.T) {
	data := loadFixture(t, "message_full.json")

	var msg MessageModel
	require.NoError(t, json.Unmarshal(data, &msg))

	assert.Equal(t, uuid.MustParse(testMsgID), msg.MessageID)
	assert.Equal(t, uuid.MustParse(testConvID), msg.ConversationID)
	assert.Equal(t, ptr(uuid.MustParse(testParentMsgID)), msg.ParentMessageID)
	assert.Equal(t, ptr("Full message with all fields"), msg.MessageBody)
	assert.Equal(t, []string{testRecipientID}, msg.To)
	assert.Equal(t, ptr(testUserID), msg.From)

	// Timestamps
	require.NotNil(t, msg.SentAt)
	require.NotNil(t, msg.ReceivedAt)
	assert.Equal(t, 2025, msg.SentAt.Year())

	// Status receipts
	require.Len(t, msg.Status, 1)
	assert.Equal(t, testUserID, msg.Status[0].UserID)
	assert.Equal(t, MessageStatusDelivered, msg.Status[0].MessageStatus)

	// Location
	require.NotNil(t, msg.UserLocation)
	assert.Equal(t, ptr(45.5231), msg.UserLocation.LatitudeDegrees)
	assert.Equal(t, ptr(-122.6765), msg.UserLocation.LongitudeDegrees)
	assert.Equal(t, ptr(100.0), msg.UserLocation.ElevationMeters)
	assert.Equal(t, ptr(1.5), msg.UserLocation.GroundVelocityMetersPerSecond)
	assert.Equal(t, ptr(270.0), msg.UserLocation.CourseDegrees)

	// Reference point
	require.NotNil(t, msg.ReferencePoint)
	assert.Equal(t, ptr(46.0), msg.ReferencePoint.LatitudeDegrees)
	assert.Equal(t, ptr(-123.0), msg.ReferencePoint.LongitudeDegrees)
	assert.Nil(t, msg.ReferencePoint.ElevationMeters)

	// Enums
	assert.Equal(t, ptr(HermesMessageTypeMapShare), msg.MessageType)
	assert.Equal(t, ptr(DeviceTypeInReach), msg.FromDeviceType)
	assert.Equal(t, ptr(MediaTypeImageAvif), msg.MediaType)

	// Other fields
	assert.Equal(t, ptr("https://share.garmin.com/abc123"), msg.MapShareUrl)
	assert.Equal(t, ptr("secret"), msg.MapSharePassword)
	assert.Equal(t, ptr("https://livetrack.garmin.com/xyz"), msg.LiveTrackUrl)
	assert.Equal(t, ptr(uuid.MustParse(testMediaID)), msg.MediaID)

	// MediaMetadata
	require.NotNil(t, msg.MediaMetadata)
	assert.Equal(t, ptr(1920), msg.MediaMetadata.Width)
	assert.Equal(t, ptr(1080), msg.MediaMetadata.Height)
	assert.Nil(t, msg.MediaMetadata.DurationMs)

	// UUID fields
	assert.Equal(t, ptr(uuid.MustParse(testMsgID)), msg.UUID)
	assert.Equal(t, ptr("Voice message transcription text"), msg.Transcription)
	assert.Equal(t, ptr(uuid.MustParse(testOtaUUID)), msg.OtaUuid)
	assert.Equal(t, ptr("unit-001"), msg.FromUnitID)
	assert.Equal(t, ptr("unit-002"), msg.IntendedUnitID)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: conversation_detail.json
// ---------------------------------------------------------------------------

func TestFixtureConversationDetail(t *testing.T) {
	data := loadFixture(t, "conversation_detail.json")

	var detail ConversationDetailModel
	require.NoError(t, json.Unmarshal(data, &detail))

	// MetaData
	assert.Equal(t, uuid.MustParse(testConvID), detail.MetaData.ConversationID)
	assert.Equal(t, []string{testUserID, testRecipientID}, detail.MetaData.MemberIDs)
	assert.False(t, detail.MetaData.IsMuted)
	assert.False(t, detail.MetaData.IsPost)

	// Messages
	require.Len(t, detail.Messages, 2)
	assert.Equal(t, uuid.MustParse(testMsgID), detail.Messages[0].MessageID)
	assert.Equal(t, ptr("Hello!"), detail.Messages[0].MessageBody)
	assert.Equal(t, ptr(testUserID), detail.Messages[0].From)
	assert.Equal(t, ptr(DeviceTypeMessengerApp), detail.Messages[0].FromDeviceType)

	assert.Equal(t, uuid.MustParse(testLastMsgID), detail.Messages[1].MessageID)
	assert.Equal(t, ptr("Hi back!"), detail.Messages[1].MessageBody)
	assert.Equal(t, ptr(testRecipientID), detail.Messages[1].From)

	assert.Equal(t, 50, detail.Limit)
	assert.Equal(t, ptr(uuid.MustParse(testLastMsgID)), detail.LastMessageID)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: get_conversations.json
// ---------------------------------------------------------------------------

func TestFixtureGetConversations(t *testing.T) {
	data := loadFixture(t, "get_conversations.json")

	var result GetConversationsModel
	require.NoError(t, json.Unmarshal(data, &result))

	require.Len(t, result.Conversations, 1)
	assert.Equal(t, uuid.MustParse(testConvID), result.Conversations[0].ConversationID)
	assert.Equal(t, []string{testUserID, testRecipientID}, result.Conversations[0].MemberIDs)
	assert.Equal(t, ptr(uuid.MustParse(testConvID)), result.LastConversationID)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: conversation_members.json
// ---------------------------------------------------------------------------

func TestFixtureConversationMembers(t *testing.T) {
	data := loadFixture(t, "conversation_members.json")

	var members []UserInfoModel
	require.NoError(t, json.Unmarshal(data, &members))

	require.Len(t, members, 2)
	assert.Equal(t, ptr(testUserIdentifier1), members[0].UserIdentifier)
	assert.Equal(t, ptr(testUserID), members[0].Address)
	assert.Equal(t, ptr("Alice"), members[0].FriendlyName)
	assert.NotNil(t, members[0].ImageUrl)

	assert.Equal(t, ptr(testUserIdentifier2), members[1].UserIdentifier)
	assert.Equal(t, ptr(testRecipientID), members[1].Address)
	assert.Equal(t, ptr("Bob"), members[1].FriendlyName)
	assert.Nil(t, members[1].ImageUrl) // empty string → nil
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: muted_conversations.json
// ---------------------------------------------------------------------------

func TestFixtureMutedConversations(t *testing.T) {
	data := loadFixture(t, "muted_conversations.json")

	var items []ConversationMuteDetailModel
	require.NoError(t, json.Unmarshal(data, &items))

	require.Len(t, items, 2)
	assert.Equal(t, uuid.MustParse(testConvID), items[0].ConversationID)
	require.NotNil(t, items[0].Expires)
	assert.Equal(t, 2025, items[0].Expires.Year())

	assert.Equal(t, uuid.MustParse(testLastMsgID), items[1].ConversationID)
	assert.Nil(t, items[1].Expires)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: network_properties.json
// ---------------------------------------------------------------------------

func TestFixtureNetworkProperties(t *testing.T) {
	data := loadFixture(t, "network_properties.json")

	var props NetworkPropertiesResponse
	require.NoError(t, json.Unmarshal(data, &props))

	assert.False(t, props.DataConstrained)
	assert.True(t, props.EnablesPremiumMessaging)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: send_message_response.json
// ---------------------------------------------------------------------------

func TestFixtureSendMessageResponse(t *testing.T) {
	data := loadFixture(t, "send_message_response.json")

	var resp SendMessageV2Response
	require.NoError(t, json.Unmarshal(data, &resp))

	assert.Equal(t, uuid.MustParse(testMsgID), resp.MessageID)
	assert.Equal(t, uuid.MustParse(testConvID), resp.ConversationID)
	assert.Nil(t, resp.SignedUploadUrl)
	assert.Nil(t, resp.ImageQuality)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: send_message_media_response.json
// ---------------------------------------------------------------------------

func TestFixtureSendMessageMediaResponse(t *testing.T) {
	data := loadFixture(t, "send_message_media_response.json")

	var resp SendMessageV2Response
	require.NoError(t, json.Unmarshal(data, &resp))

	assert.Equal(t, uuid.MustParse(testMsgID), resp.MessageID)
	assert.Equal(t, uuid.MustParse(testConvID), resp.ConversationID)

	require.NotNil(t, resp.SignedUploadUrl)
	su := resp.SignedUploadUrl
	assert.Equal(t, testUploadURL, su.UploadUrl)
	assert.Equal(t, ptr(testS3Key), su.Key)
	assert.Equal(t, ptr("STANDARD"), su.XAmzStorageClass)
	assert.Equal(t, ptr("20250115T103000Z"), su.XAmzDate)
	assert.Equal(t, ptr("abcdef1234567890"), su.XAmzSignature)
	assert.Equal(t, ptr("AWS4-HMAC-SHA256"), su.XAmzAlgorithm)
	assert.Equal(t, ptr("AKIATEST/20250115/us-east-1/s3/aws4_request"), su.XAmzCredential)
	assert.Equal(t, ptr("eyJleHBpcmF0aW9uIjoiMjAyNS0wMS0xNVQxMjowMDowMFoifQ=="), su.Policy)
	assert.Equal(t, ptr("INTERNET"), su.XAmzMetaMediaQuality)
	assert.Equal(t, ptr("image/avif"), su.ContentType)

	assert.Equal(t, ptr("INTERNET"), resp.ImageQuality)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: batch_status_update_response.json
// ---------------------------------------------------------------------------

func TestFixtureBatchStatusUpdateResponse(t *testing.T) {
	data := loadFixture(t, "batch_status_update_response.json")

	var items []UpdateMessageStatusResponse
	require.NoError(t, json.Unmarshal(data, &items))

	require.Len(t, items, 2)
	assert.Equal(t, ptr(uuid.MustParse(testMsgID)), items[0].MessageID)
	assert.Equal(t, ptr(uuid.MustParse(testConvID)), items[0].ConversationID)
	assert.Equal(t, ptr(MessageStatusRead), items[0].Status)

	assert.Equal(t, ptr(uuid.MustParse(testLastMsgID)), items[1].MessageID)
	assert.Equal(t, ptr(MessageStatusDelivered), items[1].Status)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: updated_statuses.json
// ---------------------------------------------------------------------------

func TestFixtureUpdatedStatuses(t *testing.T) {
	data := loadFixture(t, "updated_statuses.json")

	var resp GetUpdatedStatusesResponse
	require.NoError(t, json.Unmarshal(data, &resp))

	require.Len(t, resp.StatusReceiptsForMessages, 1)
	msg := resp.StatusReceiptsForMessages[0]
	assert.Equal(t, uuid.MustParse(testMsgID), msg.MessageID)
	assert.Equal(t, uuid.MustParse(testConvID), msg.ConversationID)
	require.Len(t, msg.StatusReceipts, 1)
	assert.Equal(t, testUserID, msg.StatusReceipts[0].UserID)
	assert.Equal(t, MessageStatusRead, msg.StatusReceipts[0].MessageStatus)
	require.NotNil(t, msg.StatusReceipts[0].UpdatedAt)

	assert.Equal(t, ptr(uuid.MustParse(testMsgID)), resp.LastMessageID)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: device_metadata.json
// ---------------------------------------------------------------------------

func TestFixtureDeviceMetadata(t *testing.T) {
	data := loadFixture(t, "device_metadata.json")

	var items []MessageDeviceMetadataV2
	require.NoError(t, json.Unmarshal(data, &items))

	require.Len(t, items, 1)
	md := items[0]
	assert.True(t, md.HasAllMtDeviceMetadata)
	require.NotNil(t, md.DeviceMetadata)
	assert.Equal(t, ptr(testUserID), md.DeviceMetadata.UserID)
	require.NotNil(t, md.DeviceMetadata.MessageID)
	assert.Equal(t, uuid.MustParse(testMsgID), md.DeviceMetadata.MessageID.MessageID)
	assert.Equal(t, uuid.MustParse(testConvID), md.DeviceMetadata.MessageID.ConversationID)

	require.Len(t, md.DeviceMetadata.DeviceMessageMetadata, 1)
	dev := md.DeviceMetadata.DeviceMessageMetadata[0]
	assert.Equal(t, ptr(uuid.MustParse(testDeviceInstanceID)), dev.DeviceInstanceID)
	assert.Equal(t, ptr(int64(300234063904190)), dev.IMEI)
	require.Len(t, dev.InReachMessageMetadata, 1)
	sat := dev.InReachMessageMetadata[0]
	assert.Equal(t, ptr(42), sat.Mtmsn)
	assert.Equal(t, ptr("inReach Mini 2"), sat.Text)
	assert.Equal(t, ptr(uuid.MustParse(testOtaUUID)), sat.OtaUuid)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: media_download_url.json
// ---------------------------------------------------------------------------

func TestFixtureMediaDownloadUrl(t *testing.T) {
	data := loadFixture(t, "media_download_url.json")

	var resp MediaAttachmentDownloadUrlResponse
	require.NoError(t, json.Unmarshal(data, &resp))

	assert.Equal(t, testS3DownloadURL, resp.URL)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: registration_confirm.json
// ---------------------------------------------------------------------------

func TestFixtureRegistrationConfirm(t *testing.T) {
	data := loadFixture(t, "registration_confirm.json")

	var resp AppRegistrationResponse
	require.NoError(t, json.Unmarshal(data, &resp))

	assert.Equal(t, testInstanceID, resp.InstanceID)
	assert.Equal(t, "eyJhbGciOiJFUzI1NiJ9.test.payload", resp.AccessAndRefreshToken.AccessToken)
	assert.Equal(t, "refresh-token-test-value", resp.AccessAndRefreshToken.RefreshToken)
	assert.Equal(t, 3600, resp.AccessAndRefreshToken.ExpiresIn)
	require.NotNil(t, resp.SmsOptInResult)
	assert.Equal(t, ptr(true), resp.SmsOptInResult.Success)
	assert.Equal(t, ptr(false), resp.SmsOptInResult.FatalError)
}

// ---------------------------------------------------------------------------
// Fixture-driven tests: signalr_status_update.json
// ---------------------------------------------------------------------------

func TestFixtureSignalRStatusUpdate(t *testing.T) {
	data := loadFixture(t, "signalr_status_update.json")

	var update MessageStatusUpdate
	require.NoError(t, json.Unmarshal(data, &update))

	assert.Equal(t, uuid.MustParse(testMsgID), update.MessageID.MessageID)
	assert.Equal(t, uuid.MustParse(testConvID), update.MessageID.ConversationID)
	// The fixture uses "status" key, not "messageStatus"
	assert.Equal(t, ptr(MessageStatusDelivered), update.MessageStatus)
	assert.Equal(t, ptr(uuid.MustParse(testStatusUserUUID)), update.UserID)
	require.NotNil(t, update.UpdatedAt)
}

// ---------------------------------------------------------------------------
// SignedUploadUrl content-type case variants
// ---------------------------------------------------------------------------

func TestSignedUploadUrl_ContentTypeCaseVariants(t *testing.T) {
	variants := []string{"content-type", "Content-type", "content-Type", "Content-Type"}
	for _, key := range variants {
		t.Run(key, func(t *testing.T) {
			data := `{"uploadUrl":"https://s3.example.com/","` + key + `":"audio/ogg"}`
			var su SignedUploadUrl
			require.NoError(t, json.Unmarshal([]byte(data), &su))
			assert.Equal(t, ptr("audio/ogg"), su.ContentType, "Failed for alias %q", key)
		})
	}
}

// ---------------------------------------------------------------------------
// MessageStatusUpdate: both "status" and "messageStatus" keys
// ---------------------------------------------------------------------------

func TestMessageStatusUpdate_NativeKey(t *testing.T) {
	data := `{"messageId":{"messageId":"` + testMsgID + `","conversationId":"` + testConvID + `"},"messageStatus":"Read"}`
	var update MessageStatusUpdate
	require.NoError(t, json.Unmarshal([]byte(data), &update))
	assert.Equal(t, ptr(MessageStatusRead), update.MessageStatus)
}

func TestMessageStatusUpdate_StatusKeyFallback(t *testing.T) {
	data := `{"messageId":{"messageId":"` + testMsgID + `","conversationId":"` + testConvID + `"},"status":"Delivered"}`
	var update MessageStatusUpdate
	require.NoError(t, json.Unmarshal([]byte(data), &update))
	assert.Equal(t, ptr(MessageStatusDelivered), update.MessageStatus)
}

func TestMessageStatusUpdate_MessageStatusTakesPrecedence(t *testing.T) {
	data := `{"messageId":{"messageId":"` + testMsgID + `","conversationId":"` + testConvID + `"},"status":"Delivered","messageStatus":"Read"}`
	var update MessageStatusUpdate
	require.NoError(t, json.Unmarshal([]byte(data), &update))
	assert.Equal(t, ptr(MessageStatusRead), update.MessageStatus)
}

// ---------------------------------------------------------------------------
// ConversationMessageModel lacks conversationId/to/mediaMetadata
// ---------------------------------------------------------------------------

func TestConversationMessageModel_FromKeyword(t *testing.T) {
	data := `{"messageId":"` + testMsgID + `","from":"` + testUserID + `"}`
	var msg ConversationMessageModel
	require.NoError(t, json.Unmarshal([]byte(data), &msg))
	assert.Equal(t, ptr(testUserID), msg.From)
}

// ---------------------------------------------------------------------------
// SendMessageRequest: explicit null serialization
// ---------------------------------------------------------------------------

func TestSendMessageRequest_ExplicitNull(t *testing.T) {
	req := SendMessageRequest{
		To:          []string{testRecipientID},
		MessageBody: "Test",
	}
	data, err := json.Marshal(req)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))
	// userLocation and referencePoint must be present as null
	assert.Contains(t, raw, "userLocation")
	assert.Contains(t, raw, "referencePoint")

	assert.Equal(t, json.RawMessage("null"), raw["userLocation"])
	assert.Equal(t, json.RawMessage("null"), raw["referencePoint"])
}

// ---------------------------------------------------------------------------
// UserLocation defaults
// ---------------------------------------------------------------------------

func TestUserLocation_AllOptional(t *testing.T) {
	var loc UserLocation
	require.NoError(t, json.Unmarshal([]byte(`{}`), &loc))
	assert.Nil(t, loc.LatitudeDegrees)
	assert.Nil(t, loc.LongitudeDegrees)
	assert.Nil(t, loc.ElevationMeters)
}

// ---------------------------------------------------------------------------
// DateTime parsing
// ---------------------------------------------------------------------------

func TestDateTimeParsing(t *testing.T) {
	data := `{"messageId":"` + testMsgID + `","conversationId":"` + testConvID + `","sentAt":"2025-01-15T10:30:00Z","receivedAt":"2025-01-15T10:30:05Z"}`
	var msg MessageModel
	require.NoError(t, json.Unmarshal([]byte(data), &msg))
	require.NotNil(t, msg.SentAt)
	require.NotNil(t, msg.ReceivedAt)
	assert.Equal(t, 2025, msg.SentAt.Year())
	assert.Equal(t, time.Month(1), msg.SentAt.Month())
	assert.Equal(t, 15, msg.SentAt.Day())
}

// ---------------------------------------------------------------------------
// MessageModel field name variants (REST API vs FCM)
// ---------------------------------------------------------------------------

func TestMessageModel_UnmarshalJSON_RestAPI(t *testing.T) {
	// REST API uses messageId, conversationId, parentMessageId
	data := `{
		"messageId": "11111111-2222-3333-4444-555555555555",
		"conversationId": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		"parentMessageId": "66666666-7777-8888-9999-aaaaaaaaaaaa",
		"messageBody": "Hello from REST API"
	}`

	var msg MessageModel
	require.NoError(t, json.Unmarshal([]byte(data), &msg))

	assert.Equal(t, uuid.MustParse("11111111-2222-3333-4444-555555555555"), msg.MessageID)
	assert.Equal(t, uuid.MustParse("a1b2c3d4-e5f6-7890-abcd-ef1234567890"), msg.ConversationID)
	require.NotNil(t, msg.ParentMessageID)
	assert.Equal(t, uuid.MustParse("66666666-7777-8888-9999-aaaaaaaaaaaa"), *msg.ParentMessageID)
	require.NotNil(t, msg.MessageBody)
	assert.Equal(t, "Hello from REST API", *msg.MessageBody)
}

func TestMessageModel_UnmarshalJSON_FCM(t *testing.T) {
	// FCM push notifications use messageGuid, conversationGuid, parentMessageGuid
	data := `{
		"messageGuid": "11111111-2222-3333-4444-555555555555",
		"conversationGuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		"parentMessageGuid": "66666666-7777-8888-9999-aaaaaaaaaaaa",
		"messageBody": "Hello from FCM"
	}`

	var msg MessageModel
	require.NoError(t, json.Unmarshal([]byte(data), &msg))

	assert.Equal(t, uuid.MustParse("11111111-2222-3333-4444-555555555555"), msg.MessageID)
	assert.Equal(t, uuid.MustParse("a1b2c3d4-e5f6-7890-abcd-ef1234567890"), msg.ConversationID)
	require.NotNil(t, msg.ParentMessageID)
	assert.Equal(t, uuid.MustParse("66666666-7777-8888-9999-aaaaaaaaaaaa"), *msg.ParentMessageID)
	require.NotNil(t, msg.MessageBody)
	assert.Equal(t, "Hello from FCM", *msg.MessageBody)
}

func TestMessageModel_UnmarshalJSON_GuidOverridesId(t *testing.T) {
	// When both variants are present, Guid fields should take precedence
	data := `{
		"messageId": "00000000-0000-0000-0000-000000000000",
		"messageGuid": "11111111-2222-3333-4444-555555555555",
		"conversationId": "00000000-0000-0000-0000-000000000000",
		"conversationGuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		"messageBody": "Guid wins"
	}`

	var msg MessageModel
	require.NoError(t, json.Unmarshal([]byte(data), &msg))

	// Guid fields should override Id fields
	assert.Equal(t, uuid.MustParse("11111111-2222-3333-4444-555555555555"), msg.MessageID)
	assert.Equal(t, uuid.MustParse("a1b2c3d4-e5f6-7890-abcd-ef1234567890"), msg.ConversationID)
}

func TestMessageModel_UnmarshalJSON_EmptyStringGuid(t *testing.T) {
	// FCM sends empty strings for missing UUIDs (e.g., no parent message)
	data := `{
		"messageGuid": "11111111-2222-3333-4444-555555555555",
		"conversationGuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		"parentMessageGuid": "",
		"messageBody": "No parent"
	}`

	var msg MessageModel
	require.NoError(t, json.Unmarshal([]byte(data), &msg))

	assert.Equal(t, uuid.MustParse("11111111-2222-3333-4444-555555555555"), msg.MessageID)
	assert.Equal(t, uuid.MustParse("a1b2c3d4-e5f6-7890-abcd-ef1234567890"), msg.ConversationID)
	assert.Nil(t, msg.ParentMessageID) // Empty string should be treated as nil
	require.NotNil(t, msg.MessageBody)
	assert.Equal(t, "No parent", *msg.MessageBody)
}

// ---------------------------------------------------------------------------
// NetworkPropertiesResponse defaults
// ---------------------------------------------------------------------------

func TestNetworkPropertiesResponse_Defaults(t *testing.T) {
	var resp NetworkPropertiesResponse
	require.NoError(t, json.Unmarshal([]byte(`{}`), &resp))
	assert.False(t, resp.DataConstrained)
	assert.False(t, resp.EnablesPremiumMessaging)
}
