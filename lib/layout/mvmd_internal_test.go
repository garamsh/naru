package layout

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAsTemplate_TurnsASentinelIntoItsActionAndKeepsAnOperatorsBracesAsText(t *testing.T) {
	got := asTemplate([]byte(`{"a": "` + guestInitrdSHA.sentinel + `", "h": "x{{.y}}"}`))

	assert.Equal(t, `{"a": {{json .guest_initrd.sha256}}, "h": "x{{"{{"}}.y}}"}`, string(got))
	assert.False(t, bytes.Contains(got, []byte("@guest:")), "no sentinel is left")
}
