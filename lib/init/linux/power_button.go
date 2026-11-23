package linux

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	sysInputDir = "/sys/class/input"
	devInputDir = "/dev/input"
	// powerButtonName is the name the kernel's ACPI button driver gives the input device of a power
	// button, fixed-feature or not.
	powerButtonName = "Power Button"
	// powerButtonScan is how often the input devices are listed for a power button not yet watched.
	powerButtonScan = 5 * time.Second
	evKey           = 0x01
	keyPower        = 116
	// inputEventSize is struct input_event on a 64-bit kernel: a 16-byte time, then type, code and value.
	inputEventSize = 24
)

// WatchPowerButton calls pressed each time an ACPI power button is pressed, until ctx ends. It reads the
// event devices the kernel makes of the buttons, so a hypervisor's "shut down" reaches init as a
// request. A button that appears later is watched from the next scan.
func WatchPowerButton(ctx context.Context, log *slog.Logger, pressed func()) {
	watchPowerButton(ctx, sysInputDir, devInputDir, powerButtonScan, log, pressed)
}

func watchPowerButton(ctx context.Context, sysDir, devDir string, every time.Duration, log *slog.Logger, pressed func()) {
	var mu sync.Mutex
	watching := map[string]bool{}
	scan := func() {
		names, _ := filepath.Glob(filepath.Join(sysDir, "event*"))
		for _, p := range names {
			event := filepath.Base(p)
			name, err := os.ReadFile(filepath.Join(p, "device", "name"))
			if err != nil || strings.TrimSpace(string(name)) != powerButtonName {
				continue
			}
			mu.Lock()
			skip := watching[event]
			watching[event] = true
			mu.Unlock()
			if skip {
				continue
			}
			f, err := os.Open(filepath.Join(devDir, event))
			if err != nil {
				log.Warn("open the power button", "device", event, "err", err)
				mu.Lock()
				delete(watching, event)
				mu.Unlock()
				continue
			}
			log.Info("watching the power button", "device", event)
			go func() {
				stop := context.AfterFunc(ctx, func() { _ = f.Close() })
				defer stop()
				err := readPresses(f, pressed)
				if ctx.Err() == nil && !errors.Is(err, io.EOF) {
					log.Warn("read the power button", "device", event, "err", err)
					mu.Lock()
					delete(watching, event)
					mu.Unlock()
				}
				_ = f.Close()
			}()
		}
	}
	scan()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			scan()
		}
	}
}

// readPresses calls pressed for each press of KEY_POWER in the input events r yields, and returns the
// error that ended the stream.
func readPresses(r io.Reader, pressed func()) error {
	var ev [inputEventSize]byte
	for {
		if _, err := io.ReadFull(r, ev[:]); err != nil {
			return err
		}
		typ, code, value := binary.NativeEndian.Uint16(ev[16:]), binary.NativeEndian.Uint16(ev[18:]), int32(binary.NativeEndian.Uint32(ev[20:]))
		if typ == evKey && code == keyPower && value == 1 {
			pressed()
		}
	}
}
