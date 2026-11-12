package timesync_test

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/timesync"
)

// ntpToUnix is the seconds from 1900-01-01 to 1970-01-01.
const ntpToUnix = 2208988800

// stamp returns t as the 8 bytes of an NTP timestamp, all zero for the zero time.
func stamp(t time.Time) []byte {
	if t.IsZero() {
		return make([]byte, 8)
	}
	secs := uint64(t.Unix()+ntpToUnix) & 0xffffffff
	frac := uint64(t.Nanosecond()) << 32 / 1e9
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, secs<<32|frac)
	return b
}

// answer is what a test server sends for a request: a server packet with the given receive and
// transmit times, and the options a test changes.
type answer struct {
	receive, transmit time.Time
	leap              byte
	stratum           byte
	kiss              string
	mode              byte
	// origin replaces the request's transmit timestamp when it is not nil.
	origin []byte
}

func (a answer) packet(req []byte) []byte {
	b := make([]byte, 48)
	mode := a.mode
	if mode == 0 {
		mode = 4
	}
	b[0] = a.leap<<6 | 4<<3 | mode
	b[1] = a.stratum
	copy(b[12:16], a.kiss)
	if a.origin != nil {
		copy(b[24:32], a.origin)
	} else {
		copy(b[24:32], req[40:48])
	}
	copy(b[32:40], stamp(a.receive))
	copy(b[40:48], stamp(a.transmit))
	return b
}

// sntpServer serves each request on a loopback socket with the packets reply returns for it. It
// returns the address and the requests it received.
func sntpServer(t *testing.T, reply func(req []byte) [][]byte) (addr string, requests func() [][]byte) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var got [][]byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 512)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			req := append([]byte(nil), buf[:n]...)
			mu.Lock()
			got = append(got, req)
			mu.Unlock()
			for _, p := range reply(req) {
				_, _ = conn.WriteTo(p, from)
			}
		}
	}()
	t.Cleanup(func() {
		conn.Close()
		<-done
	})
	return conn.LocalAddr().String(), func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), got...)
	}
}

// steppedNow returns a clock that reads start, then start+gap, and stays there.
func steppedNow(start time.Time, gap time.Duration) func() time.Time {
	var mu sync.Mutex
	n := 0
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return start
		}
		return start.Add(gap)
	}
}

func query(t *testing.T, now func() time.Time, addr string, wait time.Duration) (timesync.Sample, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return timesync.UDP{Now: now}.Query(ctx, addr)
}

func TestUDPQuery_MeasuresTheOffsetAndDelayFromTheFourTimestamps(t *testing.T) {
	base := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	// The server is 1 s ahead; the request takes 50 ms to arrive and the answer 50 ms to return.
	server := base.Add(time.Second + 50*time.Millisecond)
	addr, _ := sntpServer(t, func(req []byte) [][]byte {
		return [][]byte{answer{receive: server, transmit: server, stratum: 2}.packet(req)}
	})

	got, err := query(t, steppedNow(base, 100*time.Millisecond), addr, 2*time.Second)

	require.NoError(t, err)
	assert.InDelta(t, time.Second.Seconds(), got.Offset.Seconds(), 0.001)
	assert.InDelta(t, (100 * time.Millisecond).Seconds(), got.Delay.Seconds(), 0.001)
}

func TestUDPQuery_SendsAClientRequestThatCarriesItsTransmitTimestamp(t *testing.T) {
	base := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	addr, requests := sntpServer(t, func(req []byte) [][]byte {
		return [][]byte{answer{receive: base, transmit: base, stratum: 1}.packet(req)}
	})

	_, err := query(t, steppedNow(base, 0), addr, 2*time.Second)

	require.NoError(t, err)
	got := requests()
	require.Len(t, got, 1)
	require.Len(t, got[0], 48)
	assert.Equal(t, byte(4<<3|3), got[0][0], "version 4, mode client")
	assert.Equal(t, stamp(base)[:6], got[0][40:46], "the transmit timestamp is the time the clock read, but for its random low bits")
}

func TestUDPQuery_ReadsATimestampPastTheYear2036AsTheSecondEra(t *testing.T) {
	// The seconds of the timestamp wrapped to a value whose top bit is 0. The control is the test
	// above, whose 2030 times have the top bit set.
	base := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	server := base.Add(2 * time.Second)
	addr, _ := sntpServer(t, func(req []byte) [][]byte {
		return [][]byte{answer{receive: server, transmit: server, stratum: 2}.packet(req)}
	})

	got, err := query(t, steppedNow(base, 0), addr, 2*time.Second)

	require.NoError(t, err)
	assert.InDelta(t, (2 * time.Second).Seconds(), got.Offset.Seconds(), 0.001)
}

func TestUDPQuery_DropsAPacketThatDoesNotAnswerTheRequestAndWaitsForTheAnswer(t *testing.T) {
	base := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	forged := base.Add(time.Hour)
	addr, _ := sntpServer(t, func(req []byte) [][]byte {
		return [][]byte{
			answer{receive: forged, transmit: forged, stratum: 2, origin: make([]byte, 8)}.packet(req),
			{1, 2, 3},
			answer{receive: base, transmit: base, stratum: 2}.packet(req),
		}
	})

	got, err := query(t, steppedNow(base, 0), addr, 2*time.Second)

	require.NoError(t, err)
	assert.Zero(t, got.Offset.Round(time.Millisecond), "the answer is the third packet")
}

func TestUDPQuery_FailsWhenOnlyPacketsThatDoNotAnswerTheRequestArrive(t *testing.T) {
	base := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	addr, _ := sntpServer(t, func(req []byte) [][]byte {
		return [][]byte{answer{receive: base, transmit: base, stratum: 2, origin: make([]byte, 8)}.packet(req)}
	})

	_, err := query(t, steppedNow(base, 0), addr, 200*time.Millisecond)

	assert.ErrorContains(t, err, "no answer")
}

func TestUDPQuery_RefusesAnAnswerThatCannotBeUsed(t *testing.T) {
	base := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		a    answer
		want string
	}{
		"kiss-o'-death":    {answer{receive: base, transmit: base, stratum: 0, kiss: "RATE"}, `"RATE"`},
		"not synchronised": {answer{receive: base, transmit: base, stratum: 2, leap: 3}, "not synchronised"},
		"stratum 16":       {answer{receive: base, transmit: base, stratum: 16}, "stratum"},
		"not a server":     {answer{receive: base, transmit: base, stratum: 2, mode: 3}, "mode"},
		"no time":          {answer{stratum: 2}, "no time"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			addr, _ := sntpServer(t, func(req []byte) [][]byte { return [][]byte{tt.a.packet(req)} })

			// The wait is long enough that failing fast, not timing out, is what the test sees.
			start := time.Now()
			_, err := query(t, steppedNow(base, 0), addr, 5*time.Second)

			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
			assert.Less(t, time.Since(start), 2*time.Second, "an unusable answer ends the query at once")
		})
	}
}

func TestUDPQuery_FailsWhenTheServerIsSilent(t *testing.T) {
	addr, _ := sntpServer(t, func([]byte) [][]byte { return nil })

	_, err := query(t, time.Now, addr, 200*time.Millisecond)

	assert.ErrorContains(t, err, "no answer")
}
