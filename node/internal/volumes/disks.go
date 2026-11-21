package volumes

// Disk is a whole disk of the Node.
type Disk struct {
	// Path is its device node and Bytes its size.
	Path  string
	Bytes int64
}

// Disks finds a disk by its serial number; an absent disk is an error.
type Disks interface {
	Find(serial string) (Disk, error)
	// InUse returns an error naming what uses the disk, or nil when nothing does: a mount of the
	// disk or of one of its partitions, or a device mapped over it.
	InUse(Disk) error
}
