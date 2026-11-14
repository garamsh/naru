package timesync

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

const (
	packetLen = 48
	// ntpToUnix is the seconds from 1900-01-01, the NTP epoch, to 1970-01-01.
	ntpToUnix = 2208988800

	modeClient  = 3
	modeServer  = 4
	versionSNTP = 4
	// leapAlarm is the leap indicator of a server that is not synchronised.
	leapAlarm = 3
	// maxStratum is the highest stratum of a server that is synchronised.
	maxStratum = 15
)

// timestamp is an NTP 64-bit timestamp: seconds since 1900 and a binary fraction of a second.
type timestamp uint64

// toTimestamp returns t as a timestamp. Seconds wrap every 2^32 s.
func toTimestamp(t time.Time) timestamp {
	secs := uint64(t.Unix()+ntpToUnix) & 0xffffffff
	frac := uint64(t.Nanosecond()) << 32 / 1e9
	return timestamp(secs<<32 | frac)
}

// time returns the instant ts names. A timestamp whose most significant bit is 0 is taken to be
// in the second era, from 2036, as RFC 4330 section 3 prescribes.
func (ts timestamp) time() time.Time {
	secs := int64(ts >> 32)
	if secs&0x80000000 == 0 {
		secs += 1 << 32
	}
	nanos := int64(uint64(ts&0xffffffff) * 1e9 >> 32)
	return time.Unix(secs-ntpToUnix, nanos)
}

// request returns the 48 bytes asking for the time, with transmit as the transmit timestamp.
func request(transmit timestamp) []byte {
	b := make([]byte, packetLen)
	b[0] = versionSNTP<<3 | modeClient
	binary.BigEndian.PutUint64(b[40:], uint64(transmit))
	return b
}

// reply is the part of a server's packet the offset needs.
type reply struct {
	origin, receive, transmit timestamp
}

// errUnrelated is the error of a packet that does not answer the request.
var errUnrelated = errors.New("the packet does not answer this request")

// parseReply returns what a server's packet says. It returns errUnrelated for a packet that is
// not the answer to the request whose transmit timestamp was origin, and another error for an
// answer that cannot be used (RFC 4330 section 5).
func parseReply(b []byte, origin timestamp) (reply, error) {
	if len(b) < packetLen {
		return reply{}, errUnrelated
	}
	leap, version, mode := b[0]>>6, b[0]>>3&7, b[0]&7
	r := reply{
		origin:   timestamp(binary.BigEndian.Uint64(b[24:])),
		receive:  timestamp(binary.BigEndian.Uint64(b[32:])),
		transmit: timestamp(binary.BigEndian.Uint64(b[40:])),
	}
	switch {
	case r.origin != origin:
		return reply{}, errUnrelated
	case mode != modeServer:
		return reply{}, fmt.Errorf("the mode is %d, not a server's", mode)
	case version < 1 || version > versionSNTP:
		return reply{}, fmt.Errorf("the version is %d", version)
	case b[1] == 0:
		return reply{}, fmt.Errorf("the server refused with the kiss code %q", string(b[12:16]))
	case b[1] > maxStratum:
		return reply{}, fmt.Errorf("the stratum is %d", b[1])
	case leap == leapAlarm:
		return reply{}, errors.New("the server is not synchronised")
	case r.receive == 0 || r.transmit == 0:
		return reply{}, errors.New("the packet carries no time")
	}
	return r, nil
}
