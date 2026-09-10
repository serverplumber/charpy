package transcript

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// crockford is Crockford's base32 alphabet: no I, L, O or U, so a run id read
// aloud off a terminal or copied out of a bug report does not turn into a
// different one.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewRunID returns a ULID: 48 bits of millisecond timestamp followed by 80
// bits of randomness, rendered as 26 Crockford base32 characters.
//
// A ULID rather than a UUID because it sorts by creation time as a string, so
// a directory of transcripts is in run order with no metadata, and it carries
// no dependency.
func NewRunID() string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UTC().UnixMilli())<<16)
	// Bytes 6 and 7 were filled by the shifted timestamp write; the random
	// read below overwrites them, leaving exactly 48 timestamp bits.
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it ever
		// does, a run id that repeats is better than a run that cannot start.
		binary.BigEndian.PutUint64(b[8:], uint64(time.Now().UnixNano()))
	}
	return encodeULID(b)
}

func encodeULID(b [16]byte) string {
	hi := binary.BigEndian.Uint64(b[:8])
	lo := binary.BigEndian.Uint64(b[8:])

	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&0x1f]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
