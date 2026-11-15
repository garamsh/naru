package command

import (
	"testing"

	"github.com/stretchr/testify/assert"
	req "github.com/stretchr/testify/require"
)

func TestLoopbackHosts_RefusesAnAddressWithoutAPort(t *testing.T) {
	_, err := loopbackHosts(fakeAddr("no-port"))
	req.Error(t, err, "an address it cannot read would turn the Host guard off")

	hosts, err := loopbackHosts(fakeAddr("127.0.0.1:8484"))
	req.NoError(t, err)
	assert.Equal(t, []string{"127.0.0.1:8484", "localhost:8484"}, hosts)
}

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }
