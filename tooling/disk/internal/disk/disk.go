// Package disk builds naru's UEFI disk image: a raw GPT disk with one EFI system partition that
// holds the kernel, with its initramfs inside it, as \EFI\BOOT\BOOTX64.EFI, the default boot slot,
// and room for a second kernel, the trial slot \EFI\BOOT\TRIAL.EFI that an upgrade writes
// (ADR 0020), each slot up to twice the size of the kernel the disk was built with (ADR 0037). The
// same inputs give the same bytes: every GUID, the volume id and every timestamp are arguments.
package disk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// Where the extension images and their index are in the ESP (ADR 0096): the 8.3 names of the file
// system, so an image's own name is in the index, and init reads the index from the same names.
const (
	ExtensionDir   = "EXT"
	ExtensionIndex = "EXT/INDEX.JSN"
)

// ExtensionRoom is the room an ESP needs for images that hold size bytes together: their
// clusters, the file allocation tables' two copies (a 64th of them with 512-byte clusters), the
// index and a MiB of slack, a whole number of MiB. It is added to the room the slots need, so an
// extension takes nothing of what an upgrade's kernel has.
func ExtensionRoom(size int64) int64 {
	if size == 0 {
		return 0
	}
	return roundUp(size+size/64+mib, mib)
}

// ESPBytes is the size of the ESP of a disk built with a kernel of kernelSize bytes: the larger of
// 1 GiB and Slots slots of slotGrowth kernels each with slack, a whole number of MiB (ADR 0037).
func ESPBytes(kernelSize int64) int64 {
	return max(minESPBytes, roundUp(Slots*slotGrowth*kernelSize+espSlackByte, mib))
}

// Layout is what a disk spent on its extensions, which the release reads from the file naru-disk
// writes beside the disk (-layout) instead of recomputing it: the ESP's room for kernels is its
// size less ExtensionRoom (ADR 0096).
type Layout struct {
	ExtensionRoom int64 `json:"extensionRoom"`
}

// Layout returns the layout of the disk Build writes for s.
func (s Spec) Layout() (Layout, error) {
	_, _, total, err := loadExtensions(s.Extensions)
	return Layout{ExtensionRoom: ExtensionRoom(total)}, err
}

// Extension is a system extension the disk carries: Description is the extension build's
// <name>.json (name, version, sha256, services) and its image is <name>.sqfs beside it.
type Extension struct {
	Description string
}

type described struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// indexEntry and index are what the file ExtensionIndex holds; lib/init/bootmedia.Index reads the
// same fields, which the QEMU test of a blank Node boots a disk to prove.
type indexEntry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
	File    string `json:"file"`
}

type index struct {
	Extensions []indexEntry `json:"extensions"`
}

// load reads the description and checks that the image beside it has the digest it names.
func (e Extension) load() (entry indexEntry, image string, err error) {
	raw, err := os.ReadFile(e.Description)
	if err != nil {
		return entry, "", err
	}
	var d described
	if err := json.Unmarshal(raw, &d); err != nil {
		return entry, "", fmt.Errorf("%s: %w", e.Description, err)
	}
	if d.Name == "" || d.Version == "" || d.SHA256 == "" {
		return entry, "", fmt.Errorf("%s: no name, version and sha256", e.Description)
	}
	image = e.Description[:len(e.Description)-len(filepath.Ext(e.Description))] + ".sqfs"
	f, err := os.Open(image)
	if err != nil {
		return entry, "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return entry, "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != d.SHA256 {
		return entry, "", fmt.Errorf("%s has the SHA-256 %s, not the %s that %s names", image, got, d.SHA256, e.Description)
	}
	return indexEntry{Name: d.Name, Version: d.Version, SHA256: d.SHA256, Bytes: n}, image, nil
}

// Spec says what to build.
type Spec struct {
	// Kernel is the bzImage with the initramfs embedded, Out the disk image to write.
	Kernel, Out string
	// Stamp is the time of every directory entry and the source of the FAT volume id.
	Stamp time.Time
	// DiskGUID and ESPGUID are the GUIDs of the disk and of its one partition.
	DiskGUID, ESPGUID string
	// Extensions are the system extensions the ESP carries beside the kernel, for init to take into a
	// blank Node's store at its first boot (ADR 0096).
	Extensions []Extension
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

	entries, images, extBytes, err := loadExtensions(s.Extensions)
	if err != nil {
		return err
	}
	espBytes := ESPBytes(kernelSize) + ExtensionRoom(extBytes)
	tmp, err := os.MkdirTemp(filepath.Dir(s.Out), "naru-disk-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	esp := filepath.Join(tmp, "esp.img")
	if err := makeESP(esp, espBytes, s.Stamp, kernel, kernelSize, entries, images); err != nil {
		return err
	}
	if err := assemble(s, esp, espBytes); err != nil {
		return err
	}
	return verify(s, kernel)
}

func makeESP(esp string, length int64, stamp time.Time, kernel io.Reader, kernelSize int64, entries []indexEntry, images []string) error {
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
	if err := addExtensions(v, entries, images); err != nil {
		_ = v.Close()
		return err
	}
	return v.Close()
}

// loadExtensions reads each extension, names its file in the ESP E0.SQF, E1.SQF and so on, and
// returns the index entries, the image paths and the images' total size.
func loadExtensions(exts []Extension) (entries []indexEntry, images []string, total int64, err error) {
	for i, e := range exts {
		entry, image, err := e.load()
		if err != nil {
			return nil, nil, 0, err
		}
		for _, have := range entries {
			if have.Name == entry.Name {
				return nil, nil, 0, fmt.Errorf("the extension %q is given twice", entry.Name)
			}
		}
		entry.File = fmt.Sprintf("E%d.SQF", i)
		entries = append(entries, entry)
		images = append(images, image)
		total += entry.Bytes
	}
	return entries, images, total, nil
}

func addExtensions(v *fat.Volume, entries []indexEntry, images []string) error {
	if len(entries) == 0 {
		return nil
	}
	for i, e := range entries {
		f, err := os.Open(images[i])
		if err != nil {
			return err
		}
		err = v.Add(ExtensionDir+"/"+e.File, f, e.Bytes)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	raw, err := json.Marshal(index{Extensions: entries})
	if err != nil {
		return err
	}
	return v.Add(ExtensionIndex, bytes.NewReader(raw), int64(len(raw)))
}

// copySparse copies src into dst from offset off and writes no block of zeros, so a hole of dst,
// which must already be as long as the copy ends, stays one: the ESP is 1 GiB and nearly all zeros,
// and writing it whole costs a build and a test seconds of disk time and a GiB each.
func copySparse(dst *os.File, src io.Reader, off int64) error {
	buf := make([]byte, 64<<10)
	for {
		n, err := io.ReadFull(src, buf)
		if len(bytes.TrimLeft(buf[:n], "\x00")) > 0 {
			if _, werr := dst.WriteAt(buf[:n], off); werr != nil {
				return werr
			}
		}
		off += int64(n)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
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
	copyErr := copySparse(out, in, mib)
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
	if err := esp.Truncate(info.Size() - 2*mib); err != nil {
		_ = esp.Close()
		return err
	}
	if err := copySparse(esp, io.NewSectionReader(f, mib, info.Size()-2*mib), 0); err != nil {
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
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return nil
}
