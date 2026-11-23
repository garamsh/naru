package linux

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func inputEvent(typ, code uint16, value int32) []byte {
	b := make([]byte, inputEventSize)
	binary.NativeEndian.PutUint16(b[16:], typ)
	binary.NativeEndian.PutUint16(b[18:], code)
	binary.NativeEndian.PutUint32(b[20:], uint32(value))
	return b
}

func TestReadPresses_CountsOnlyThePressOfThePowerKey(t *testing.T) {
	var stream bytes.Buffer
	for _, ev := range [][]byte{
		inputEvent(evKey, keyPower, 0), // a release
		inputEvent(evKey, keyPower, 2), // a repeat
		inputEvent(evKey, 30, 1),       // another key
		inputEvent(0, keyPower, 1),     // another event type
		inputEvent(evKey, keyPower, 1), // the press
	} {
		stream.Write(ev)
	}
	var n int
	err := readPresses(&stream, func() { n++ })
	require.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 1, n, "one press of five events")

	var control int
	_ = readPresses(bytes.NewReader(inputEvent(evKey, keyPower, 1)), func() { control++ })
	assert.Equal(t, 1, control, "control: the press alone is counted")
}

func TestWatchPowerButton_ReadsTheDeviceNamedPowerButtonAndNoOther(t *testing.T) {
	sys, dev := t.TempDir(), t.TempDir()
	device := func(event, name string, events ...[]byte) {
		require.NoError(t, os.MkdirAll(filepath.Join(sys, event, "device"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sys, event, "device", "name"), []byte(name+"\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dev, event), bytes.Join(events, nil), 0o644))
	}
	device("event0", "Power Button", inputEvent(evKey, keyPower, 1))
	device("event1", "AT Translated Set 2 keyboard", inputEvent(evKey, keyPower, 1), inputEvent(evKey, keyPower, 1))

	var n atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		watchPowerButton(ctx, sys, dev, time.Hour, slog.New(slog.DiscardHandler), func() { n.Add(1) })
		close(done)
	}()
	require.Eventually(t, func() bool { return n.Load() >= 1 }, 5*time.Second, 10*time.Millisecond, "the power button's press is delivered")
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), n.Load(), "the keyboard's presses of the same key are not: only a device named Power Button is read")
	cancel()
	<-done
}
