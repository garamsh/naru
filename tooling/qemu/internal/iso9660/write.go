// Package iso9660 writes a small ISO 9660 volume with a Joliet tree: one flat
// root directory of files, which is what a NoCloud drive is. It exists because
// the workstation has no genisoimage, mkisofs or xorriso and a test must not
// need root to make a volume. Proxmox builds its cloud-init volume with
// genisoimage -iso-level 3 -J -R -V cidata; the kernel reads the long file
// names of this volume from the Joliet tree.
package iso9660

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
)

const (
	sector = 2048
	// The layout: the system area, the primary and the Joliet descriptor, the terminator, the
	// two path tables of each tree, the two root directories, then the files.
	pvdSector        = 16
	svdSector        = 17
	terminatorSector = 18
	primaryLPath     = 19
	primaryMPath     = 20
	jolietLPath      = 21
	jolietMPath      = 22
	primaryRoot      = 23
	jolietRoot       = 24
	firstFile        = 25
	pathTableSize    = 10
)

// File is a file of the root directory.
type File struct {
	Name string
	Data []byte
}

// Write returns the volume labelled label that holds files. The label is at
// most 16 characters, and every name at most 64 characters, so that both fit
// the Joliet limits and the one-sector directories.
func Write(label string, files []File) ([]byte, error) {
	if label == "" || len(label) > 16 || strings.Trim(label, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" {
		return nil, fmt.Errorf("label %q is not 1 to 16 letters, digits, '_' or '-'", label)
	}
	files = append([]File(nil), files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	extents := make([]int, len(files))
	next := firstFile
	for i, f := range files {
		if f.Name == "" || len(f.Name) > 64 || strings.ContainsAny(f.Name, "/\\;:*?\"<>|\x00") {
			return nil, fmt.Errorf("file name %q is not a plain name of at most 64 characters", f.Name)
		}
		if i > 0 && files[i-1].Name == f.Name {
			return nil, fmt.Errorf("file name %q is used twice", f.Name)
		}
		extents[i] = next
		next += (len(f.Data) + sector - 1) / sector
	}
	primary := directory(primaryRoot, files, extents, isoName)
	joliet := directory(jolietRoot, files, extents, jolietName)
	if len(primary) > sector || len(joliet) > sector {
		return nil, errors.New("too many files for a one-sector directory")
	}
	if names := isoNames(files); len(names) != len(files) {
		return nil, errors.New("two names are the same in 8.3 form")
	}

	img := make([]byte, next*sector)
	put := func(at int, b []byte) { copy(img[at*sector:], b) }
	put(pvdSector, descriptor(1, label, next, primaryRoot, primaryLPath, primaryMPath, false))
	put(svdSector, descriptor(2, label, next, jolietRoot, jolietLPath, jolietMPath, true))
	put(terminatorSector, append([]byte{255}, append([]byte("CD001"), 1)...))
	put(primaryLPath, pathTable(primaryRoot, binary.LittleEndian))
	put(primaryMPath, pathTable(primaryRoot, binary.BigEndian))
	put(jolietLPath, pathTable(jolietRoot, binary.LittleEndian))
	put(jolietMPath, pathTable(jolietRoot, binary.BigEndian))
	put(primaryRoot, primary)
	put(jolietRoot, joliet)
	for i, f := range files {
		put(extents[i], f.Data)
	}
	return img, nil
}

// both is the both-endian form of a 32-bit number.
func both(v uint32) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
	return b
}

func both16(v uint16) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
	return b
}

// recordTime is the fixed time of every record, so the same files give the same volume.
var recordTime = []byte{100, 1, 1, 0, 0, 0, 0}

// record is a directory record.
func record(extent, size int, dir bool, id []byte) []byte {
	n := 33 + len(id)
	if n%2 == 1 {
		n++
	}
	r := make([]byte, n)
	r[0] = byte(n)
	copy(r[2:], both(uint32(extent)))
	copy(r[10:], both(uint32(size)))
	copy(r[18:], recordTime)
	if dir {
		r[25] = 2
	}
	copy(r[28:], both16(1))
	r[32] = byte(len(id))
	copy(r[33:], id)
	return r
}

