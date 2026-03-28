package garminmessenger

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// HermesUserNamespace is the UUID v5 namespace used by Garmin Messenger
// to derive user identifiers from phone numbers.
var HermesUserNamespace = uuid.Must(uuid.Parse("65F85187-FAE9-4211-90D9-8F534AFA231B"))

// PhoneToHermesUserID derives the Hermes user UUID from a phone number
// using UUID v5 with the Hermes namespace.
func PhoneToHermesUserID(phone string) string {
	return uuid.NewSHA1(HermesUserNamespace, []byte(phone)).String()
}

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

// DeviceType represents the type of device a message was sent from.
type DeviceType string

const (
	DeviceTypeMessengerApp DeviceType = "MessengerApp"
	DeviceTypeInReach      DeviceType = "inReach"
	DeviceTypeUnknown      DeviceType = "Unknown"
	DeviceTypeExternal     DeviceType = "External"
	DeviceTypeGarminOSApp  DeviceType = "GarminOSApp"
)

// MessageStatus represents the delivery status of a message.
type MessageStatus string

const (
	MessageStatusInitialized    MessageStatus = "Initialized"
	MessageStatusProcessing     MessageStatus = "Processing"
	MessageStatusSent           MessageStatus = "Sent"
	MessageStatusDelivered      MessageStatus = "Delivered"
	MessageStatusRead           MessageStatus = "Read"
	MessageStatusUndeliverable  MessageStatus = "Undeliverable"
	MessageStatusRetryableError MessageStatus = "RetryableError"
	MessageStatusDeleted        MessageStatus = "Deleted"
	MessageStatusExpired        MessageStatus = "Expired"
	MessageStatusUninitialized  MessageStatus = "Uninitialized"
)

// HermesMessageType represents the type of a Hermes message.
type HermesMessageType string

const (
	HermesMessageTypeUnknown        HermesMessageType = "Unknown"
	HermesMessageTypeMapShare       HermesMessageType = "MapShare"
	HermesMessageTypeReferencePoint HermesMessageType = "ReferencePoint"
	// HermesMessageTypeReaction is set on reaction messages.
	// Garmin stores reactions separately in the Android database and sends
	// them with this messageType alongside ParentMessageID identifying the
	// reacted-to message.
	HermesMessageTypeReaction HermesMessageType = "Reaction"
)

// IsReaction reports whether this message is a reaction.
func (t *HermesMessageType) IsReaction() bool {
	return t != nil && *t == HermesMessageTypeReaction
}

// MediaType represents the type of a media attachment.
type MediaType string

const (
	MediaTypeImageAvif MediaType = "ImageAvif"
	MediaTypeAudioOgg  MediaType = "AudioOgg"
)

// ---------------------------------------------------------------------------
// Shared sub-models
// ---------------------------------------------------------------------------

// UserLocation represents GPS coordinates and motion data.
type UserLocation struct {
	LatitudeDegrees               *float64 `json:"latitudeDegrees,omitempty"`
	LongitudeDegrees              *float64 `json:"longitudeDegrees,omitempty"`
	ElevationMeters               *float64 `json:"elevationMeters,omitempty"`
	GroundVelocityMetersPerSecond *float64 `json:"groundVelocityMetersPerSecond,omitempty"`
	CourseDegrees                 *float64 `json:"courseDegrees,omitempty"`
}

// StatusReceipt represents a delivery/read receipt for a message.
type StatusReceipt struct {
	UserID                string        `json:"userId"`
	AppOrDeviceInstanceID *string       `json:"appOrDeviceInstanceId,omitempty"`
	DeviceType            *DeviceType   `json:"deviceType,omitempty"`
	MessageStatus         MessageStatus `json:"messageStatus"`
	UpdatedAt             *time.Time    `json:"updatedAt,omitempty"`
}

// SimpleCompoundMessageId identifies a message within a conversation.
type SimpleCompoundMessageId struct {
	MessageID      uuid.UUID `json:"messageId"`
	ConversationID uuid.UUID `json:"conversationId"`
}

