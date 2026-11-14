package timesync

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"time"
)

// Sample is what one server said about the clock.
type Sample struct {
	// Offset is how far the server's time is ahead of the Node's clock, and Delay the round trip
	// without the server's own processing time (RFC 4330 section 5).
	Offset, Delay time.Duration
}

// Querier asks one server for the time.
type Querier interface {
	// Query asks addr, a host:port, and returns its answer. It gives up when ctx ends.
	Query(ctx context.Context, addr string) (Sample, error)
}

// UDP asks over SNTP on UDP. Now is the clock the offset is measured against.
type UDP struct {
	Now func() time.Time
}

// Query sends one request to addr and waits for the answer to it. A packet that does not answer
// the request is dropped and the wait goes on, until ctx ends.
func (u UDP) Query(ctx context.Context, addr string) (Sample, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return Sample{}, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return Sample{}, err
		}
	}

	// The low 16 bits of the fraction are random, so that an answer cannot be forged by one that
	// knows only the time (RFC 4330 section 3).
	var noise [2]byte
	if _, err := rand.Read(noise[:]); err != nil {
		return Sample{}, err
	}
	sent := u.Now()
	t1 := toTimestamp(sent)&^0xffff | timestamp(noise[0])<<8 | timestamp(noise[1])
	if _, err := conn.Write(request(t1)); err != nil {
		return Sample{}, err
	}
	buf := make([]byte, 512)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return Sample{}, fmt.Errorf("no answer: %w", ctx.Err())
			}
			return Sample{}, fmt.Errorf("no answer: %w", err)
		}
		t4 := u.Now()
		r, err := parseReply(buf[:n], t1)
		if errors.Is(err, errUnrelated) {
			continue
		}
		if err != nil {
			return Sample{}, err
		}
		return sample(sent, r.receive.time(), r.transmit.time(), t4), nil
	}
}

// sample returns the offset and delay of a request sent at t1, received by the server at t2,
// answered at t3 and received at t4.
func sample(t1, t2, t3, t4 time.Time) Sample {
	return Sample{
		Offset: (t2.Sub(t1) + t3.Sub(t4)) / 2,
		Delay:  t4.Sub(t1) - t3.Sub(t2),
	}
}
