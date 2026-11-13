package node

import (
	"fmt"
	"os"

	"github.com/garamsh/naru/lib/iso9660"
)

// Label and size of the NoCloud drive: the volume Proxmox writes into 4 MiB with genisoimage and
// qemu-img dd.
const (
	CidataLabel = "cidata"

	cidataBytes = 4 << 20
)

// BuildCidata writes the NoCloud drive of a Node to path: a 4 MiB raw file whose ISO 9660 volume,
// labelled CidataLabel, holds user-data (the Node's naru.yaml), network-config and a meta-data
// that names the instance.
func BuildCidata(path, instance string, userData, networkConfig []byte) error {
	img, err := iso9660.Write(CidataLabel, []iso9660.File{
		{Name: "user-data", Data: userData},
		{Name: "meta-data", Data: []byte("instance-id: " + instance + "\n")},
		{Name: "network-config", Data: networkConfig},
	})
	if err != nil {
		return err
	}
	if len(img) > cidataBytes {
		return fmt.Errorf("the NoCloud volume is %d bytes, more than the %d of a Proxmox cloud-init drive", len(img), cidataBytes)
	}
	disk := make([]byte, cidataBytes)
	copy(disk, img)
	return os.WriteFile(path, disk, 0o644)
}
