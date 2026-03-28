package fcm

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/palchrb/garmin-messenger-relay/internal/hermes/internal/mcspb"
	"google.golang.org/protobuf/proto"
)

const mcsVersion = 41

// mcsTag identifies MCS protocol message types.
type mcsTag uint8

const (
	tagHeartbeatPing     mcsTag = 0
	tagHeartbeatAck      mcsTag = 1
	tagLoginRequest      mcsTag = 2
	tagLoginResponse     mcsTag = 3
	tagClose             mcsTag = 4
	tagIqStanza          mcsTag = 7
	tagDataMessageStanza mcsTag = 8
	tagStreamErrorStanza mcsTag = 10
)

// mcsClient is a lightweight MCS (Mobile Connection Server) protocol client.
// Android-native version: no encryption keys needed (messages arrive as plaintext AppData).
type mcsClient struct {
	conn          io.ReadWriteCloser
	androidID     uint64
	securityToken uint64
	persistentIDs []string
	logger        *slog.Logger

	heartbeatInterval time.Duration

	onDataMessage  func(persistentID string, payload []byte, appData []*mcspb.AppData)
	onConnected    func()
	onDisconnected func(reason string)

	writeMu sync.Mutex
}

// newMCSClient creates a new MCS client bound to the given connection.
// Android-native version: no privateKey or authSecret needed.
func newMCSClient(conn io.ReadWriteCloser, androidID, securityToken uint64, persistentIDs []string, logger *slog.Logger) *mcsClient {
	return &mcsClient{
		conn:              conn,
		androidID:         androidID,
		securityToken:     securityToken,
		persistentIDs:     persistentIDs,
		logger:            logger,
		heartbeatInterval: 5 * time.Minute,
	}
}

// connect performs the MCS login handshake and enters the read loop.
// It blocks until ctx is cancelled, the server sends a Close, or an error occurs.
func (m *mcsClient) connect(ctx context.Context) error {
	// Close conn when context is cancelled so blocking reads/writes unblock.
	connClosed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			m.conn.Close()
		case <-connClosed:
		}
	}()
	defer close(connClosed)

	if err := m.sendLogin(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("mcs: send login: %w", err)
	}

	// Read server version byte
	var vBuf [1]byte
	if _, err := io.ReadFull(m.conn, vBuf[:]); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("mcs: read version: %w", err)
	}

	// Start heartbeat goroutine
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()
	go m.heartbeatLoop(heartbeatCtx)

	// Read loop — blocks until error or conn close
	err := m.readLoop()
	if ctx.Err() != nil {
		if m.onDisconnected != nil {
			m.onDisconnected("context cancelled")
		}
		return nil
	}
	if m.onDisconnected != nil {
		reason := "read loop ended"
		if err != nil {
			reason = err.Error()
		}
		m.onDisconnected(reason)
	}
	return err
}

func (m *mcsClient) sendLogin() error {
	decID := fmt.Sprintf("%d", m.androidID)
	authToken := fmt.Sprintf("%d", m.securityToken)

	// Android-native login ID format: "android-{hex(androidId)}"
	androidLoginID := fmt.Sprintf("android-%x", m.androidID)

	loginReq := &mcspb.LoginRequest{
		Id:                   proto.String(androidLoginID),
		Domain:               proto.String("mcs.android.com"),
		User:                 proto.String(decID),
		Resource:             proto.String(decID),
		AuthToken:            proto.String(authToken),
		DeviceId:             proto.String(androidLoginID),
		LastRmqId:            proto.Int64(1),
		ReceivedPersistentId: m.persistentIDs,
		AdaptiveHeartbeat:    proto.Bool(false),
		UseRmq2:              proto.Bool(true),
		AccountId:            proto.Int64(1000000),
		AuthService:          mcspb.LoginRequest_ANDROID_ID.Enum(),
		NetworkType:          proto.Int32(1),
		Setting: []*mcspb.Setting{
			{Name: proto.String("new_vc"), Value: proto.String("1")},
		},
	}

	return m.sendPacket(tagLoginRequest, loginReq, true)
}

func (m *mcsClient) sendPacket(tag mcsTag, msg proto.Message, includeVersion bool) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	if includeVersion {
		if _, err := m.conn.Write([]byte{mcsVersion}); err != nil {
			return err
		}
	}

	// Tag byte
	if _, err := m.conn.Write([]byte{byte(tag)}); err != nil {
		return err
	}

	// Varint-encoded size
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], uint64(len(data)))
	if _, err := m.conn.Write(buf[:n]); err != nil {
		return err
	}

	// Proto data
	if _, err := m.conn.Write(data); err != nil {
		return err
	}

	return nil
}

