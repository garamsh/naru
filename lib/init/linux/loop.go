package linux

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	loopControlPath = "/dev/loop-control"
	loopDevFormat   = "/dev/loop%d"

	// The ioctls of linux/loop.h.
	loopCtlGetFree = 0x4C82
	loopConfigure  = 0x4C0A

	loFlagsReadOnly  = 1
	loFlagsAutoclear = 4

	// loopAttempts is how many times Mount asks for a free device and configures it, since another
	// caller can take the one it was given.
	loopAttempts = 5
	// loopNodeWait is how long Mount waits for devtmpfs to make the node of a new device.
	loopNodeWait = 2 * time.Second
)

// loopInfo64 is struct loop_info64 and loopConfig is struct loop_config, both of linux/loop.h.
type loopInfo64 struct {
	device, inode, rdevice, offset, sizeLimit uint64
	number, encryptType, encryptKeySize       uint32
	flags                                     uint32
	fileName, cryptName                       [64]byte
	encryptKey                                [32]byte
	init                                      [2]uint64
}

type loopConfig struct {
	fd        uint32
	blockSize uint32
	info      loopInfo64
	reserved  [8]uint64
}

// Loop mounts squashfs images read-only through loop devices.
type Loop struct{}

// Mount attaches image to a free loop device, read-only and to be released with its last
// reference, and mounts it on dir as squashfs, read-only, with no set-user-ID bit and no device
// nodes honoured. Programs in it can run, which is what an extension is for.
func (Loop) Mount(image *os.File, dir string) error {
	dev, err := attach(image)
	if err != nil {
		return err
	}
	if err := syscall.Mount(dev.Name(), dir, "squashfs", syscall.MS_RDONLY|nosuidNodev, ""); err != nil {
		_ = dev.Close() // autoclear detaches the device
		return fmt.Errorf("mount %s on %s: %w", dev.Name(), dir, err)
	}
	// The mount holds the device now; autoclear detaches it when the file system is unmounted.
	return dev.Close()
}

// Unmount unmounts dir. The loop device behind it detaches with it.
func (Loop) Unmount(dir string) error {
	return syscall.Unmount(dir, 0)
}

// attach configures a free loop device for image and returns it open.
func attach(image *os.File) (*os.File, error) {
	ctl, err := os.OpenFile(loopControlPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", loopControlPath, err)
	}
	defer ctl.Close()
	var last error
	for range loopAttempts {
		n, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ctl.Fd(), loopCtlGetFree, 0)
		if errno != 0 {
			return nil, fmt.Errorf("get a free loop device: %w", errno)
		}
		dev, err := openLoop(fmt.Sprintf(loopDevFormat, n))
		if err != nil {
			return nil, err
		}
		cfg := loopConfig{fd: uint32(image.Fd())}
		cfg.info.flags = loFlagsReadOnly | loFlagsAutoclear
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, dev.Fd(), loopConfigure, uintptr(unsafe.Pointer(&cfg)))
		if errno == 0 {
			return dev, nil
		}
		_ = dev.Close()
		last = fmt.Errorf("attach %s: %w", dev.Name(), errno)
		if errno != syscall.EBUSY {
			break
		}
	}
	return nil, last
}

// openLoop opens the node of a loop device, waiting for devtmpfs to make it.
func openLoop(path string) (*os.File, error) {
	deadline := time.Now().Add(loopNodeWait)
	for {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err == nil || !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			return f, err
		}
		time.Sleep(diskPollInterval)
	}
}
