package garminmessenger

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OTAUUIDOption configures GenerateOTAUUID behavior.
type OTAUUIDOption func(*otaUUIDParams)

type otaUUIDParams struct {
	timestamp     *uint32
	groupIndex    *int
	fragmentIndex *int
	reserved1     int
	reserved2     int
	randomValue   *uint64
}

// WithTimestamp sets the timestamp (Unix seconds, truncated to 32 bits).
func WithTimestamp(t time.Time) OTAUUIDOption {
	return func(p *otaUUIDParams) {
		ts := uint32(t.Unix())
		p.timestamp = &ts
	}
}

// WithTimestampRaw sets the raw 32-bit timestamp value.
func WithTimestampRaw(ts uint32) OTAUUIDOption {
	return func(p *otaUUIDParams) {
		p.timestamp = &ts
	}
}

// WithGroupIndex sets the group index (0-14).
func WithGroupIndex(idx int) OTAUUIDOption {
	return func(p *otaUUIDParams) {
		p.groupIndex = &idx
	}
}

// WithFragmentIndex sets the fragment index (0-30).
func WithFragmentIndex(idx int) OTAUUIDOption {
	return func(p *otaUUIDParams) {
		p.fragmentIndex = &idx
	}
}

// WithReserved1 sets reserved1 (0 or 1).
func WithReserved1(v int) OTAUUIDOption {
	return func(p *otaUUIDParams) {
		p.reserved1 = v
	}
}

// WithReserved2 sets reserved2 (0-16383).
func WithReserved2(v int) OTAUUIDOption {
	return func(p *otaUUIDParams) {
		p.reserved2 = v
	}
}

// WithRandomValue sets the deterministic random value (8 bytes).
func WithRandomValue(v uint64) OTAUUIDOption {
	return func(p *otaUUIDParams) {
		p.randomValue = &v
	}
}

// GenerateOTAUUID generates an OTA UUID using Garmin's custom bit layout.
//
// 16-byte layout:
//   - Bytes 0-3: timestamp (big-endian)
//   - Bytes 4-5, 7, 9-13: random bytes
//   - Byte 6: 0x80 | (groupIndex+1) & 0x0F
//   - Byte 8: 0x80 | (fragmentIndex+1) & 0x1F | (reserved1 << 5)
//   - Byte 14: 0x80 | (reserved2 >> 8) & 0x3F
//   - Byte 15: reserved2 & 0xFF
func GenerateOTAUUID(opts ...OTAUUIDOption) (uuid.UUID, error) {
	params := &otaUUIDParams{}
	for _, opt := range opts {
		opt(params)
	}

	// Default timestamp: current time
	var ts uint32
	if params.timestamp != nil {
		ts = *params.timestamp
	} else {
		ts = uint32(time.Now().Unix())
	}

	// Default random value: crypto/rand
	var randomBytes [8]byte
	if params.randomValue != nil {
		binary.BigEndian.PutUint64(randomBytes[:], *params.randomValue)
	} else {
		if _, err := rand.Read(randomBytes[:]); err != nil {
			return uuid.UUID{}, fmt.Errorf("generating random bytes: %w", err)
		}
	}

	// Build the raw 16-byte UUID
	var raw [16]byte

	// Set marker bits
	raw[6] = 0x80
	raw[8] = 0x80
	raw[14] = 0x80

	// Timestamp in first 4 bytes (big-endian)
	binary.BigEndian.PutUint32(raw[0:4], ts)

	// Random bytes placement
	raw[4] = randomBytes[0]
	raw[5] = randomBytes[1]
	raw[7] = randomBytes[2]
	raw[9] = randomBytes[3]
	raw[10] = randomBytes[4]
	raw[11] = randomBytes[5]
	raw[12] = randomBytes[6]
	raw[13] = randomBytes[7]

	// Group index
	if params.groupIndex != nil {
		gi := *params.groupIndex
		if gi < 0 || gi >= 15 {
			return uuid.UUID{}, fmt.Errorf("group_index must be in range 0..14")
		}
		raw[6] |= byte((gi + 1) & 0x0F)
	}

	// Fragment index
	if params.fragmentIndex != nil {
		fi := *params.fragmentIndex
		if fi < 0 || fi >= 31 {
			return uuid.UUID{}, fmt.Errorf("fragment_index must be in range 0..30")
		}
		raw[8] |= byte((fi + 1) & 0x1F)
	}

	// Reserved1
	if params.reserved1 != 0 && params.reserved1 != 1 {
		return uuid.UUID{}, fmt.Errorf("reserved1 must be 0 or 1")
	}
	raw[8] |= byte((params.reserved1 & 1) << 5)

	// Reserved2
	if params.reserved2 < 0 || params.reserved2 >= (1<<14) {
		return uuid.UUID{}, fmt.Errorf("reserved2 must be in range 0..16383")
	}
	raw[14] |= byte((params.reserved2 >> 8) & 0x3F)
	raw[15] |= byte(params.reserved2 & 0xFF)

	u, err := uuid.FromBytes(raw[:])
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("creating UUID from bytes: %w", err)
	}
	return u, nil
}
