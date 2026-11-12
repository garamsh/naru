package efivar

import (
	"os"
	"syscall"
	"unsafe"
)

// The ioctls and the flag of chattr's immutable attribute, from <linux/fs.h>.
const (
	fsIocGetFlags = 0x80086601
	fsIocSetFlags = 0x40086602
	fsImmutableFl = 0x10
)

// setImmutable sets or clears the immutable flag of the file, which efivarfs sets on a variable
// that firmware protects from deletion.
func setImmutable(path string, on bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var flags uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocGetFlags, uintptr(unsafe.Pointer(&flags))); e != 0 {
		return e
	}
	if on {
		flags |= fsImmutableFl
	} else {
		flags &^= fsImmutableFl
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocSetFlags, uintptr(unsafe.Pointer(&flags))); e != 0 {
		return e
	}
	return nil
}
