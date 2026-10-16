package webhook

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewEventID returns `evt_` and a ULID: 48 bits of milliseconds and 80
// random bits in Crockford base32, so ids sort by creation time.
func NewEventID(now time.Time) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(now.UnixMilli())<<16)
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	// 128 bits as 26 base32 digits (130 bits; the top two are zero).
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])
	var out [26]byte
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return "evt_" + string(out[:])
}

const alnum = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// NewSecret returns a signing secret: `whsec_test_` and 32 random
// alphanumerics (about 190 bits). Test-mode only, like every Waybill key.
func NewSecret() string {
	out := make([]byte, 0, 32)
	var buf [64]byte
	for len(out) < 32 {
		if _, err := rand.Read(buf[:]); err != nil {
			panic(err)
		}
		for _, c := range buf {
			if c < 248 && len(out) < 32 { // 248 = 4 × 62: no modulo bias
				out = append(out, alnum[c%62])
			}
		}
	}
	return "whsec_test_" + string(out)
}
