package fcm

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/palchrb/garmin-messenger-relay/internal/hermes/internal/mcspb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// writePacket writes a MCS wire-format packet to w: tag byte + varint size + marshaled proto.
// If includeVersion is true, it prepends the MCS version byte (41).
func writePacket(t *testing.T, w io.Writer, tag mcsTag, msg proto.Message, includeVersion bool) {
	t.Helper()
	if includeVersion {
		_, err := w.Write([]byte{mcsVersion})
		require.NoError(t, err)
	}

	data, err := proto.Marshal(msg)
	require.NoError(t, err)

	// Tag byte
	_, err = w.Write([]byte{byte(tag)})
	require.NoError(t, err)

	// Varint-encoded size
	size := uint64(len(data))
	var buf [10]byte
	n := 0
	for size >= 0x80 {
		buf[n] = byte(size) | 0x80
		size >>= 7
		n++
	}
	buf[n] = byte(size)
	n++
	_, err = w.Write(buf[:n])
	require.NoError(t, err)

	// Marshaled proto (skip 0-length writes — net.Pipe blocks on them)
	if len(data) > 0 {
		_, err = w.Write(data)
		require.NoError(t, err)
	}
}

// readTag reads a single MCS tag byte from r.
func readTag(t *testing.T, r io.Reader) mcsTag {
	t.Helper()
	var buf [1]byte
	_, err := io.ReadFull(r, buf[:])
	require.NoError(t, err)
	return mcsTag(buf[0])
}

// readVarintTest reads a protobuf varint from r.
func readVarintTest(t *testing.T, r io.Reader) uint64 {
	t.Helper()
	var result uint64
	var shift uint
	for {
		var buf [1]byte
		_, err := io.ReadFull(r, buf[:])
		require.NoError(t, err)
		b := buf[0]
		result |= uint64(b&0x7F) << shift
		if b < 0x80 {
			break
		}
		shift += 7
	}
	return result
}

func TestMCS_LoginPacket(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	mcs := newMCSClient(client, 12345, 67890, []string{"pid-1"}, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- mcs.connect(ctx)
	}()

	// Read version byte from client
	var versionBuf [1]byte
	_, err := io.ReadFull(server, versionBuf[:])
	require.NoError(t, err)
	assert.Equal(t, byte(mcsVersion), versionBuf[0])

	// Read tag (should be LoginRequest = 2)
	tag := readTag(t, server)
	assert.Equal(t, tagLoginRequest, tag)

	// Read size
	size := readVarintTest(t, server)

	// Read marshaled LoginRequest
	data := make([]byte, size)
	_, err = io.ReadFull(server, data)
	require.NoError(t, err)

	var loginReq mcspb.LoginRequest
	require.NoError(t, proto.Unmarshal(data, &loginReq))

	// Android-native: login ID format is "android-{hex(androidId)}"
	assert.Equal(t, "android-3039", loginReq.GetId()) // 12345 decimal = 0x3039
	assert.Equal(t, "mcs.android.com", loginReq.GetDomain())
	assert.Equal(t, "12345", loginReq.GetUser())
	assert.Equal(t, "12345", loginReq.GetResource())
	assert.Equal(t, "67890", loginReq.GetAuthToken())
	assert.Equal(t, "android-3039", loginReq.GetDeviceId())
	assert.True(t, loginReq.GetUseRmq2())
	assert.Equal(t, int64(1), loginReq.GetLastRmqId())
	assert.Equal(t, mcspb.LoginRequest_ANDROID_ID, loginReq.GetAuthService())
	assert.Equal(t, []string{"pid-1"}, loginReq.GetReceivedPersistentId())

	cancel()
	<-errCh
}

