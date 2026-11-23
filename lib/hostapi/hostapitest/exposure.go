// Package hostapitest is the conformance test of the host API: it drives an implementation through
// hostapi.Client alone and fails it where it departs from what the host API defines.
package hostapitest

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/wire"
)

// ExposureProtocols checks that the implementation c calls takes a lease on a port of every
// wire.Protocol and answers and lists it in that spelling, and refuses any other spelling with 422
// naming the port: the protocol orch sends is the one the host takes (ADR 0107). lease is a lease
// the implementation accepts at address as it is set up; each case sends it with its first port
// alone, changing only the protocol, and deletes it after.
func ExposureProtocols(t *testing.T, c *hostapi.Client, address string, lease hostapi.ExposureRequest) {
	t.Helper()
	require.NotEmpty(t, lease.Ports, "the lease names a port")
	with := func(p wire.Protocol) hostapi.ExposureRequest {
		r := lease
		r.Ports = []hostapi.ExposurePort{lease.Ports[0]}
		r.Ports[0].Protocol = p
		r.ProxyProtocol = ""
		return r
	}
	held := func(t *testing.T) []hostapi.ExposurePort {
		list, err := c.Exposures(t.Context())
		require.NoError(t, err)
		i := slices.IndexFunc(list, func(e hostapi.Exposure) bool { return e.Address == address })
		if i < 0 {
			return nil
		}
		return list[i].Ports
	}
	for _, p := range wire.Protocols() {
		t.Run("takes "+string(p), func(t *testing.T) {
			got, err := c.PutExposure(t.Context(), address, with(p))
			require.NoError(t, err)
			assert.Equal(t, with(p).Ports, got.Ports, "answered in the spelling it was sent in")
			assert.Equal(t, with(p).Ports, held(t), "and listed in it")
			require.NoError(t, c.DeleteExposure(t.Context(), address))
		})
	}
	var other []wire.Protocol
	for _, p := range wire.Protocols() {
		other = append(other, wire.Protocol(strings.ToLower(string(p))))
	}
	for _, p := range append(other, "Tcp", "SCTP", "") {
		t.Run("refuses "+string(p), func(t *testing.T) {
			_, err := c.PutExposure(t.Context(), address, with(p))
			var refused *hostapi.Error
			require.True(t, errors.As(err, &refused), "refused with a host API error: %v", err)
			assert.Equal(t, http.StatusUnprocessableEntity, refused.Status)
			assert.Equal(t, []string{"ports[0]"}, refused.Fields, "naming the port")
			assert.Empty(t, held(t), "and holds nothing")
		})
	}
}
