package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

// importedBody is createBody with its disk named imported and asked not started, as a move's
// target is created.
func importedBody() map[string]any {
	b := createBody()
	b["disks"] = []map[string]any{{"name": "data", "sizeBytes": 1 << 20, "mountPath": "/data", "imported": true}}
	b["noStart"] = true
	return b
}

func (e *env) importDisk(id string, size uint64) {
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	e.rt.disks[id] = mvmd.Disk{ID: id, SizeBytes: size}
}

func TestCreate_AnImportedDiskIsTakenAsItIsAndNoneIsMade(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	e.importDisk("web.data", 1<<20)

	code, body := e.do(t, p, "POST", "/v1/microvms", importedBody())
	require.Equal(t, http.StatusCreated, code, string(body))
	assert.NotContains(t, e.rt.calls, "createDisk web.data", "no disk is made over the imported one")
	assert.Equal(t, []mvmd.Attachment{{ID: "web.data", MountPath: "/data"}}, e.rt.machines["web"].Disks)
	assert.Equal(t, mvmd.StateCreated, e.rt.machines["web"].State)

	code, body = e.do(t, p, "POST", "/v1/microvms", importedBody())
	assert.Equal(t, http.StatusOK, code, "a repeat is the microVM made: %s", body)
}

func TestCreate_AnImportedDiskTheHostDoesNotHoldAsNamedIs409(t *testing.T) {
	for name, have := range map[string]*mvmd.Disk{
		"absent":         nil,
		"another size":   {ID: "web.data", SizeBytes: 1 << 21},
		"held elsewhere": {ID: "web.data", SizeBytes: 1 << 20, Holder: "other"},
	} {
		t.Run(name, func(t *testing.T) {
			e := start(t)
			p := e.as(t, ProvisionerOrganization)
			if have != nil {
				e.rt.disks[have.ID] = *have
			}
			code, body := e.do(t, p, "POST", "/v1/microvms", importedBody())
			assert.Equal(t, http.StatusConflict, code, string(body))
			assert.NotContains(t, e.rt.calls, "createMachine web")
		})
	}
}

func TestCreate_ADiskBothImportedAndFromASnapshotIs422(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	b := importedBody()
	b["disks"] = []map[string]any{{"name": "data", "sizeBytes": 1 << 20, "imported": true, "fromSnapshot": "s1"}}
	code, body := e.do(t, p, "POST", "/v1/microvms", b)
	require.Equal(t, http.StatusUnprocessableEntity, code, string(body))
	assert.Equal(t, []string{"disks[0]"}, codeOf(t, body).Fields)
}

func TestCreate_AFailedCreateLeavesTheImportedDisk(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	e.importDisk("web.data", 1<<20)
	e.rt.failAt["createMachine web"] = errors.New("mvmd failed")

	code, _ := e.do(t, p, "POST", "/v1/microvms", importedBody())
	assert.Equal(t, http.StatusBadGateway, code)
	assert.Contains(t, e.rt.disks, "web.data", "the import's disk is not the create's to remove")
}

func TestCreate_ACreateThatCannotAttachLeavesTheImportedDiskAndRemovesWhatItMade(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, p, "PUT", "/v1/networks/far", hostapi.NetworkRequest{Prefix: "10.9.0.0/16"})
	require.Equal(t, http.StatusOK, code)
	e.importDisk("web.data", 1<<20)
	b := importedBody()
	b["network"] = "far"
	b["disks"] = []map[string]any{{"name": "data", "sizeBytes": 1 << 20, "imported": true}, {"name": "scratch", "sizeBytes": 1 << 20}}
	e.nft.hook = func(context.Context) error { return errors.New("nft failed") }

	code, body := e.do(t, p, "POST", "/v1/microvms", b)
	require.GreaterOrEqual(t, code, 400, string(body))
	assert.NotContains(t, e.rt.machines, "web")
	assert.Contains(t, e.rt.disks, "web.data", "the import's disk is kept")
	assert.NotContains(t, e.rt.disks, "web.scratch", "the disk the create made is removed")
}
