//go:build qemu_real

package tests_test

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// ntpEpoch is the seconds between 1900, which NTP counts from, and 1970.
const ntpEpoch = 2208988800

// sntpServer answers SNTP requests (RFC 4330) on the loopback of the test host with the host's own
// clock, and counts them. A Node reaches it at 10.0.2.2, which QEMU's user-mode network maps to the
// host's loopback. It stops when the test ends.
func sntpServer(t *testing.T) (port string, requests *atomic.Int32) {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	requests = new(atomic.Int32)
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 48 {
				continue
			}
			requests.Add(1)
			now := time.Now()
			stamp := make([]byte, 8)
			binary.BigEndian.PutUint32(stamp, uint32(now.Unix()+ntpEpoch))
			binary.BigEndian.PutUint32(stamp[4:], uint32(uint64(now.Nanosecond())<<32/1e9))
			reply := make([]byte, 48)
			reply[0], reply[1] = 4<<3|4, 2 // version 4, mode server, stratum 2
			copy(reply[16:24], stamp)      // reference
			copy(reply[24:32], buf[40:48]) // origin: the client's transmit timestamp
			copy(reply[32:40], stamp)      // receive
			copy(reply[40:48], stamp)      // transmit
			_, _ = conn.WriteTo(reply, from)
		}
	}()
	return strconv.Itoa(conn.LocalAddr().(*net.UDPAddr).Port), requests
}

// timeBody is the part of GET /v1/time this test reads.
type timeBody struct {
	Configured   bool     `json:"configured"`
	Servers      []string `json:"servers"`
	Synchronised bool     `json:"synchronised"`
	LastSync     *struct {
		Server        string  `json:"server"`
		OffsetSeconds float64 `json:"offsetSeconds"`
		Correction    string  `json:"correction"`
	} `json:"lastSync"`
}

func getTime(t *testing.T, file string) timeBody {
	t.Helper()
	hc, endpoint := apiClient(t, file)
	resp, err := hc.Get("https://" + endpoint + "/v1/time")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body timeBody
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// TestNode_ANodeWithAWrongClockStepsItFromSpecTimeServersBeforeItIssuesCertificates boots a Node
// whose real-time clock starts years in the past, with spec.time naming an SNTP server that runs on
// the test host, and a spec.pki that has init issue certificates. The console must show the clock
// stepped from that server before init wrote the authority, and the Node API must report the step.
func TestNode_ANodeWithAWrongClockStepsItFromSpecTimeServersBeforeItIssuesCertificates(t *testing.T) {
	port, requests := sntpServer(t)
	server := "10.0.2.2:" + port
	c := start(t, node.ClusterSpec{
		N: 1, Gaon: true, BootPKI: true, NodeAPI: true,
		TimeServers: []string{server}, RTCBase: "2021-03-04T05:06:07",
	})
	n := c.Nodes[0]

	expectAll(t, n, judge.TimeSynced(server))
	expectAll(t, n, judge.Issuance(judge.GaonIssued...))
	eventually(t, "the Node API answers an admin", func() bool { return ctl(c.API.Admin, "version").Err == nil })

	assert.Positive(t, requests.Load(), "the server on the host was asked")
	got := getTime(t, c.API.Admin)
	assert.True(t, got.Configured)
	assert.Equal(t, []string{server}, got.Servers)
	assert.True(t, got.Synchronised)
	if assert.NotNil(t, got.LastSync) {
		assert.Equal(t, server, got.LastSync.Server)
		assert.Equal(t, "step", got.LastSync.Correction)
		year := (365 * 24 * time.Hour).Seconds()
		assert.Greater(t, got.LastSync.OffsetSeconds, year, "the clock was years behind before the step")
	}
}
