// Package efivar encodes the UEFI variables the A/B boot of naru uses (ADR 0020): a Boot####
// entry that names a file of the EFI system partition, and BootNext, and reads and writes them
// through efivarfs. It is what the QEMU test of BootNext runs in the guest; the bytes it writes are
// the ones ADR 0020 specifies for the tool that does this in production.
package efivar

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf16"
)

// Dir is where efivarfs is mounted.
const Dir = "/sys/firmware/efi/efivars"

// GlobalGUID is the vendor GUID of the UEFI variables this package writes.
const GlobalGUID = "8be4df61-93ca-11d2-aa0d-00e098032b8c"

// Attributes of a boot variable: non-volatile, boot-service and runtime access.
const attrs = 0x7

// ESP is a partition of a GPT disk by the three values a hard-drive device path node carries.
type ESP struct {
	// Number is the partition's number from 1, Start its first LBA and Sectors its length.
	Number         uint32
	Start, Sectors uint64
	// GUID is the partition's unique GUID as the GPT stores it (the mixed-endian form), which is
	// also how a device path node carries it, so it is copied and never converted.
	GUID [16]byte
}

// DevicePath is the device path of file, a path with backslashes such as `\EFI\BOOT\TRIAL.EFI`, on
// the partition: a hard-drive node, a file-path node and the end node.
func (e ESP) DevicePath(file string) []byte {
	hd := make([]byte, 42)
	hd[0], hd[1] = 4, 1 // media device path, hard drive
	binary.LittleEndian.PutUint16(hd[2:], 42)
	binary.LittleEndian.PutUint32(hd[4:], e.Number)
	binary.LittleEndian.PutUint64(hd[8:], e.Start)
	binary.LittleEndian.PutUint64(hd[16:], e.Sectors)
	copy(hd[24:], e.GUID[:])
	hd[40], hd[41] = 2, 2 // partition format GPT, signature type GUID

	name := ucs2(file)
	fp := make([]byte, 4, 4+len(name))
	fp[0], fp[1] = 4, 4 // media device path, file path
	binary.LittleEndian.PutUint16(fp[2:], uint16(4+len(name)))
	fp = append(fp, name...)

	return append(append(hd, fp...), 0x7f, 0xff, 4, 0)
}

// LoadOption is the value of a Boot#### variable, an EFI_LOAD_OPTION that is active, has the
// description and boots the device path: attributes, the length of the path list, the
// description as null-terminated UCS-2, and the path list. The variable's own attributes come first
// in an efivarfs file, which WriteVariable adds.
func LoadOption(description string, devicePath []byte) []byte {
	desc := ucs2(description)
	out := make([]byte, 6, 6+len(desc)+len(devicePath))
	binary.LittleEndian.PutUint32(out, 1) // LOAD_OPTION_ACTIVE
	binary.LittleEndian.PutUint16(out[4:], uint16(len(devicePath)))
	return append(append(out, desc...), devicePath...)
}

// Boot returns the name of the entry with number n: Boot0100 for 0x100.
func Boot(n uint16) string { return fmt.Sprintf("Boot%04X", n) }

// ucs2 is s as null-terminated UCS-2, little endian.
func ucs2(s string) []byte {
	u := append(utf16.Encode([]rune(s)), 0)
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b
}

// WriteVariable creates or replaces the variable name under dir, an efivarfs mount, with the
// attributes of a boot variable. A file of efivarfs is written whole in one write, attributes first,
// and may be immutable: the flag is cleared on a file that exists.
func WriteVariable(dir, name string, value []byte) error {
	p := filepath.Join(dir, name+"-"+GlobalGUID)
	if _, err := os.Stat(p); err == nil {
		if err := setImmutable(p, false); err != nil {
			return err
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	buf := binary.LittleEndian.AppendUint32(nil, attrs)
	buf = append(buf, value...)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ReadVariable returns the value of the variable name under dir, without its attributes, or
// ErrNotExist when there is none.
func ReadVariable(dir, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, name+"-"+GlobalGUID))
	if err != nil {
		return nil, err
	}
	if len(b) < 4 {
		return nil, errors.New(name + " is shorter than its attributes")
	}
	return b[4:], nil
}

// Uint16 is a variable of two bytes, BootCurrent or BootNext, as a number.
func Uint16(dir, name string) (uint16, error) {
	b, err := ReadVariable(dir, name)
	if err != nil {
		return 0, err
	}
	if len(b) != 2 {
		return 0, fmt.Errorf("%s is %d bytes, not 2", name, len(b))
	}
	return binary.LittleEndian.Uint16(b), nil
}

// SetBootNext makes the firmware boot entry n once, at the next start.
func SetBootNext(dir string, n uint16) error {
	return WriteVariable(dir, "BootNext", binary.LittleEndian.AppendUint16(nil, n))
}