// directory is the one sector of the root directory at extent, listing files by the names name gives.
func directory(extent int, files []File, extents []int, name func(string) []byte) []byte {
	out := record(extent, sector, true, []byte{0})
	out = append(out, record(extent, sector, true, []byte{1})...)
	for _, i := range byDirectoryOrder(files, name) {
		out = append(out, record(extents[i], len(files[i].Data), false, name(files[i].Name))...)
	}
	return out
}

// byDirectoryOrder is the order of the files by the bytes of the identifiers name gives.
func byDirectoryOrder(files []File, name func(string) []byte) []int {
	idx := make([]int, len(files))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return bytes.Compare(name(files[idx[a]].Name), name(files[idx[b]].Name)) < 0
	})
	return idx
}

// isoName is the name in the primary tree: upper case, '_' for anything outside A-Z, 0-9 and '_',
// and ";1", which plain ISO 9660 requires.
func isoName(n string) []byte {
	base, ext := n, ""
	if i := strings.LastIndexByte(n, '.'); i >= 0 {
		base, ext = n[:i], n[i+1:]
	}
	clean := func(s string, max int) string {
		out := []byte(strings.ToUpper(s))
		if len(out) > max {
			out = out[:max]
		}
		for i, c := range out {
			if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
				out[i] = '_'
			}
		}
		return string(out)
	}
	return []byte(clean(base, 8) + "." + clean(ext, 3) + ";1")
}

func isoNames(files []File) map[string]bool {
	m := map[string]bool{}
	for _, f := range files {
		m[string(isoName(f.Name))] = true
	}
	return m
}

// jolietName is the name in UCS-2, big-endian.
func jolietName(n string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(n)) {
		b = binary.BigEndian.AppendUint16(b, u)
	}
	return b
}

// pathTable is the path table of a tree that has only its root.
func pathTable(rootExtent int, order binary.ByteOrder) []byte {
	t := make([]byte, pathTableSize)
	t[0] = 1
	order.PutUint32(t[2:], uint32(rootExtent))
	order.PutUint16(t[6:], 1)
	return t
}

// descriptor is the primary (typ 1) or the Joliet (typ 2) volume descriptor.
func descriptor(typ byte, label string, sectors, root, lPath, mPath int, joliet bool) []byte {
	d := make([]byte, sector)
	text := func(at, n int, s string) {
		field := d[at : at+n]
		if joliet {
			for i := 0; i+1 < len(field); i += 2 {
				binary.BigEndian.PutUint16(field[i:], ' ')
			}
			copy(field, jolietName(s))
			return
		}
		copy(field, bytes.Repeat([]byte{' '}, n))
		copy(field, s)
	}
	d[0] = typ
	copy(d[1:], "CD001")
	d[6] = 1
	text(8, 32, "")
	text(40, 32, label)
	copy(d[80:], both(uint32(sectors)))
	if joliet {
		copy(d[88:], "%/E")
	}
	copy(d[120:], both16(1))
	copy(d[124:], both16(1))
	copy(d[128:], both16(sector))
	copy(d[132:], both(pathTableSize))
	binary.LittleEndian.PutUint32(d[140:], uint32(lPath))
	binary.BigEndian.PutUint32(d[148:], uint32(mPath))
	copy(d[156:], record(root, sector, true, []byte{0}))
	for _, f := range [][2]int{{190, 128}, {318, 128}, {446, 128}, {574, 128}, {702, 37}, {739, 37}, {776, 37}} {
		text(f[0], f[1], "")
	}
	for _, at := range []int{813, 830} {
		copy(d[at:], "2000010100000000")
		d[at+16] = 0
	}
	for _, at := range []int{847, 864} {
		copy(d[at:], "0000000000000000")
		d[at+16] = 0
	}
	d[881] = 1
	return d
}
