package garminmessenger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// HermesAPIOption configures HermesAPI.
type HermesAPIOption func(*HermesAPI)

// WithAPIHTTPClient sets a custom HTTP client for the API.
func WithAPIHTTPClient(client *http.Client) HermesAPIOption {
	return func(api *HermesAPI) {
		api.httpClient = client
	}
}

// WithAPILogger sets a custom logger for the API.
func WithAPILogger(logger *slog.Logger) HermesAPIOption {
	return func(api *HermesAPI) {
		api.logger = logger
	}
}

// HermesAPI is the synchronous Hermes REST API client.
type HermesAPI struct {
	auth       *HermesAuth
	baseURL    string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewHermesAPI creates a new HermesAPI client.
func NewHermesAPI(auth *HermesAuth, opts ...HermesAPIOption) *HermesAPI {
	api := &HermesAPI{
		auth:       auth,
		baseURL:    auth.HermesBase,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     slog.Default(),
	}
	for _, opt := range opts {
		opt(api)
	}
	return api
}

// Close releases resources.
func (api *HermesAPI) Close() {}

// ----- conversations ---------------------------------------------------------

// GetConversationsOption configures GetConversations.
type GetConversationsOption func(*getConversationsParams)
type getConversationsParams struct {
	limit     int
	afterDate *time.Time
}

// WithLimit sets the conversation limit.
func WithLimit(n int) GetConversationsOption {
	return func(p *getConversationsParams) { p.limit = n }
}

// WithAfterDate sets the after date filter.
func WithAfterDate(t time.Time) GetConversationsOption {
	return func(p *getConversationsParams) { p.afterDate = &t }
}

// GetConversations returns conversations updated after a date.
func (api *HermesAPI) GetConversations(ctx context.Context, opts ...GetConversationsOption) (*GetConversationsModel, error) {
	params := &getConversationsParams{limit: 50}
	for _, opt := range opts {
		opt(params)
	}

	q := url.Values{"Limit": {fmt.Sprint(params.limit)}}
	if params.afterDate != nil {
		q.Set("AfterDate", params.afterDate.Format(time.RFC3339))
	}

	var result GetConversationsModel
	if err := api.doGet(ctx, "Conversation/Updated", q, "1.0", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetConversationDetailOption configures GetConversationDetail.
type GetConversationDetailOption func(*getConversationDetailParams)
type getConversationDetailParams struct {
	limit       int
	olderThanID *uuid.UUID
	newerThanID *uuid.UUID
}

// WithDetailLimit sets the message limit for conversation detail.
func WithDetailLimit(n int) GetConversationDetailOption {
	return func(p *getConversationDetailParams) { p.limit = n }
}

// WithOlderThanID paginates to messages older than the given ID.
func WithOlderThanID(id uuid.UUID) GetConversationDetailOption {
	return func(p *getConversationDetailParams) { p.olderThanID = &id }
}

// WithNewerThanID paginates to messages newer than the given ID.
func WithNewerThanID(id uuid.UUID) GetConversationDetailOption {
	return func(p *getConversationDetailParams) { p.newerThanID = &id }
}

// GetConversationDetail returns messages in a conversation.
func (api *HermesAPI) GetConversationDetail(ctx context.Context, conversationID uuid.UUID, opts ...GetConversationDetailOption) (*ConversationDetailModel, error) {
	params := &getConversationDetailParams{limit: 50}
	for _, opt := range opts {
		opt(params)
	}

	q := url.Values{"Limit": {fmt.Sprint(params.limit)}}
	if params.olderThanID != nil {
		q.Set("olderThanId", params.olderThanID.String())
	}
	if params.newerThanID != nil {
		q.Set("newerThanId", params.newerThanID.String())
	}

	var result ConversationDetailModel
	if err := api.doGet(ctx, fmt.Sprintf("Conversation/Details/%s", conversationID), q, "2.0", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// MuteConversation mutes or unmutes a conversation.
func (api *HermesAPI) MuteConversation(ctx context.Context, conversationID uuid.UUID, muted bool) error {
	action := "Mute"
	if !muted {
		action = "Unmute"
	}
	path := fmt.Sprintf("Conversation/%s/%s", conversationID, action)
	if muted {
		return api.doPostNoResult(ctx, path, map[string]bool{"isMuted": true}, "1.0")
	}
	return api.doPostNoResult(ctx, path, nil, "1.0")
}

// GetConversationMembers returns member details for a conversation.
func (api *HermesAPI) GetConversationMembers(ctx context.Context, conversationID uuid.UUID) ([]UserInfoModel, error) {
	var result []UserInfoModel
	if err := api.doGet(ctx, fmt.Sprintf("Conversation/Members/%s", conversationID), nil, "1.0", &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetMutedConversations returns muted conversations with expiry.
func (api *HermesAPI) GetMutedConversations(ctx context.Context) ([]ConversationMuteDetailModel, error) {
	var result []ConversationMuteDetailModel
	if err := api.doGet(ctx, "Conversation/Muted", nil, "1.0", &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ----- messages --------------------------------------------------------------

// SendMessageOption configures SendMessage.
type SendMessageOption func(*sendMessageParams)
type sendMessageParams struct {
	userLocation    *UserLocation
	referencePoint  *UserLocation
	messageType     *HermesMessageType
	isPost          bool
	mediaID         *uuid.UUID
	mediaType       *MediaType
	mediaMetadata   *MediaMetadata
	parentMessageID *uuid.UUID
}

// WithUserLocation sets the sender's location.
func WithUserLocation(loc UserLocation) SendMessageOption {
	return func(p *sendMessageParams) { p.userLocation = &loc }
}

// WithReferencePoint sets the reference point.
func WithReferencePoint(ref UserLocation) SendMessageOption {
	return func(p *sendMessageParams) { p.referencePoint = &ref }
}

// WithMessageType sets the message type.
func WithMessageType(mt HermesMessageType) SendMessageOption {
	return func(p *sendMessageParams) { p.messageType = &mt }
}

// WithIsPost marks the message as a post.
func WithIsPost(p bool) SendMessageOption {
	return func(params *sendMessageParams) { params.isPost = p }
}

// withMediaID sets the media ID (internal use).
func withMediaID(id uuid.UUID) SendMessageOption {
	return func(p *sendMessageParams) { p.mediaID = &id }
}

// withMediaType sets the media type (internal use).
func withMediaType(mt MediaType) SendMessageOption {
	return func(p *sendMessageParams) { p.mediaType = &mt }
}

// WithMediaMetadata sets the media metadata (e.g. duration for audio).
func WithMediaMetadata(meta MediaMetadata) SendMessageOption {
	return func(p *sendMessageParams) { p.mediaMetadata = &meta }
}

// WithParentMessageID sets the parent message ID (for reactions and replies).
func WithParentMessageID(id uuid.UUID) SendMessageOption {
	return func(p *sendMessageParams) { p.parentMessageID = &id }
}

// SendMessage sends a message to one or more recipients.
func (api *HermesAPI) SendMessage(ctx context.Context, to []string, body string, opts ...SendMessageOption) (*SendMessageV2Response, error) {
	params := &sendMessageParams{}
	for _, opt := range opts {
		opt(params)
	}

	msgUUID := uuid.New()
	otaUUID, err := GenerateOTAUUID()
	if err != nil {
		return nil, fmt.Errorf("generating OTA UUID: %w", err)
	}

	req := SendMessageRequest{
		To:              to,
		MessageBody:     body,
		UserLocation:    params.userLocation,
		ReferencePoint:  params.referencePoint,
		MessageType:     params.messageType,
		IsPost:          params.isPost,
		MediaID:         params.mediaID,
		MediaType:       params.mediaType,
		MediaMetadata:   params.mediaMetadata,
		UUID:            &msgUUID,
		OtaUuid:         &otaUUID,
		ParentMessageID: params.parentMessageID,
	}

	var result SendMessageV2Response
	if err := api.doPost(ctx, "Message/Send", req, "2.0", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// SendReaction sends a reaction emoji to a specific message.
// Garmin stores reactions separately from regular messages in its database,
// using messageType="Reaction" and parentMessageId to identify the target.
func (api *HermesAPI) SendReaction(ctx context.Context, to []string, emoji string, parentMessageID uuid.UUID) (*SendMessageV2Response, error) {
	reactionType := HermesMessageTypeReaction
	return api.SendMessage(ctx, to, emoji,
		WithMessageType(reactionType),
		WithParentMessageID(parentMessageID),
	)
}

// GetMessageDeviceMetadata returns satellite device metadata for messages.
func (api *HermesAPI) GetMessageDeviceMetadata(ctx context.Context, ids []SimpleCompoundMessageId) ([]MessageDeviceMetadataV2, error) {
	var result []MessageDeviceMetadataV2
	if err := api.doPost(ctx, "Message/DeviceMetadata", ids, "2.0", &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ----- media -----------------------------------------------------------------

// UploadMedia uploads media to S3 using a presigned POST from Hermes.
func (api *HermesAPI) UploadMedia(ctx context.Context, signedURL *SignedUploadUrl, fileData []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// Add S3 presigned POST fields as form data
	addField := func(name string, val *string) error {
		if val != nil {
			return w.WriteField(name, *val)
		}
		return nil
	}
	for _, f := range []struct {
		name string
		val  *string
	}{
		{"key", signedURL.Key},
		{"x-amz-storage-class", signedURL.XAmzStorageClass},
		{"x-amz-date", signedURL.XAmzDate},
		{"x-amz-signature", signedURL.XAmzSignature},
		{"x-amz-algorithm", signedURL.XAmzAlgorithm},
		{"x-amz-credential", signedURL.XAmzCredential},
		{"policy", signedURL.Policy},
		{"x-amz-meta-media-quality", signedURL.XAmzMetaMediaQuality},
		{"Content-Type", signedURL.ContentType},
	} {
		if err := addField(f.name, f.val); err != nil {
			return fmt.Errorf("writing form field %s: %w", f.name, err)
		}
	}

	// File part
	contentType := "application/octet-stream"
	if signedURL.ContentType != nil {
		contentType = *signedURL.ContentType
	}
	part, err := w.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="file"; filename="attachment"`},
		"Content-Type":        {contentType},
	})
	if err != nil {
		return fmt.Errorf("creating multipart file part: %w", err)
	}
	if _, err := part.Write(fileData); err != nil {
		return fmt.Errorf("writing file data: %w", err)
	}
	w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", signedURL.UploadUrl, &buf)
	if err != nil {
		return fmt.Errorf("creating S3 upload request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	api.logRequest("POST", signedURL.UploadUrl, req.Header, nil)
	resp, err := api.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("S3 upload: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	api.logResponse(resp, respBody)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(respBody),
			URL:        signedURL.UploadUrl,
			Method:     "POST",
		}
	}
	return nil
}

// GetMediaDownloadURL returns a presigned S3 download URL.
func (api *HermesAPI) GetMediaDownloadURL(ctx context.Context, msgUUID, mediaID, messageID, conversationID uuid.UUID, mediaType MediaType) (*MediaAttachmentDownloadUrlResponse, error) {
	q := url.Values{
		"uuid":           {msgUUID.String()},
		"mediaType":      {string(mediaType)},
		"mediaId":        {mediaID.String()},
		"messageId":      {messageID.String()},
		"conversationId": {conversationID.String()},
	}
	var result MediaAttachmentDownloadUrlResponse
	if err := api.doGet(ctx, "Message/Media/DownloadUrl", q, "2.0", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DownloadMedia downloads a media attachment — fetches presigned URL then downloads.
func (api *HermesAPI) DownloadMedia(ctx context.Context, msgUUID, mediaID, messageID, conversationID uuid.UUID, mediaType MediaType) ([]byte, error) {
	urlResp, err := api.GetMediaDownloadURL(ctx, msgUUID, mediaID, messageID, conversationID, mediaType)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", urlResp.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating download request: %w", err)
	}
	api.logRequest("GET", urlResp.URL[:min(80, len(urlResp.URL))], req.Header, nil)
	resp, err := api.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading media: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	api.logger.Log(nil, LevelTrace, "<<< Response", "status", resp.StatusCode, "bytes", len(respBody))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(respBody),
			URL:        urlResp.URL,
			Method:     "GET",
		}
	}
	return respBody, nil
}

// UpdateMedia confirms upload or requests a new signed URL.
func (api *HermesAPI) UpdateMedia(ctx context.Context, req UpdateMediaRequest) (*UpdateMediaResponse, error) {
	var result UpdateMediaResponse
	if err := api.doPost(ctx, "Message/UpdateMedia", req, "2.0", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// SendMediaMessage sends a message with a media attachment (convenience method).
func (api *HermesAPI) SendMediaMessage(ctx context.Context, to []string, body string, fileData []byte, mediaType MediaType, opts ...SendMessageOption) (*SendMessageV2Response, error) {
	mediaID := uuid.New()
	opts = append(opts, withMediaID(mediaID), withMediaType(mediaType))
	result, err := api.SendMessage(ctx, to, body, opts...)
	if err != nil {
		return nil, err
	}
	if result.SignedUploadUrl != nil {
		if err := api.UploadMedia(ctx, result.SignedUploadUrl, fileData); err != nil {
			return nil, fmt.Errorf("uploading media: %w", err)
		}
	} else {
		api.logger.Warn("Server did not return signedUploadUrl for media message", "messageId", result.MessageID)
	}
	return result, nil
}

// ----- status updates --------------------------------------------------------

// MarkAsRead marks a message as read.
func (api *HermesAPI) MarkAsRead(ctx context.Context, conversationID, messageID uuid.UUID) (*UpdateMessageStatusResponse, error) {
	var result UpdateMessageStatusResponse
	if err := api.doPut(ctx, fmt.Sprintf("Status/Read/%s/%s", conversationID, messageID), nil, "1.0", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// MarkAsDelivered marks a message as delivered.
func (api *HermesAPI) MarkAsDelivered(ctx context.Context, conversationID, messageID uuid.UUID) (*UpdateMessageStatusResponse, error) {
	var result UpdateMessageStatusResponse
	if err := api.doPut(ctx, fmt.Sprintf("Status/Delivered/%s/%s", conversationID, messageID), nil, "1.0", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// UpdateMessageStatuses performs a batch status update.
func (api *HermesAPI) UpdateMessageStatuses(ctx context.Context, updates []UpdateMessageStatusRequest) ([]UpdateMessageStatusResponse, error) {
	var result []UpdateMessageStatusResponse
	if err := api.doPut(ctx, "Status/UpdateMessageStatuses", updates, "1.0", &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetUpdatedStatusesOption configures GetUpdatedStatuses.
type GetUpdatedStatusesOption func(*getUpdatedStatusesParams)
type getUpdatedStatusesParams struct {
	limit int
}

// WithStatusLimit sets the status limit.
func WithStatusLimit(n int) GetUpdatedStatusesOption {
	return func(p *getUpdatedStatusesParams) { p.limit = n }
}

// GetUpdatedStatuses returns status changes since a date.
func (api *HermesAPI) GetUpdatedStatuses(ctx context.Context, afterDate time.Time, opts ...GetUpdatedStatusesOption) (*GetUpdatedStatusesResponse, error) {
	params := &getUpdatedStatusesParams{limit: 50}
	for _, opt := range opts {
		opt(params)
	}

	q := url.Values{
		"AfterDate": {afterDate.Format(time.RFC3339)},
		"Limit":     {fmt.Sprint(params.limit)},
	}

	var result GetUpdatedStatusesResponse
	if err := api.doGet(ctx, "Status/Updated", q, "1.0", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ----- user info -------------------------------------------------------------

// GetCapabilities returns the user's capabilities.
func (api *HermesAPI) GetCapabilities(ctx context.Context) (map[string]any, error) {
	var result map[string]any
	if err := api.doGet(ctx, "UserInfo/Capabilities", nil, "1.0", &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetBlockedUsers returns blocked users.
func (api *HermesAPI) GetBlockedUsers(ctx context.Context) ([]map[string]any, error) {
	var result []map[string]any
	if err := api.doGet(ctx, "UserInfo/BlockedUsers", nil, "1.0", &result); err != nil {
		return nil, err
	}
	return result, nil
}

// BlockUser blocks a user.
func (api *HermesAPI) BlockUser(ctx context.Context, userID string) error {
	return api.doPostNoResult(ctx, "UserInfo/Block", map[string]string{"userId": userID}, "1.0")
}

// UnblockUser unblocks a user.
func (api *HermesAPI) UnblockUser(ctx context.Context, userID string) error {
	return api.doPostNoResult(ctx, "UserInfo/Unblock", map[string]string{"userId": userID}, "1.0")
}

// ----- network info ----------------------------------------------------------

// GetNetworkProperties returns network status flags.
func (api *HermesAPI) GetNetworkProperties(ctx context.Context) (*NetworkPropertiesResponse, error) {
	var result NetworkPropertiesResponse
	if err := api.doGet(ctx, "NetworkInfo/Properties", nil, "1.0", &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ----- internal HTTP helpers -------------------------------------------------

func (api *HermesAPI) headers(ctx context.Context, apiVersion string) (http.Header, error) {
	h, err := api.auth.Headers(ctx)
	if err != nil {
		return nil, err
	}
	h.Set("Api-Version", apiVersion)
	return h, nil
}

func (api *HermesAPI) doGet(ctx context.Context, path string, query url.Values, apiVersion string, result any) error {
	u := api.baseURL + "/" + path
	if query != nil {
		u += "?" + query.Encode()
	}

	h, err := api.headers(ctx, apiVersion)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return fmt.Errorf("creating GET request: %w", err)
	}
	for k, v := range h {
		req.Header[k] = v
	}

	api.logRequest("GET", u, req.Header, nil)
	resp, err := api.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	api.logResponse(resp, body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       truncate(string(body), 2000),
			URL:        u,
			Method:     "GET",
		}
	}

	if result != nil {
		if err := json.Unmarshal(body, result); err != nil {
			return fmt.Errorf("decoding GET %s response: %w", path, err)
		}
	}
	return nil
}

func (api *HermesAPI) doPost(ctx context.Context, path string, reqBody any, apiVersion string, result any) error {
	return api.doMutate(ctx, "POST", path, reqBody, apiVersion, result)
}

func (api *HermesAPI) doPostNoResult(ctx context.Context, path string, reqBody any, apiVersion string) error {
	return api.doMutate(ctx, "POST", path, reqBody, apiVersion, nil)
}

func (api *HermesAPI) doPut(ctx context.Context, path string, reqBody any, apiVersion string, result any) error {
	return api.doMutate(ctx, "PUT", path, reqBody, apiVersion, result)
}

func (api *HermesAPI) doMutate(ctx context.Context, method, path string, reqBody any, apiVersion string, result any) error {
	u := api.baseURL + "/" + path

	h, err := api.headers(ctx, apiVersion)
	if err != nil {
		return err
	}

	var reqData []byte
	var bodyReader io.Reader
	if reqBody != nil {
		var err2 error
		reqData, err2 = json.Marshal(reqBody)
		if err2 != nil {
			return fmt.Errorf("marshaling request body: %w", err2)
		}
		bodyReader = bytes.NewReader(reqData)
		h.Set("Content-Type", "application/json")
	}

	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return fmt.Errorf("creating %s request: %w", method, err)
	}
	for k, v := range h {
		req.Header[k] = v
	}

	api.logRequest(method, u, req.Header, reqData)
	resp, err := api.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, u, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	api.logResponse(resp, body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       truncate(string(body), 2000),
			URL:        u,
			Method:     method,
		}
	}

	if result != nil && len(body) > 0 {
		if err := json.Unmarshal(body, result); err != nil {
			return fmt.Errorf("decoding %s %s response: %w", method, path, err)
		}
	}
	return nil
}

func (api *HermesAPI) logRequest(method, url string, headers http.Header, body []byte) {
	api.logger.Log(nil, LevelTrace, ">>> "+method, "url", url)
	for k, v := range headers {
		val := v[0]
		if len(v) > 1 {
			val = fmt.Sprintf("%v", v)
		}
		if len(val) > 120 {
			api.logger.Log(nil, LevelTrace, "  Request header", "key", k, "value", val[:60]+"..."+val[len(val)-20:])
		} else {
			api.logger.Log(nil, LevelTrace, "  Request header", "key", k, "value", val)
		}
	}
	if body != nil {
		api.logger.Log(nil, LevelTrace, "  Request body", "json", truncate(string(body), 2000))
	}
}

func (api *HermesAPI) logResponse(resp *http.Response, body []byte) {
	api.logger.Log(nil, LevelTrace, "<<< Response", "status", resp.StatusCode, "url", resp.Request.URL.String(), "bytes", len(body))
	for k, v := range resp.Header {
		api.logger.Log(nil, LevelTrace, "  Response header", "key", k, "value", v[0])
	}
	if len(body) > 0 {
		api.logger.Log(nil, LevelTrace, "  Response body", "json", truncate(string(body), 2000))
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
