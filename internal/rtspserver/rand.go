package rtspserver

import (
	"crypto/rand"
	"encoding/binary"
)

// randUint32 returns a cryptographically random uint32, used to seed the
// RTP timestamp offset (RFC 3550 §5.1).
func randUint32() (uint32, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}
