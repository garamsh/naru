package main

import (
	"fmt"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFields_ReadsTheMembersNamesOfAMemberListAnswer(t *testing.T) {
	member := func(name string) []byte {
		b := []byte{0x08, 0x07, 0x12, byte(len(name))} // ID 7, name
		return append(b, name...)
	}
	var answer []byte
	answer = append(answer, 0x0a, 0x02, 0x08, 0x01) // header
	for _, n := range []string{"node-1", "node-2", "node-3"} {
		m := member(n)
		answer = append(append(answer, 0x12, byte(len(m))), m...)
	}
	var names []string
	for _, m := range fields(answer, 2) {
		for _, n := range fields(m, 2) {
			names = append(names, string(n))
		}
	}
	assert.Equal(t, []string{"node-1", "node-2", "node-3"}, names)
	assert.Empty(t, fields(answer[:5], 2), "a truncated answer yields no member")
}

func TestClassify_NamesHowTheRequestEnded(t *testing.T) {
	assert.Equal(t, "connected", classify(200, nil))
	assert.Equal(t, "answered-403", classify(403, nil))
	assert.Equal(t, "refused-by-tcp-reset", classify(0, fmt.Errorf("dial: %w", syscall.ECONNREFUSED)))
	assert.Equal(t, "refused-at-tls-handshake", classify(0, fmt.Errorf("remote error: tls: certificate required")))
	assert.Equal(t, "timeout", classify(0, fmt.Errorf("i/o timeout")))
}
