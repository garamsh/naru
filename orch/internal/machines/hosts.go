package machines

import (
	"context"

	"github.com/garamsh/naru/lib/hostapi"
)

// Hosts is the host API as the reconciler needs it, and the guest calls the Machine API makes
// through it. Every call but List names an admitted host by its ID; a microVM or disk snapshot that
// is already gone answers nil to Delete, DeleteDiskSnapshot and Stop.
type Hosts interface {
	Guests
	// List returns every admitted host, with what each that answers reports.
	List(ctx context.Context) ([]Host, error)
	// Create makes the microVM and starts it, and returns it as it then is.
	Create(ctx context.Context, host string, r hostapi.CreateRequest) (hostapi.MicroVM, error)
	Start(ctx context.Context, host, microVM string) (hostapi.MicroVM, error)
	Stop(ctx context.Context, host, microVM string, graceSeconds uint32) (hostapi.MicroVM, error)
	// Delete stops and deletes the microVM and its disks.
	Delete(ctx context.Context, host, microVM string) error
	// SnapshotDisk copies a disk of the microVM into the disk snapshot name; the same name of the
	// same disk again is the snapshot taken.
	SnapshotDisk(ctx context.Context, host, microVM, disk, name string) error
	DeleteDiskSnapshot(ctx context.Context, host, name string) error
	// ExportDisk asks the host for a grant to its disk, <microVM>.<volume>, that the host target
	// fetches once.
	ExportDisk(ctx context.Context, host, disk, target string) (hostapi.Export, error)
	// ImportDisk has the host fetch a disk from another host's export and make it.
	ImportDisk(ctx context.Context, host string, r hostapi.ImportRequest) (hostapi.Imported, error)
}

// Host is an admitted host as the reconciler sees it in one pass.
type Host struct {
	ID     string
	Region string
	// Labels are the operator's; Reported the host's own facts (ADR 0110 item 3).
	Labels   map[string]string
	Reported map[string]string
	Draining bool
	// Answering is whether the host answered this pass. What follows is empty when it did not.
	Answering bool
	// Stale is an answer that is the host's last one, given again while the probe of this pass
	// failed: a microVM made since is missing from it.
	Stale         bool
	Capacity      hostapi.Resources
	MicroVMs      []hostapi.MicroVM
	DiskSnapshots []hostapi.DiskSnapshot
}

// microVM returns the host's microVM name, if it reports one.
func (h Host) microVM(name string) (hostapi.MicroVM, bool) {
	for _, m := range h.MicroVMs {
		if m.Name == name {
			return m, true
		}
	}
	return hostapi.MicroVM{}, false
}
