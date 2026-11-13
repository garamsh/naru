package netecho

import (
	"net"
	"syscall"
	"unsafe"
)

const (
	siocSIFADDR    = 0x8916
	siocSIFNETMASK = 0x891c
)

// ifreq is the kernel's struct ifreq for an address request.
type ifreq struct {
	name [syscall.IFNAMSIZ]byte
	addr syscall.RawSockaddrInet4
	_    [8]byte
}

// addLocal gives the loopback the address as a /32 under the alias label, the way ifconfig does.
func addLocal(label string, ip net.IP) error {
	v4 := ip.To4()
	if v4 == nil {
		return errNotIPv4
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	for _, req := range []struct {
		code uintptr
		addr [4]byte
	}{{siocSIFADDR, [4]byte(v4)}, {siocSIFNETMASK, [4]byte{255, 255, 255, 255}}} {
		var r ifreq
		copy(r.name[:], label)
		r.addr = syscall.RawSockaddrInet4{Family: syscall.AF_INET, Addr: req.addr}
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req.code, uintptr(unsafe.Pointer(&r))); errno != 0 {
			return errno
		}
	}
	return nil
}
