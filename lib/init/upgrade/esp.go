package upgrade

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ESPGUID is the fixed partition GUID of the EFI system partition of the image's disk
// (image/host/disk/disk.env, ADR 0020). The partition is found by it, not by a device name.
const ESPGUID = "cb4162e7-9d1e-48df-b937-f5792071b950"

// The files of the two kernel slots, relative to the ESP's mount, and the trial slot's path as the
// firmware names it.
const (
	slotDir       = "EFI/BOOT"
	defaultSlot   = "BOOTX64.EFI"
	trialSlot     = "TRIAL.EFI"
	trialFirmware = `\EFI\BOOT\TRIAL.EFI`
)

// esp is the EFI system partition as a hard-drive device path node names it.
type esp struct {
	device         string
	number         uint32
	start, sectors uint64
	// guid is the partition GUID in the mixed-endian form GPT and device paths store.
	guid [16]byte
}

// findESP returns the partition of sysClassBlock whose PARTUUID is guid, with its device node
// under devDir.
func findESP(sysClassBlock, devDir, guid string) (esp, error) {
	want, err := gptGUID(guid)
	if err != nil {
		return esp{}, err
	}
	entries, err := os.ReadDir(sysClassBlock)
	if err != nil {
		return esp{}, err
	}
	for _, e := range entries {
		dir := filepath.Join(sysClassBlock, e.Name())
		uevent, err := os.ReadFile(filepath.Join(dir, "uevent"))
		if err != nil || !hasPartUUID(string(uevent), guid) {
			continue
		}
		p := esp{device: filepath.Join(devDir, e.Name()), guid: want}
		n, err1 := readNumber(filepath.Join(dir, "partition"))
		start, err2 := readNumber(filepath.Join(dir, "start"))
		size, err3 := readNumber(filepath.Join(dir, "size"))
		if err := errors.Join(err1, err2, err3); err != nil {
			return esp{}, fmt.Errorf("the geometry of %s: %w", e.Name(), err)
		}
		p.number, p.start, p.sectors = uint32(n), start, size
		return p, nil
	}
	return esp{}, fmt.Errorf("no partition has the GUID %s", guid)
}

func hasPartUUID(uevent, guid string) bool {
	for _, line := range strings.Split(uevent, "\n") {
		if v, ok := strings.CutPrefix(line, "PARTUUID="); ok && strings.EqualFold(strings.TrimSpace(v), guid) {
			return true
		}
	}
	return false
}

func readNumber(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// gptGUID converts the text form of a GUID to the mixed-endian bytes a GPT entry stores: the first
// three groups little-endian, the last two as written.
func gptGUID(s string) ([16]byte, error) {
	var out [16]byte
	hex := strings.ReplaceAll(s, "-", "")
	if len(hex) != 32 {
		return out, fmt.Errorf("%q is not a GUID", s)
	}
	var raw [16]byte
	for i := range raw {
		v, err := strconv.ParseUint(hex[2*i:2*i+2], 16, 8)
		if err != nil {
			return out, fmt.Errorf("%q is not a GUID", s)
		}
		raw[i] = byte(v)
	}
	copy(out[0:4], []byte{raw[3], raw[2], raw[1], raw[0]})
	copy(out[4:6], []byte{raw[5], raw[4]})
	copy(out[6:8], []byte{raw[7], raw[6]})
	copy(out[8:], raw[8:])
	return out, nil
}

// withESP mounts the partition for as long as fn runs and unmounts it after, so the ESP is
// mounted only while it is written or renamed. fn is given the mount point.
func (u *Upgrader) withESP(p esp, fn func(dir string) error) (err error) {
	if err := os.MkdirAll(u.cfg.ESPMount, 0o755); err != nil {
		return err
	}
	if err := u.cfg.Mount(p.device, u.cfg.ESPMount, "vfat", syscall.MS_NODEV|syscall.MS_NOSUID|syscall.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount the ESP %s: %w", p.device, err)
	}
	defer func() {
		if serr := u.cfg.Sync(u.cfg.ESPMount); serr != nil {
			err = errors.Join(err, fmt.Errorf("flush the ESP: %w", serr))
		}
		if uerr := u.cfg.Unmount(u.cfg.ESPMount, 0); uerr != nil {
			err = errors.Join(err, fmt.Errorf("unmount the ESP: %w", uerr))
		}
	}()
	return fn(filepath.Join(u.cfg.ESPMount, filepath.FromSlash(slotDir)))
}

// syncFS flushes the file system mounted at dir, and no other.
func syncFS(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(unix.Syncfs(int(d.Fd())), d.Close())
}

// syncDir fsyncs a directory so that a rename or a removal in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
