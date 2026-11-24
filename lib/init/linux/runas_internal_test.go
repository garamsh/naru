package linux

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"

	"github.com/garamsh/naru/lib/init/config"
)

// The capability is the kernel's to grant at exec, which needs root, so this white-box test reads
// what init asks the kernel for.
func TestProcAttr_AServiceThatBindsLowPortsAsAUserIsStartedWithThatOneCapability(t *testing.T) {
	user := &config.User{UID: 6003, GID: 6003}
	attr := procAttr(config.Service{User: user, BindLowPorts: true}, 7)
	assert.Equal(t, []uintptr{unix.CAP_NET_BIND_SERVICE}, attr.AmbientCaps)
	assert.Equal(t, uint32(6003), attr.Credential.Uid)
	assert.Equal(t, 7, attr.CgroupFD)

	assert.Empty(t, procAttr(config.Service{User: user}, 7).AmbientCaps, "control: a user's service without bindLowPorts gets none")
	assert.Empty(t, procAttr(config.Service{BindLowPorts: true}, 7).AmbientCaps, "root's service needs none")
}