// MediaMetadata contains dimensions and duration for media attachments.
type MediaMetadata struct {
	Width      *int `json:"width,omitempty"`
	Height     *int `json:"height,omitempty"`
	DurationMs *int `json:"durationMs,omitempty"`
}

// SignedUploadUrl contains AWS S3 presigned upload parameters.
type SignedUploadUrl struct {
	UploadUrl            string  `json:"uploadUrl"`
	Key                  *string `json:"key,omitempty"`
	XAmzStorageClass     *string `json:"x-amz-storage-class,omitempty"`
	XAmzDate             *string `json:"x-amz-date,omitempty"`
	XAmzSignature        *string `json:"x-amz-signature,omitempty"`
	XAmzAlgorithm        *string `json:"x-amz-algorithm,omitempty"`
	XAmzCredential       *string `json:"x-amz-credential,omitempty"`
	Policy               *string `json:"policy,omitempty"`
	XAmzMetaMediaQuality *string `json:"x-amz-meta-media-quality,omitempty"`
	ContentType          *string `json:"content-type,omitempty"`
}

// UnmarshalJSON handles case-insensitive content-type field variants.
func (s *SignedUploadUrl) UnmarshalJSON(data []byte) error {
	// Use a map to find content-type regardless of case
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Normalize content-type: check all case variants
	var contentTypeVal json.RawMessage
	for key, val := range raw {
		if strings.EqualFold(key, "content-type") {
			contentTypeVal = val
			// Remove all content-type variants so they don't interfere
			delete(raw, key)
		}
	}

	// Re-marshal the cleaned map and decode into an alias type
	type Alias SignedUploadUrl
	cleaned, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var alias Alias
	if err := json.Unmarshal(cleaned, &alias); err != nil {
		return err
	}
	*s = SignedUploadUrl(alias)

	// Set content-type from the normalized value
	if contentTypeVal != nil {
		var ct string
		if err := json.Unmarshal(contentTypeVal, &ct); err == nil {
			s.ContentType = &ct
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Message models
// ---------------------------------------------------------------------------

// MessageModel is the full message format returned by SignalR ReceiveMessage
// and some REST endpoints.
type MessageModel struct {
	MessageID        uuid.UUID          `json:"messageId"`
	ConversationID   uuid.UUID          `json:"conversationId"`
	ParentMessageID  *uuid.UUID         `json:"parentMessageId,omitempty"`
	MessageBody      *string            `json:"messageBody,omitempty"`
	To               []string           `json:"to,omitempty"`
	From             *string            `json:"from,omitempty"`
	SentAt           *time.Time         `json:"sentAt,omitempty"`
	ReceivedAt       *time.Time         `json:"receivedAt,omitempty"`
	Status           []StatusReceipt    `json:"status,omitempty"`
	UserLocation     *UserLocation      `json:"userLocation,omitempty"`
	ReferencePoint   *UserLocation      `json:"referencePoint,omitempty"`
	MessageType      *HermesMessageType `json:"messageType,omitempty"`
	MapShareUrl      *string            `json:"mapShareUrl,omitempty"`
	MapSharePassword *string            `json:"mapSharePassword,omitempty"`
	LiveTrackUrl     *string            `json:"liveTrackUrl,omitempty"`
	FromDeviceType   *DeviceType        `json:"fromDeviceType,omitempty"`
	MediaID          *uuid.UUID         `json:"mediaId,omitempty"`
	MediaType        *MediaType         `json:"mediaType,omitempty"`
	MediaMetadata    *MediaMetadata     `json:"mediaMetadata,omitempty"`
	UUID             *uuid.UUID         `json:"uuid,omitempty"`
	Transcription    *string            `json:"transcription,omitempty"`
	OtaUuid          *uuid.UUID         `json:"otaUuid,omitempty"`
	FromUnitID       *string            `json:"fromUnitId,omitempty"`
	IntendedUnitID   *string            `json:"intendedUnitId,omitempty"`
}

// UnmarshalJSON handles both REST API field names (messageId, conversationId)
// and FCM push notification field names (messageGuid, conversationGuid).
// It also handles empty string UUIDs from FCM (treats them as nil).
func (m *MessageModel) UnmarshalJSON(data []byte) error {
	type Alias MessageModel
	aux := &struct {
		MessageGuid       *string `json:"messageGuid"`
		ConversationGuid  *string `json:"conversationGuid"`
		ParentMessageGuid *string `json:"parentMessageGuid"`
		*Alias
	}{
		Alias: (*Alias)(m),
	}

	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}

	// FCM push notifications use *Guid field names, REST API uses *Id field names.
	// Prefer Guid variants if present (they will override the Id variants that were
	// already parsed into the embedded Alias).
	// Handle empty strings as nil (FCM sends "" for missing parent messages).
	if aux.MessageGuid != nil && *aux.MessageGuid != "" {
		parsed, err := uuid.Parse(*aux.MessageGuid)
		if err != nil {
			return fmt.Errorf("parsing messageGuid: %w", err)
		}
		m.MessageID = parsed
	}
	if aux.ConversationGuid != nil && *aux.ConversationGuid != "" {
		parsed, err := uuid.Parse(*aux.ConversationGuid)
		if err != nil {
			return fmt.Errorf("parsing conversationGuid: %w", err)
		}
		m.ConversationID = parsed
	}
	if aux.ParentMessageGuid != nil && *aux.ParentMessageGuid != "" {
		parsed, err := uuid.Parse(*aux.ParentMessageGuid)
		if err != nil {
			return fmt.Errorf("parsing parentMessageGuid: %w", err)
		}
		m.ParentMessageID = &parsed
	}

	return nil
}

// ConversationMessageModel is a message within a conversation detail response.
// It lacks ConversationID, To, and MediaMetadata fields.
type ConversationMessageModel struct {
	MessageID        uuid.UUID          `json:"messageId"`
	ParentMessageID  *uuid.UUID         `json:"parentMessageId,omitempty"`
	MessageBody      *string            `json:"messageBody,omitempty"`
	From             *string            `json:"from,omitempty"`
	SentAt           *time.Time         `json:"sentAt,omitempty"`
	ReceivedAt       *time.Time         `json:"receivedAt,omitempty"`
	Status           []StatusReceipt    `json:"status,omitempty"`
	UserLocation     *UserLocation      `json:"userLocation,omitempty"`
	ReferencePoint   *UserLocation      `json:"referencePoint,omitempty"`
	MessageType      *HermesMessageType `json:"messageType,omitempty"`
	MapShareUrl      *string            `json:"mapShareUrl,omitempty"`
	MapSharePassword *string            `json:"mapSharePassword,omitempty"`
	LiveTrackUrl     *string            `json:"liveTrackUrl,omitempty"`
	FromDeviceType   *DeviceType        `json:"fromDeviceType,omitempty"`
	MediaID          *uuid.UUID         `json:"mediaId,omitempty"`
	MediaType        *MediaType         `json:"mediaType,omitempty"`
	UUID             *uuid.UUID         `json:"uuid,omitempty"`
	Transcription    *string            `json:"transcription,omitempty"`
	OtaUuid          *uuid.UUID         `json:"otaUuid,omitempty"`
	FromUnitID       *string            `json:"fromUnitId,omitempty"`
	IntendedUnitID   *string            `json:"intendedUnitId,omitempty"`
}

// ---------------------------------------------------------------------------
// Conversation models
// ---------------------------------------------------------------------------

// ConversationMetaModel contains metadata about a conversation.
type ConversationMetaModel struct {
	ConversationID uuid.UUID `json:"conversationId"`
	MemberIDs      []string  `json:"memberIds"`
	UpdatedDate    time.Time `json:"updatedDate"`
	CreatedDate    time.Time `json:"createdDate"`
	IsMuted        bool      `json:"isMuted"`
	IsPost         bool      `json:"isPost"`
}

// ConversationDetailModel contains a conversation's metadata and messages.
type ConversationDetailModel struct {
	MetaData      ConversationMetaModel      `json:"metaData"`
	Messages      []ConversationMessageModel `json:"messages"`
	Limit         int                        `json:"limit"`
	LastMessageID *uuid.UUID                 `json:"lastMessageId,omitempty"`
}

// GetConversationsModel is the response from GET Conversation/Updated.
type GetConversationsModel struct {
	Conversations      []ConversationMetaModel `json:"conversations"`
	LastConversationID *uuid.UUID              `json:"lastConversationId,omitempty"`
}

// UserInfoModel represents member info within a conversation.
type UserInfoModel struct {
	UserIdentifier *string `json:"userIdentifier,omitempty"`
	Address        *string `json:"address,omitempty"`
	FriendlyName   *string `json:"friendlyName,omitempty"`
	ImageUrl       *string `json:"imageUrl,omitempty"`
}

// UnmarshalJSON handles empty imageUrl strings as nil.
func (u *UserInfoModel) UnmarshalJSON(data []byte) error {
	type Alias UserInfoModel
	var alias Alias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*u = UserInfoModel(alias)
	// Treat empty imageUrl as nil to match Python behavior
	if u.ImageUrl != nil && *u.ImageUrl == "" {
		u.ImageUrl = nil
	}
	return nil
}

// ConversationMuteDetailModel is an entry from GET Conversation/Muted.
type ConversationMuteDetailModel struct {
	ConversationID uuid.UUID  `json:"conversationId"`
	Expires        *time.Time `json:"expires,omitempty"`
}

// ---------------------------------------------------------------------------
// Request / response models
// ---------------------------------------------------------------------------

// SendMessageRequest is the request body for POST Message/Send.
// UserLocation and ReferencePoint serialize as explicit null when nil
// (server rejects requests with those fields missing entirely).
type SendMessageRequest struct {
	To              []string           `json:"to"`
	MessageBody     string             `json:"messageBody"`
	UserLocation    *UserLocation      `json:"userLocation"`
	ReferencePoint  *UserLocation      `json:"referencePoint"`
	MessageType     *HermesMessageType `json:"messageType"`
	IsPost          bool               `json:"isPost"`
	MediaID         *uuid.UUID         `json:"mediaId"`
	MediaType       *MediaType         `json:"mediaType"`
	MediaMetadata   *MediaMetadata     `json:"mediaMetadata,omitempty"`
	UUID            *uuid.UUID         `json:"uuid"`
	OtaUuid         *uuid.UUID         `json:"otaUuid"`
	// ParentMessageID is set when sending a reaction to identify the target message.
	ParentMessageID *uuid.UUID         `json:"parentMessageId,omitempty"`
}

// SendMessageV2Response is the response from POST Message/Send v2.
type SendMessageV2Response struct {
	MessageID       uuid.UUID        `json:"messageId"`
	ConversationID  uuid.UUID        `json:"conversationId"`
	SignedUploadUrl *SignedUploadUrl `json:"signedUploadUrl,omitempty"`
	ImageQuality    *string          `json:"imageQuality,omitempty"`
}

// UpdateMessageStatusResponse is the response from status update endpoints.
type UpdateMessageStatusResponse struct {
	MessageID      *uuid.UUID     `json:"messageId,omitempty"`
	ConversationID *uuid.UUID     `json:"conversationId,omitempty"`
	Status         *MessageStatus `json:"status,omitempty"`
}

// UpdateMessageStatusRequest is a single item for PUT Status/UpdateMessageStatuses.
type UpdateMessageStatusRequest struct {
	MessageID      uuid.UUID     `json:"messageId"`
	ConversationID uuid.UUID     `json:"conversationId"`
	MessageStatus  MessageStatus `json:"messageStatus"`
}

// StatusReceiptsForMessage contains status receipts for a single message.
type StatusReceiptsForMessage struct {
	MessageID      uuid.UUID       `json:"messageId"`
	ConversationID uuid.UUID       `json:"conversationId"`
	StatusReceipts []StatusReceipt `json:"statusReceipts,omitempty"`
}

// GetUpdatedStatusesResponse is the response from GET Status/Updated.
type GetUpdatedStatusesResponse struct {
	StatusReceiptsForMessages []StatusReceiptsForMessage `json:"statusReceiptsForMessages"`
	LastMessageID             *uuid.UUID                 `json:"lastMessageId,omitempty"`
}

// UpdateMediaRequest is the request body for POST Message/UpdateMedia.
type UpdateMediaRequest struct {
	MediaType      MediaType  `json:"mediaType"`
	MediaID        uuid.UUID  `json:"mediaId"`
	MessageID      *uuid.UUID `json:"messageId,omitempty"`
	ConversationID *uuid.UUID `json:"conversationId,omitempty"`
}

// UpdateMediaResponse is the response from POST Message/UpdateMedia.
type UpdateMediaResponse struct {
	SignedUploadUrl SignedUploadUrl `json:"signedUploadUrl"`
	ImageQuality    *string         `json:"imageQuality,omitempty"`
}

// MediaAttachmentDownloadUrlResponse is the response from GET Message/Media/DownloadUrl.
type MediaAttachmentDownloadUrlResponse struct {
	URL string `json:"downloadUrl"`
}

// InReachMessageMetadata contains satellite message metadata.
type InReachMessageMetadata struct {
	MessageID *uuid.UUID `json:"messageId,omitempty"`
	Mtmsn     *int       `json:"mtmsn,omitempty"`
	Text      *string    `json:"text,omitempty"`
	OtaUuid   *uuid.UUID `json:"otaUuid,omitempty"`
}

// DeviceInstanceMetadata is per-device metadata for a physical device.
type DeviceInstanceMetadata struct {
	DeviceInstanceID       *uuid.UUID               `json:"deviceInstanceId,omitempty"`
	IMEI                   *int64                   `json:"imei,omitempty"`
	InReachMessageMetadata []InReachMessageMetadata `json:"inReachMessageMetadata,omitempty"`
}

// DeviceMetadataEntry is the inner deviceMetadata object.
type DeviceMetadataEntry struct {
	UserID                *string                  `json:"userId,omitempty"`
	MessageID             *SimpleCompoundMessageId `json:"messageId,omitempty"`
	DeviceMessageMetadata []DeviceInstanceMetadata `json:"deviceMessageMetadata,omitempty"`
}

// MessageDeviceMetadataV2 is the top-level item from POST Message/DeviceMetadata.
type MessageDeviceMetadataV2 struct {
	HasAllMtDeviceMetadata bool                 `json:"hasAllMtDeviceMetadata"`
	DeviceMetadata         *DeviceMetadataEntry `json:"deviceMetadata,omitempty"`
}

// NetworkPropertiesResponse is the response from GET NetworkInfo/Properties.
type NetworkPropertiesResponse struct {
	DataConstrained         bool `json:"dataConstrained"`
	EnablesPremiumMessaging bool `json:"enablesPremiumMessaging"`
}

// UpdateAppPnsHandleBody is the request body for PATCH Registration/App.
type UpdateAppPnsHandleBody struct {
	PnsHandle      string `json:"pnsHandle"`
	PnsEnvironment string `json:"pnsEnvironment"`
	AppDescription string `json:"appDescription"`
}

// ---------------------------------------------------------------------------
// Auth models
// ---------------------------------------------------------------------------

// AccessAndRefreshToken contains the JWT tokens from registration.
type AccessAndRefreshToken struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int    `json:"expiresIn"`
}

// NewAppRegistrationBody is the request body for POST Registration/App.
type NewAppRegistrationBody struct {
	SmsNumber string `json:"smsNumber"`
	Platform  string `json:"platform"`
}

// NewAppRegistrationResponse is the response from POST Registration/App.
type NewAppRegistrationResponse struct {
	RequestID         string  `json:"requestId"`
	ValidUntil        *string `json:"validUntil,omitempty"`
	AttemptsRemaining *int    `json:"attemptsRemaining,omitempty"`
}

// OtpRequest is a pending OTP request returned by HermesAuth.RequestOTP.
type OtpRequest struct {
	RequestID         string  `json:"request_id"`
	PhoneNumber       string  `json:"phone_number"`
	DeviceName        string  `json:"device_name"`
	ValidUntil        *string `json:"valid_until,omitempty"`
	AttemptsRemaining *int    `json:"attempts_remaining,omitempty"`
}

// ConfirmAppRegistrationBody is the request for POST Registration/App/Confirm.
type ConfirmAppRegistrationBody struct {
	RequestID        string `json:"requestId"`
	SmsNumber        string `json:"smsNumber"`
	VerificationCode string `json:"verificationCode"`
	Platform         string `json:"platform"`
	PnsHandle        string `json:"pnsHandle"`
	PnsEnvironment   string `json:"pnsEnvironment"`
	AppDescription   string `json:"appDescription"`
	OptInForSms      bool   `json:"optInForSms"`
}

// SmsOptInResult contains the SMS opt-in result from registration.
type SmsOptInResult struct {
	Success    *bool `json:"success,omitempty"`
	FatalError *bool `json:"fatalError,omitempty"`
}

// AppRegistrationResponse is the response from POST Registration/App/Confirm.
type AppRegistrationResponse struct {
	InstanceID            string                `json:"instanceId"`
	AccessAndRefreshToken AccessAndRefreshToken `json:"accessAndRefreshToken"`
	SmsOptInResult        *SmsOptInResult       `json:"smsOptInResult,omitempty"`
}

// RefreshAuthBody is the request for POST Registration/App/Refresh.
type RefreshAuthBody struct {
	RefreshToken string `json:"refreshToken"`
	InstanceID   string `json:"instanceId"`
}

// ---------------------------------------------------------------------------
// SignalR event models
// ---------------------------------------------------------------------------

// MessageStatusUpdate is a SignalR status change event.
type MessageStatusUpdate struct {
	MessageID        SimpleCompoundMessageId `json:"messageId"`
	UserID           *uuid.UUID              `json:"userId,omitempty"`
	DeviceInstanceID *uuid.UUID              `json:"deviceInstanceId,omitempty"`
	DeviceType       *DeviceType             `json:"deviceType,omitempty"`
	MessageStatus    *MessageStatus          `json:"messageStatus,omitempty"`
	UpdatedAt        *time.Time              `json:"updatedAt,omitempty"`
}

// UnmarshalJSON handles both "status" and "messageStatus" keys,
// preferring "messageStatus" when both are present.
func (m *MessageStatusUpdate) UnmarshalJSON(data []byte) error {
	type Alias MessageStatusUpdate
	var alias Alias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*m = MessageStatusUpdate(alias)

	// If messageStatus is nil, check for "status" key
	if m.MessageStatus == nil {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		if statusVal, ok := raw["status"]; ok {
			var status MessageStatus
			if err := json.Unmarshal(statusVal, &status); err == nil {
				m.MessageStatus = &status
			}
		}
	}

	return nil
}

// ConversationMuteStatusUpdate is a SignalR mute status change event.
type ConversationMuteStatusUpdate struct {
	ConversationID *uuid.UUID `json:"conversationId,omitempty"`
	IsMuted        *bool      `json:"isMuted,omitempty"`
}

// UserBlockStatusUpdate is a SignalR user block status change event.
type UserBlockStatusUpdate struct {
	UserID    *string `json:"userId,omitempty"`
	IsBlocked *bool   `json:"isBlocked,omitempty"`
}

// ServerNotification is a SignalR server notification event.
type ServerNotification struct {
	NotificationType *string `json:"notificationType,omitempty"`
	Message          *string `json:"message,omitempty"`
}