func TestMCS_LoginResponse(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	connected := make(chan struct{}, 1)
	mcs := newMCSClient(client, 100, 200, nil, slog.Default())
	mcs.onConnected = func() {
		close(connected)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = mcs.connect(ctx)
	}()

	// Read and discard version + login request from client
	discardLoginPacket(t, server)

	// Send version byte + LoginResponse
	loginResp := &mcspb.LoginResponse{
		Id: proto.String("server-id"),
	}
	writePacket(t, server, tagLoginResponse, loginResp, true)

	select {
	case <-connected:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("onConnected not called within timeout")
	}

	cancel()
}

func TestMCS_HeartbeatPingResponse(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	mcs := newMCSClient(client, 100, 200, nil, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = mcs.connect(ctx)
	}()

	discardLoginPacket(t, server)

	// Send version + LoginResponse first
	loginResp := &mcspb.LoginResponse{Id: proto.String("s")}
	writePacket(t, server, tagLoginResponse, loginResp, true)

	// Send a HeartbeatPing
	ping := &mcspb.HeartbeatPing{StreamId: proto.Int32(1)}
	writePacket(t, server, tagHeartbeatPing, ping, false)

	// Read the HeartbeatAck response
	tag := readTag(t, server)
	assert.Equal(t, tagHeartbeatAck, tag)

	size := readVarintTest(t, server)
	data := make([]byte, size)
	_, err := io.ReadFull(server, data)
	require.NoError(t, err)

	var ack mcspb.HeartbeatAck
	require.NoError(t, proto.Unmarshal(data, &ack))

	cancel()
}

func TestMCS_DataMessageStanza(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	var receivedPersistentID string
	var receivedPayload []byte
	var receivedAppData []*mcspb.AppData
	done := make(chan struct{}, 1)

	mcs := newMCSClient(client, 100, 200, nil, slog.Default())
	mcs.onDataMessage = func(persistentID string, payload []byte, appData []*mcspb.AppData) {
		receivedPersistentID = persistentID
		receivedPayload = payload
		receivedAppData = appData
		close(done)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = mcs.connect(ctx)
	}()

	discardLoginPacket(t, server)

	// LoginResponse
	loginResp := &mcspb.LoginResponse{Id: proto.String("s")}
	writePacket(t, server, tagLoginResponse, loginResp, true)

	// DataMessageStanza
	dataMsg := &mcspb.DataMessageStanza{
		From:         proto.String("sender"),
		Category:     proto.String(garminAppPackage),
		PersistentId: proto.String("persistent-123"),
		AppData: []*mcspb.AppData{
			{Key: proto.String("newMessage"), Value: proto.String(`{"messageId":"abc"}`)},
		},
	}
	writePacket(t, server, tagDataMessageStanza, dataMsg, false)

	select {
	case <-done:
		assert.Equal(t, "persistent-123", receivedPersistentID)
		assert.Nil(t, receivedPayload)
		require.Len(t, receivedAppData, 1)
		assert.Equal(t, "newMessage", receivedAppData[0].GetKey())
	case <-time.After(2 * time.Second):
		t.Fatal("onDataMessage not called within timeout")
	}

	cancel()
}


func TestMCS_Close(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	mcs := newMCSClient(client, 100, 200, nil, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- mcs.connect(ctx)
	}()

	discardLoginPacket(t, server)

	// LoginResponse
	loginResp := &mcspb.LoginResponse{Id: proto.String("s")}
	writePacket(t, server, tagLoginResponse, loginResp, true)

	// Close tag
	closeMsg := &mcspb.Close{}
	writePacket(t, server, tagClose, closeMsg, false)

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "close")
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not return within timeout")
	}
}

func TestMCS_ContextCancel(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	disconnected := make(chan struct{}, 1)
	mcs := newMCSClient(client, 100, 200, nil, slog.Default())
	mcs.onDisconnected = func(reason string) {
		close(disconnected)
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- mcs.connect(ctx)
	}()

	discardLoginPacket(t, server)

	// LoginResponse
	loginResp := &mcspb.LoginResponse{Id: proto.String("s")}
	writePacket(t, server, tagLoginResponse, loginResp, true)

	// Give read loop time to start
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		// Should return nil or context cancelled
		if err != nil {
			assert.Contains(t, err.Error(), "cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not return after cancel")
	}
}

