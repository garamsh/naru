// Package disk builds naru's UEFI disk image: a raw GPT disk with one EFI system partition that
// holds the kernel, with its initramfs inside it, as \EFI\BOOT\BOOTX64.EFI, the default boot slot,
// and room for a second kernel, the trial slot \EFI\BOOT\TRIAL.EFI that an upgrade writes
// (ADR 0020), each slot up to twice the size of the kernel the disk was built with (ADR 0037). The
// same inputs give the same bytes: every GUID, the volume id and every timestamp are arguments.
package disk

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/garamsh/naru/tooling/disk/internal/fat"
)

// Paths inside the ESP. Loader is the default slot, the one firmware boots when nothing says
// otherwise. Trial is where an upgrade writes the new kernel; the disk is built without it.
// Both are 8.3 names in one directory, so promoting the trial is one rename within it.
const (
	Loader = "EFI/BOOT/BOOTX64.EFI"
	Trial  = "EFI/BOOT/TRIAL.EFI"
)

// StartSector is the first sector of the ESP, and Slots the number of kernels it has room for.
// The device path of a file in the ESP is therefore known without the disk: HD(1,GPT,<ESP GUID>,
// StartSector,<partition sectors>)/File(\EFI\BOOT\TRIAL.EFI).
const (
	StartSector = mib / sectorBytes
	Slots       = 2
)

// The host tools. They are named by absolute path because /sbin is not on an ordinary user's PATH.
const (
	mkfsFAT = "/sbin/mkfs.fat"
	sgdisk  = "/sbin/sgdisk"
)

const (
	mib          = 1 << 20
	sectorBytes  = 512
	espTypeGUID  = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	espLabel     = "NARUESP"
	espPartName  = "naru-esp"
	minESPBytes  = 1024 * mib
	espSlackByte = 8 * mib
	// slotGrowth is how many times the built kernel's size a slot is meant to hold.
	slotGrowth = 2
)

// ESPBytes is the size of the ESP of a disk built with a kernel of kernelSize bytes: the larger of
// 1 GiB and Slots slots of slotGrowth kernels each with slack, a whole number of MiB (ADR 0037).
func ESPBytes(kernelSize int64) int64 {
	return max(minESPBytes, roundUp(Slots*slotGrowth*kernelSize+espSlackByte, mib))
}

// Spec says what to build.
type Spec struct {
	// Kernel is the bzImage with the initramfs embedded, Out the disk image to write.
	Kernel, Out string
	// Stamp is the time of every directory entry and the source of the FAT volume id.
	Stamp time.Time
	// DiskGUID and ESPGUID are the GUIDs of the disk and of its one partition.
	DiskGUID, ESPGUID string
}

// Build writes the disk image Out and reads the kernel back from it.
func Build(s Spec) error {
	kernel, err := os.Open(s.Kernel)
	if err != nil {
		return err
	}
	defer func() { _ = kernel.Close() }()
	kernelSize, err := size(kernel)
	if err != nil {
		return err
	}

	espBytes := ESPBytes(kernelSize)
	tmp, err := os.MkdirTemp(filepath.Dir(s.Out), "naru-disk-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	esp := filepath.Join(tmp, "esp.img")
	if err := makeESP(esp, espBytes, s.Stamp, kernel, kernelSize); err != nil {
		return err
	}
	if err := assemble(s, esp, espBytes); err != nil {
		return err
	}
	return verify(s, kernel)
}

func makeESP(esp string, length int64, stamp time.Time, kernel io.Reader, kernelSize int64) error {
	if err := os.WriteFile(esp, nil, 0o644); err != nil {
		return err
	}
	if err := os.Truncate(esp, length); err != nil {
		return err
	}
	// --invariant fixes what mkfs.fat would take from the clock and the random pool; -i then sets the volume id.
	if err := run(mkfsFAT, "-F", "32", "-S", "512", "-s", "1", "-n", espLabel, "--invariant", "-i", fmt.Sprintf("%08x", uint32(stamp.Unix())), esp); err != nil {
		return err
	}
	v, err := fat.Open(esp, stamp)
	if err != nil {
		return err
	}
	if err := v.Add(Loader, kernel, kernelSize); err != nil {
		_ = v.Close()
		return err
	}
	return v.Close()
}

// assemble lays the ESP into a disk whose first partition starts at 1 MiB, with a MiB after it
// for the backup GPT, and writes the partition table with sgdisk.
func assemble(s Spec, esp string, espBytes int64) error {
	out, err := os.Create(s.Out)
	if err != nil {
		return err
	}
	if err := out.Truncate(mib + espBytes + mib); err != nil {
		_ = out.Close()
		return err
	}
	in, err := os.Open(esp)
	if err != nil {
		_ = out.Close()
		return err
	}
	_, copyErr := io.Copy(io.NewOffsetWriter(out, mib), in)
	err = errors.Join(copyErr, in.Close(), out.Close())
	if err != nil {
		return err
	}
	first := int64(StartSector)
	last := first + espBytes/sectorBytes - 1
	return run(sgdisk,
		"--clear",
		"--disk-guid="+s.DiskGUID,
		fmt.Sprintf("--new=1:%d:%d", first, last),
		"--typecode=1:"+espTypeGUID,
		"--partition-guid=1:"+s.ESPGUID,
		"--change-name=1:"+espPartName,
		s.Out,
	)
}

// verify reads the kernel back out of the disk image and fails unless it is the bytes that went in.
func verify(s Spec, kernel *os.File) error {
	info, err := os.Stat(s.Out)
	if err != nil {
		return err
	}
	f, err := os.Open(s.Out)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	esp, err := os.CreateTemp(filepath.Dir(s.Out), "naru-verify-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(esp.Name()) }()
	if _, err := io.Copy(esp, io.NewSectionReader(f, mib, info.Size()-2*mib)); err != nil {
		_ = esp.Close()
		return err
	}
	if err := esp.Close(); err != nil {
		return err
	}
	v, err := fat.Open(esp.Name(), s.Stamp)
	if err != nil {
		return err
	}
	defer func() { _ = v.Close() }()
	for path, src := range map[string]*os.File{Loader: kernel} {
		got, err := v.ReadFile(path)
		if err != nil {
			return err
		}
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return err
		}
		want, err := io.ReadAll(src)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("%s in %s is not the file that went in", path, s.Out)
		}
	}
	return nil
}

func size(f *os.File) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func roundUp(n, to int64) int64 { return (n + to - 1) / to * to }

func run(name string, args ...string) error {
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil { //nolint:gosec // the tool and its arguments are built here
		return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return nil
}