func (m *mcsClient) readLoop() error {
	for {
		tag, err := m.readTagByte()
		if err != nil {
			return fmt.Errorf("mcs: read tag: %w", err)
		}

		size, err := m.readVarint()
		if err != nil {
			return fmt.Errorf("mcs: read size: %w", err)
		}

		data := make([]byte, size)
		if _, err := io.ReadFull(m.conn, data); err != nil {
			return fmt.Errorf("mcs: read body: %w", err)
		}

		if err := m.handlePacket(tag, data); err != nil {
			return err
		}
	}
}

func (m *mcsClient) handlePacket(tag mcsTag, data []byte) error {
	switch tag {
	case tagLoginResponse:
		var resp mcspb.LoginResponse
		if err := proto.Unmarshal(data, &resp); err != nil {
			return fmt.Errorf("mcs: unmarshal LoginResponse: %w", err)
		}
		m.logger.Debug("MCS login response", "id", resp.GetId())
		m.persistentIDs = nil
		if m.onConnected != nil {
			m.onConnected()
		}

	case tagHeartbeatPing:
		var ping mcspb.HeartbeatPing
		if err := proto.Unmarshal(data, &ping); err != nil {
			m.logger.Warn("MCS: failed to unmarshal HeartbeatPing", "error", err)
			return nil
		}
		m.logger.Debug("MCS heartbeat ping received")
		ack := &mcspb.HeartbeatAck{}
		if err := m.sendPacket(tagHeartbeatAck, ack, false); err != nil {
			return fmt.Errorf("mcs: send heartbeat ack: %w", err)
		}

	case tagHeartbeatAck:
		m.logger.Debug("MCS heartbeat ack received")

	case tagDataMessageStanza:
		var msg mcspb.DataMessageStanza
		if err := proto.Unmarshal(data, &msg); err != nil {
			m.logger.Warn("MCS: failed to unmarshal DataMessageStanza", "error", err)
			return nil
		}
		m.logger.Debug("MCS data message", "from", msg.GetFrom(), "category", msg.GetCategory(), "persistentId", msg.GetPersistentId())

		// Android-native: Messages arrive as plaintext AppData (no encryption)
		if len(msg.GetRawData()) > 0 {
			m.logger.Error("MCS: encrypted raw_data no longer supported - re-register with Android-native FCM")
			return fmt.Errorf("encrypted FCM messages no longer supported; re-run 'garmin-messenger login'")
		}

		// Parse plaintext AppData
		appData := msg.GetAppData()
		if m.onDataMessage != nil {
			m.onDataMessage(msg.GetPersistentId(), nil, appData)
		}

	case tagClose:
		return fmt.Errorf("mcs: server sent close")

	case tagIqStanza:
		var iq mcspb.IqStanza
		if err := proto.Unmarshal(data, &iq); err != nil {
			m.logger.Warn("MCS: failed to unmarshal IqStanza", "error", err)
		} else {
			m.logger.Info("MCS IqStanza received", "type", iq.GetType(), "id", iq.GetId(), "from", iq.GetFrom(), "to", iq.GetTo())
		}

	case tagStreamErrorStanza:
		var se mcspb.StreamErrorStanza
		if err := proto.Unmarshal(data, &se); err != nil {
			return fmt.Errorf("mcs: stream error (unmarshal failed: %w)", err)
		}
		return fmt.Errorf("mcs: stream error: type=%s text=%s", se.GetType(), se.GetText())

	default:
		m.logger.Debug("MCS unknown tag", "tag", tag)
	}

	return nil
}

func (m *mcsClient) readTagByte() (mcsTag, error) {
	var buf [1]byte
	if _, err := io.ReadFull(m.conn, buf[:]); err != nil {
		return 0, err
	}
	return mcsTag(buf[0]), nil
}

func (m *mcsClient) readVarint() (uint64, error) {
	var result uint64
	var shift uint
	for {
		var buf [1]byte
		if _, err := io.ReadFull(m.conn, buf[:]); err != nil {
			return 0, err
		}
		b := buf[0]
		result |= uint64(b&0x7F) << shift
		if b < 0x80 {
			break
		}
		shift += 7
		if shift >= 64 {
			return 0, fmt.Errorf("varint overflow: more than 10 bytes")
		}
	}
	return result, nil
}

func (m *mcsClient) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(m.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ping := &mcspb.HeartbeatPing{}
			if err := m.sendPacket(tagHeartbeatPing, ping, false); err != nil {
				m.logger.Warn("MCS: failed to send heartbeat ping", "error", err)
				return
			}
			m.logger.Debug("MCS heartbeat ping sent")
		}
	}
}
