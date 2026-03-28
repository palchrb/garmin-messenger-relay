package garminmessenger

import (
	"encoding/binary"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOtaUUID_ReturnsUUIDType(t *testing.T) {
	result, err := GenerateOTAUUID()
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, result)
}

func TestOtaUUID_DeterministicWithFixedInputs(t *testing.T) {
	u1, err := GenerateOTAUUID(WithTimestampRaw(0x12345678), WithRandomValue(0xAABBCCDDEEFF0011))
	require.NoError(t, err)
	u2, err := GenerateOTAUUID(WithTimestampRaw(0x12345678), WithRandomValue(0xAABBCCDDEEFF0011))
	require.NoError(t, err)
	assert.Equal(t, u1, u2)
}

func TestOtaUUID_TimestampInFirst4Bytes(t *testing.T) {
	ts := uint32(0x12345678)
	u, err := GenerateOTAUUID(WithTimestampRaw(ts), WithRandomValue(0))
	require.NoError(t, err)
	raw := u[:]
	assert.Equal(t, ts, binary.BigEndian.Uint32(raw[0:4]))
}

func TestOtaUUID_FixedMarkerBits(t *testing.T) {
	u, err := GenerateOTAUUID(WithTimestampRaw(0), WithRandomValue(0))
	require.NoError(t, err)
	raw := u[:]
	assert.Equal(t, byte(0x80), raw[6]&0x80)
	assert.Equal(t, byte(0x80), raw[8]&0x80)
	assert.Equal(t, byte(0x80), raw[14]&0x80)
}

func TestOtaUUID_RandomBytesPlacement(t *testing.T) {
	randVal := uint64(0x0102030405060708)
	u, err := GenerateOTAUUID(WithTimestampRaw(0), WithRandomValue(randVal))
	require.NoError(t, err)
	raw := u[:]
	var randBytes [8]byte
	binary.BigEndian.PutUint64(randBytes[:], randVal)
	assert.Equal(t, randBytes[0], raw[4])  // 0x01
	assert.Equal(t, randBytes[1], raw[5])  // 0x02
	assert.Equal(t, randBytes[2], raw[7])  // 0x03
	assert.Equal(t, randBytes[3], raw[9])  // 0x04
	assert.Equal(t, randBytes[4], raw[10]) // 0x05
	assert.Equal(t, randBytes[5], raw[11]) // 0x06
	assert.Equal(t, randBytes[6], raw[12]) // 0x07
	assert.Equal(t, randBytes[7], raw[13]) // 0x08
}

func TestOtaUUID_GroupIndexValidRange(t *testing.T) {
	for gi := 0; gi < 15; gi++ {
		u, err := GenerateOTAUUID(WithTimestampRaw(0), WithRandomValue(0), WithGroupIndex(gi))
		require.NoError(t, err)
		raw := u[:]
		assert.Equal(t, byte((gi+1)&0x0F), raw[6]&0x0F, "group_index=%d", gi)
	}
}

func TestOtaUUID_GroupIndexOutOfRange(t *testing.T) {
	_, err := GenerateOTAUUID(WithGroupIndex(15))
	assert.ErrorContains(t, err, "group_index")

	_, err = GenerateOTAUUID(WithGroupIndex(-1))
	assert.ErrorContains(t, err, "group_index")
}

func TestOtaUUID_FragmentIndexValidRange(t *testing.T) {
	for fi := 0; fi < 31; fi++ {
		u, err := GenerateOTAUUID(WithTimestampRaw(0), WithRandomValue(0), WithFragmentIndex(fi))
		require.NoError(t, err)
		raw := u[:]
		assert.Equal(t, byte((fi+1)&0x1F), raw[8]&0x1F, "fragment_index=%d", fi)
	}
}

func TestOtaUUID_FragmentIndexOutOfRange(t *testing.T) {
	_, err := GenerateOTAUUID(WithFragmentIndex(31))
	assert.ErrorContains(t, err, "fragment_index")

	_, err = GenerateOTAUUID(WithFragmentIndex(-1))
	assert.ErrorContains(t, err, "fragment_index")
}

func TestOtaUUID_Reserved1Validation(t *testing.T) {
	_, err := GenerateOTAUUID(WithReserved1(0))
	assert.NoError(t, err)

	_, err = GenerateOTAUUID(WithReserved1(1))
	assert.NoError(t, err)

	_, err = GenerateOTAUUID(WithReserved1(2))
	assert.ErrorContains(t, err, "reserved1")
}

func TestOtaUUID_Reserved1Encoding(t *testing.T) {
	u, err := GenerateOTAUUID(WithTimestampRaw(0), WithRandomValue(0), WithReserved1(1))
	require.NoError(t, err)
	raw := u[:]
	assert.Equal(t, byte(0x20), raw[8]&0x20) // bit 5 set
}

func TestOtaUUID_Reserved2Validation(t *testing.T) {
	_, err := GenerateOTAUUID(WithReserved2(0))
	assert.NoError(t, err)

	_, err = GenerateOTAUUID(WithReserved2(16383)) // 2^14 - 1
	assert.NoError(t, err)

	_, err = GenerateOTAUUID(WithReserved2(16384))
	assert.ErrorContains(t, err, "reserved2")

	_, err = GenerateOTAUUID(WithReserved2(-1))
	assert.ErrorContains(t, err, "reserved2")
}

func TestOtaUUID_Reserved2Encoding(t *testing.T) {
	u, err := GenerateOTAUUID(WithTimestampRaw(0), WithRandomValue(0), WithReserved2(0x1234))
	require.NoError(t, err)
	raw := u[:]
	high := byte((0x1234 >> 8) & 0x3F)
	low := byte(0x1234 & 0xFF)
	assert.Equal(t, high, raw[14]&0x3F)
	assert.Equal(t, low, raw[15])
}

func TestOtaUUID_NoArgsUsesRandom(t *testing.T) {
	u1, err := GenerateOTAUUID()
	require.NoError(t, err)
	u2, err := GenerateOTAUUID()
	require.NoError(t, err)
	assert.NotEqual(t, u1, u2)
}

func TestOtaUUID_CombinedGroupAndFragment(t *testing.T) {
	u, err := GenerateOTAUUID(
		WithTimestampRaw(0),
		WithRandomValue(0),
		WithGroupIndex(5),
		WithFragmentIndex(10),
	)
	require.NoError(t, err)
	raw := u[:]
	assert.Equal(t, byte(6), raw[6]&0x0F)  // group_index + 1
	assert.Equal(t, byte(11), raw[8]&0x1F) // fragment_index + 1
	// marker bits still set
	assert.Equal(t, byte(0x80), raw[6]&0x80)
	assert.Equal(t, byte(0x80), raw[8]&0x80)
}
