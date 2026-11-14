package service_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/naru/lib/init/service"
)

func TestBackoff_Next(t *testing.T) {
	var b service.Backoff

	var got []time.Duration
	for range 8 {
		got = append(got, b.Next(time.Second))
	}
	assert.Equal(t, []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
	}, got)

	assert.Equal(t, time.Second, b.Next(60*time.Second), "a run of 60 s resets the delay")
	assert.Equal(t, 2*time.Second, b.Next(59*time.Second), "a shorter run does not")
}