// discardLoginPacket reads and discards the version byte + login request packet
// from the MCS client side of the pipe.
func discardLoginPacket(t *testing.T, r io.Reader) {
	t.Helper()
	// Version byte
	var vBuf [1]byte
	_, err := io.ReadFull(r, vBuf[:])
	require.NoError(t, err)

	// Tag byte
	_, err = io.ReadFull(r, vBuf[:])
	require.NoError(t, err)

	// Size varint
	size := readVarintTest(t, r)

	// Body
	body := make([]byte, size)
	_, err = io.ReadFull(r, body)
	require.NoError(t, err)
}

func TestMCS_HeartbeatTimerSendsping(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	mcs := newMCSClient(client, 100, 200, nil, slog.Default())
	mcs.heartbeatInterval = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = mcs.connect(ctx)
	}()

	discardLoginPacket(t, server)

	loginResp := &mcspb.LoginResponse{Id: proto.String("s")}
	writePacket(t, server, tagLoginResponse, loginResp, true)

	// Wait for the heartbeat ping
	done := make(chan struct{})
	go func() {
		defer close(done)
		tag := readTag(t, server)
		assert.Equal(t, tagHeartbeatPing, tag)
	}()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat ping not sent within timeout")
	}

	cancel()
}

func TestMCS_VarintOverflow(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	mcs := newMCSClient(client, 100, 200, nil, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- mcs.connect(ctx)
	}()

	discardLoginPacket(t, server)

	// Send version + LoginResponse
	loginResp := &mcspb.LoginResponse{Id: proto.String("s")}
	writePacket(t, server, tagLoginResponse, loginResp, true)

	// Send a tag byte followed by 10 continuation bytes (all 0x80) to trigger varint overflow.
	// Written as a single buffer because net.Pipe() Write blocks until all bytes are consumed.
	var overflow [11]byte
	overflow[0] = byte(tagDataMessageStanza)
	for i := 1; i < 11; i++ {
		overflow[i] = 0x80
	}
	_, err := server.Write(overflow[:])
	require.NoError(t, err)

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "varint overflow")
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not return within timeout")
	}
}

func TestMCS_IqStanzaIgnored(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	var mu sync.Mutex
	dataReceived := false
	dataDone := make(chan struct{}, 1)

	mcs := newMCSClient(client, 100, 200, nil, slog.Default())
	mcs.onDataMessage = func(persistentID string, payload []byte, appData []*mcspb.AppData) {
		mu.Lock()
		dataReceived = true
		mu.Unlock()
		close(dataDone)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = mcs.connect(ctx)
	}()

	discardLoginPacket(t, server)

	loginResp := &mcspb.LoginResponse{Id: proto.String("s")}
	writePacket(t, server, tagLoginResponse, loginResp, true)

	// Send IqStanza (should be ignored)
	iq := &mcspb.IqStanza{
		Type: mcspb.IqStanza_RESULT.Enum(),
		Id:   proto.String("1"),
	}
	writePacket(t, server, tagIqStanza, iq, false)

	// Send DataMessageStanza after IQ to confirm read loop continued
	dataMsg := &mcspb.DataMessageStanza{
		From:         proto.String("sender"),
		Category:     proto.String("test"),
		PersistentId: proto.String("p1"),
		AppData:      []*mcspb.AppData{{Key: proto.String("k"), Value: proto.String("v")}},
	}
	writePacket(t, server, tagDataMessageStanza, dataMsg, false)

	select {
	case <-dataDone:
		mu.Lock()
		assert.True(t, dataReceived)
		mu.Unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("data message not received after IqStanza")
	}

	cancel()
}
