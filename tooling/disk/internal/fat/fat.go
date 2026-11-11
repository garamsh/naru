// Package fat adds files to a FAT32 volume that mkfs.fat has already made. It exists because the
// workstation has neither root, to mount the volume, nor mtools, and because every byte it writes
// is a function of its arguments: the directory entries carry the time it is given and nothing
// else. It supports what an EFI system partition of naru needs: 8.3 names in upper case, and a
// directory that fits one cluster.
package fat

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	dirEntryBytes = 32
	attrDirectory = 0x10
	attrArchive   = 0x20
	endOfChain    = 0x0FFFFFFF
	// entryMask drops the four reserved high bits of a FAT32 entry.
	entryMask = 0x0FFFFFFF
)

// Volume is a FAT32 volume in a file, open for adding files.
type Volume struct {
	f                 *os.File
	sectorBytes       int64
	sectorsPerCluster int64
	reserved          int64
	copies            int64
	fatSectors        int64
	rootCluster       uint32
	fsInfoSector      int64
	clusters          uint32
	dataStart         int64
	table             []uint32
	date, clock       uint16
}

// Open opens the volume in the file at path. Every entry Add writes is stamped with stamp, in UTC.
func Open(path string, stamp time.Time) (*Volume, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	v, err := newVolume(f, stamp.UTC())
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

func newVolume(f *os.File, stamp time.Time) (*Volume, error) {
	if stamp.Year() < 1980 || stamp.Year() > 2107 {
		return nil, fmt.Errorf("%d is outside the years a FAT entry holds", stamp.Year())
	}
	boot := make([]byte, 512)
	if _, err := f.ReadAt(boot, 0); err != nil {
		return nil, err
	}
	if boot[510] != 0x55 || boot[511] != 0xAA {
		return nil, errors.New("no boot sector signature")
	}
	v := &Volume{
		f:                 f,
		sectorBytes:       int64(binary.LittleEndian.Uint16(boot[11:])),
		sectorsPerCluster: int64(boot[13]),
		reserved:          int64(binary.LittleEndian.Uint16(boot[14:])),
		copies:            int64(boot[16]),
		fatSectors:        int64(binary.LittleEndian.Uint32(boot[36:])),
		rootCluster:       binary.LittleEndian.Uint32(boot[44:]),
		fsInfoSector:      int64(binary.LittleEndian.Uint16(boot[48:])),
	}
	if binary.LittleEndian.Uint16(boot[17:]) != 0 || binary.LittleEndian.Uint16(boot[22:]) != 0 || v.fatSectors == 0 {
		return nil, errors.New("not a FAT32 volume")
	}
	total := int64(binary.LittleEndian.Uint32(boot[32:]))
	v.dataStart = (v.reserved + v.copies*v.fatSectors) * v.sectorBytes
	v.clusters = uint32((total-v.reserved-v.copies*v.fatSectors)/v.sectorsPerCluster) + 2
	if v.clusters < 65525 {
		return nil, fmt.Errorf("%d clusters is a FAT16 or FAT12 volume", v.clusters)
	}
	raw := make([]byte, 4*int64(v.clusters))
	if _, err := f.ReadAt(raw, v.reserved*v.sectorBytes); err != nil {
		return nil, err
	}
	v.table = make([]uint32, v.clusters)
	for i := range v.table {
		v.table[i] = binary.LittleEndian.Uint32(raw[4*i:])
	}
	v.date = uint16(stamp.Year()-1980)<<9 | uint16(stamp.Month())<<5 | uint16(stamp.Day())
	v.clock = uint16(stamp.Hour())<<11 | uint16(stamp.Minute())<<5 | uint16(stamp.Second()/2)
	return v, nil
}

// Add writes size bytes from content as the file at path, such as "EFI/BOOT/BOOTX64.EFI", creating
// the directories on the way. It fails when the file exists.
func (v *Volume) Add(path string, content io.Reader, size int64) error {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	dir := v.rootCluster
	for _, name := range parts[:len(parts)-1] {
		next, err := v.directory(dir, name)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		dir = next
	}
	short, err := shortName(parts[len(parts)-1])
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	_, _, found, err := v.find(dir, short)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if found {
		return fmt.Errorf("%s: exists", path)
	}
	first, err := v.store(content, size)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := v.insert(dir, short, attrArchive, first, uint32(size)); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// find returns the first cluster and the size of the entry short in the directory dir.
func (v *Volume) find(dir uint32, short string) (first, size uint32, found bool, err error) {
	buf, err := v.readCluster(dir)
	if err != nil {
		return 0, 0, false, err
	}
	for off := 0; off < len(buf); off += dirEntryBytes {
		if string(buf[off:off+11]) == short {
			first = uint32(binary.LittleEndian.Uint16(buf[off+20:]))<<16 | uint32(binary.LittleEndian.Uint16(buf[off+26:]))
			return first, binary.LittleEndian.Uint32(buf[off+28:]), true, nil
		}
	}
	return 0, 0, false, nil
}

// ReadFile returns the contents of the file at path, following its chain of clusters.
func (v *Volume) ReadFile(path string) ([]byte, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	dir := v.rootCluster
	for i, name := range parts {
		short, err := shortName(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		first, size, found, err := v.find(dir, short)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if !found {
			return nil, fmt.Errorf("%s: not found", path)
		}
		if i < len(parts)-1 {
			dir = first
			continue
		}
		return v.chain(first, size)
	}
	return nil, fmt.Errorf("%s: no name", path)
}

func (v *Volume) chain(first, size uint32) ([]byte, error) {
	clusterBytes := v.sectorBytes * v.sectorsPerCluster
	out := make([]byte, 0, size)
	for c := first; len(out) < int(size); c = v.table[c] & entryMask {
		if c < 2 || c >= v.clusters {
			return nil, fmt.Errorf("the chain leaves the volume at cluster %d", c)
		}
		b, err := v.readCluster(c)
		if err != nil {
			return nil, err
		}
		out = append(out, b[:min(clusterBytes, int64(size)-int64(len(out)))]...)
	}
	return out, nil
}

// directory returns the cluster of the directory name in dir, creating it when absent.
func (v *Volume) directory(dir uint32, name string) (uint32, error) {
	short, err := shortName(name)
	if err != nil {
		return 0, err
	}
	if c, _, found, err := v.find(dir, short); err != nil || found {
		return c, err
	}
	c, err := v.allocate()
	if err != nil {
		return 0, err
	}
	table := make([]byte, v.sectorBytes*v.sectorsPerCluster)
	parent := dir
	if parent == v.rootCluster {
		parent = 0
	}
	v.fill(table, ".          ", attrDirectory, c, 0)
	v.fill(table[dirEntryBytes:], "..         ", attrDirectory, parent, 0)
	if err := v.writeCluster(c, table); err != nil {
		return 0, err
	}
	return c, v.insert(dir, short, attrDirectory, c, 0)
}

// insert writes the entry for short into the first free slot of the directory in cluster dir.
func (v *Volume) insert(dir uint32, short string, attr byte, first, size uint32) error {
	buf, err := v.readCluster(dir)
	if err != nil {
		return err
	}
	for off := 0; off < len(buf); off += dirEntryBytes {
		if buf[off] == 0x00 || buf[off] == 0xE5 {
			v.fill(buf[off:], short, attr, first, size)
			return v.writeCluster(dir, buf)
		}
	}
	return errors.New("the directory has no free entry in its one cluster")
}

func (v *Volume) fill(b []byte, short string, attr byte, first, size uint32) {
	clear(b[:dirEntryBytes])
	copy(b, short)
	b[11] = attr
	binary.LittleEndian.PutUint16(b[14:], v.clock)
	binary.LittleEndian.PutUint16(b[16:], v.date)
	binary.LittleEndian.PutUint16(b[18:], v.date)
	binary.LittleEndian.PutUint16(b[20:], uint16(first>>16))
	binary.LittleEndian.PutUint16(b[22:], v.clock)
	binary.LittleEndian.PutUint16(b[24:], v.date)
	binary.LittleEndian.PutUint16(b[26:], uint16(first))
	binary.LittleEndian.PutUint32(b[28:], size)
}

// store writes size bytes of content into a chain of free clusters and returns the first.
func (v *Volume) store(content io.Reader, size int64) (uint32, error) {
	clusterBytes := v.sectorBytes * v.sectorsPerCluster
	buf := make([]byte, clusterBytes)
	var first, prev uint32
	for left := size; left > 0; left -= clusterBytes {
		clear(buf)
		if _, err := io.ReadFull(content, buf[:min(left, clusterBytes)]); err != nil {
			return 0, err
		}
		c, err := v.allocate()
		if err != nil {
			return 0, err
		}
		if first == 0 {
			first = c
		} else {
			v.table[prev] = c
		}
		prev = c
		if err := v.writeCluster(c, buf); err != nil {
			return 0, err
		}
	}
	return first, nil
}

// allocate marks the lowest free cluster the end of a chain and returns it.
func (v *Volume) allocate() (uint32, error) {
	for c := uint32(2); c < v.clusters; c++ {
		if v.table[c]&entryMask == 0 {
			v.table[c] = endOfChain
			return c, nil
		}
	}
	return 0, errors.New("the volume is full")
}

func (v *Volume) offset(cluster uint32) int64 {
	return v.dataStart + int64(cluster-2)*v.sectorBytes*v.sectorsPerCluster
}

func (v *Volume) readCluster(c uint32) ([]byte, error) {
	b := make([]byte, v.sectorBytes*v.sectorsPerCluster)
	_, err := v.f.ReadAt(b, v.offset(c))
	return b, err
}

func (v *Volume) writeCluster(c uint32, b []byte) error {
	_, err := v.f.WriteAt(b, v.offset(c))
	return err
}

// Close writes the allocation table to every copy, sets the free cluster count of the FSInfo
// sector, and closes the file.
func (v *Volume) Close() error {
	raw := make([]byte, 4*len(v.table))
	free := uint32(0)
	for i, e := range v.table {
		binary.LittleEndian.PutUint32(raw[4*i:], e)
		if i >= 2 && e&entryMask == 0 {
			free++
		}
	}
	return errors.Join(v.flush(raw, free), v.f.Close())
}

func (v *Volume) flush(raw []byte, free uint32) error {
	for i := range v.copies {
		if _, err := v.f.WriteAt(raw, (v.reserved+i*v.fatSectors)*v.sectorBytes); err != nil {
			return err
		}
	}
	info := make([]byte, 8)
	binary.LittleEndian.PutUint32(info[0:], free)
	binary.LittleEndian.PutUint32(info[4:], 0xFFFFFFFF)
	_, err := v.f.WriteAt(info, v.fsInfoSector*v.sectorBytes+488)
	return err
}

// shortName returns name as the 11 bytes of an 8.3 directory entry.
func shortName(name string) (string, error) {
	base, ext, _ := strings.Cut(name, ".")
	if base == "" || len(base) > 8 || len(ext) > 3 || name != strings.ToUpper(name) || strings.ContainsAny(name, " \\/") {
		return "", fmt.Errorf("%q is not an upper-case 8.3 name", name)
	}
	return fmt.Sprintf("%-8s%-3s", base, ext), nil
}
