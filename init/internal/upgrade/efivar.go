package upgrade

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf16"
)

// efiVarGUID is the vendor GUID of the UEFI global variables the upgrade writes and reads.
const efiVarGUID = "8be4df61-93ca-11d2-aa0d-00e098032b8c"

// varAttrs are the attributes of a boot variable: non-volatile, boot-service and runtime access.
const varAttrs = 0x7

// trialEntry is the number of the firmware entry that boots the trial slot; no BootOrder lists it,
// so only BootNext reaches it.
const trialEntry = 0x0100

// bootName returns the name of the entry with number n: Boot0100 for 0x100.
func bootName(n uint16) string { return fmt.Sprintf("Boot%04X", n) }

// devicePath is the device path of file, a path with backslashes such as `\EFI\BOOT\TRIAL.EFI`, on
// the partition: a hard-drive node, a file-path node and the end node.
func (e esp) devicePath(file string) []byte {
	hd := make([]byte, 42)
	hd[0], hd[1] = 4, 1 // media device path, hard drive
	binary.LittleEndian.PutUint16(hd[2:], 42)
	binary.LittleEndian.PutUint32(hd[4:], e.number)
	binary.LittleEndian.PutUint64(hd[8:], e.start)
	binary.LittleEndian.PutUint64(hd[16:], e.sectors)
	copy(hd[24:], e.guid[:])
	hd[40], hd[41] = 2, 2 // partition format GPT, signature type GUID

	name := ucs2(file)
	fp := make([]byte, 4, 4+len(name))
	fp[0], fp[1] = 4, 4 // media device path, file path
	binary.LittleEndian.PutUint16(fp[2:], uint16(4+len(name)))
	fp = append(fp, name...)

	return append(append(hd, fp...), 0x7f, 0xff, 4, 0)
}

// loadOption is the value of a Boot#### variable, an EFI_LOAD_OPTION that is active, has the
// description and boots the device path: attributes, the length of the path list, the description
// as null-terminated UCS-2, and the path list.
func loadOption(description string, devicePath []byte) []byte {
	desc := ucs2(description)
	out := make([]byte, 6, 6+len(desc)+len(devicePath))
	binary.LittleEndian.PutUint32(out, 1) // LOAD_OPTION_ACTIVE
	binary.LittleEndian.PutUint16(out[4:], uint16(len(devicePath)))
	return append(append(out, desc...), devicePath...)
}

func ucs2(s string) []byte {
	u := append(utf16.Encode([]rune(s)), 0)
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b
}

func varPath(dir, name string) string { return filepath.Join(dir, name+"-"+efiVarGUID) }

// writeVariable creates or replaces the variable name under dir, an efivarfs mount. A file of
// efivarfs is written whole in one write, attributes first, and may be immutable: the flag is
// cleared on a file that exists.
func writeVariable(dir, name string, value []byte) error {
	if err := deleteVariable(dir, name); err != nil {
		return err
	}
	buf := binary.LittleEndian.AppendUint32(nil, varAttrs)
	buf = append(buf, value...)
	f, err := os.OpenFile(varPath(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// deleteVariable removes the variable name, and is not an error when there is none.
func deleteVariable(dir, name string) error {
	p := varPath(dir, name)
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := setImmutable(p, false); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// readVariable returns the value of the variable name without its attributes, or an error that
// is os.ErrNotExist when there is none.
func readVariable(dir, name string) ([]byte, error) {
	b, err := os.ReadFile(varPath(dir, name))
	if err != nil {
		return nil, err
	}
	if len(b) < 4 {
		return nil, errors.New(name + " is shorter than its attributes")
	}
	return b[4:], nil
}

// readUint16 reads a variable of two bytes, BootCurrent or BootNext.
func readUint16(dir, name string) (uint16, error) {
	b, err := readVariable(dir, name)
	if err != nil {
		return 0, err
	}
	if len(b) != 2 {
		return 0, fmt.Errorf("%s is %d bytes, not 2", name, len(b))
	}
	return binary.LittleEndian.Uint16(b), nil
}
