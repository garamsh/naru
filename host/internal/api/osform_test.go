package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

const userData = "#cloud-config\nhostname: web\n"

func osBody() map[string]any {
	return map[string]any{
		"name": "web", "cpu": 2, "memory": 1 << 28, "bootSet": "naru", "cmdline": "naru.role=worker",
		"seed": []byte(userData), "dataDisk": 1 << 30, "podPrefix": "10.200.1.0/24",
	}
}

func TestOSForm_CreatesTheDataDiskThenTheMachineThenTheSeedAttachedBeforeTheStart(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, b := e.do(t, c, "POST", "/v1/microvms", osBody())
	require.Equal(t, 201, code, string(b))

	assert.Equal(t, []string{
		"createRawDisk web.data", "createMachine web", "createRawDisk web.seed", "attachDisk web.seed", "start web",
	}, e.rt.calls)
	assert.Empty(t, e.rt.pulled, "an OS form has no image to pull")
	assert.Empty(t, e.rt.contents["web.data"], "the data disk is blank, which init formats; mvmd's own file system would leave it no label and no verity")

	m := e.rt.machines["web"]
	assert.Equal(t, "naru", m.BootSet)
	assert.Equal(t, "naru.role=worker", m.Cmdline)
	assert.Equal(t, []string{"10.200.1.0/24"}, m.RoutedPrefixes, "the Pod prefix reaches the Machine")
	assert.Empty(t, m.ImageDigest)
	assert.Zero(t, m.RootDiskBytes)
	assert.Equal(t, []mvmd.Attachment{{ID: "web.data"}, {ID: "web.seed", ReadOnly: true}}, m.Disks, "the seed is attached read-only")

	vol := e.rt.contents["web.seed"]
	require.NotEmpty(t, vol)
	assert.Zero(t, len(vol)%512)
	assert.Equal(t, "CD001", string(vol[16*2048+1:16*2048+6]))
	assert.Equal(t, "cidata          ", string(vol[16*2048+40:16*2048+56]), "the volume label")
	for _, want := range []string{
		userData, "instance-id: web", "version: 1", "name: eth0", "address: '10.1.0.5'", "netmask: '255.255.255.0'", "gateway: '10.1.0.1'",
	} {
		assert.True(t, bytes.Contains(vol, []byte(want)), "the seed has no %q", want)
	}

	var v hostapi.MicroVM
	require.NoError(t, json.Unmarshal(b, &v))
	assert.Equal(t, "running", v.State)
	assert.Equal(t, "naru", v.BootSet)
	assert.Equal(t, "10.200.1.0/24", v.PodPrefix)
	assert.Equal(t, uint64(1<<30), v.Disks[0].SizeBytes)
	assert.True(t, v.Disks[1].ReadOnly)
}

func TestOSForm_AFailureAtEachStepRemovesOnlyWhatThisRequestMade(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name string
		prep func(*env)
		// disks and machines are what stays; the request failed with code.
		disks    []string
		machines []string
		code     int
	}{
		{"the data disk", func(e *env) { e.rt.failAt["createRawDisk web.data"] = boom }, nil, nil, 502},
		{"the data disk of another request", func(e *env) { e.rt.disks["web.data"] = mvmdDisk("web.data") }, []string{"web.data"}, nil, 409},
		{"the Machine", func(e *env) { e.rt.failAt["createMachine web"] = boom }, nil, nil, 502},
		{"the seed disk", func(e *env) { e.rt.failAt["createRawDisk web.seed"] = boom }, nil, nil, 502},
		{"the seed disk of another request", func(e *env) { e.rt.disks["web.seed"] = mvmdDisk("web.seed") }, []string{"web.seed"}, nil, 409},
		{"the attachment", func(e *env) { e.rt.failAt["attachDisk web.seed"] = boom }, nil, nil, 502},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := start(t)
			e.rt.disks["other.data"] = mvmdDisk("other.data")
			e.rt.machines["other"] = mvmd.Machine{ID: "other", State: mvmd.StateRunning}
			tc.prep(e)
			code, _ := e.do(t, e.as(t, ProvisionerOrganization), "POST", "/v1/microvms", osBody())
			assert.Equal(t, tc.code, code)
			var disks, machines []string
			for id := range e.rt.disks {
				if id != "other.data" {
					disks = append(disks, id)
				}
			}
			for id := range e.rt.machines {
				if id != "other" {
					machines = append(machines, id)
				}
			}
			assert.ElementsMatch(t, tc.disks, disks)
			assert.ElementsMatch(t, tc.machines, machines)
			assert.Contains(t, e.rt.disks, "other.data", "a disk of another microVM was removed")
			assert.Contains(t, e.rt.machines, "other", "another microVM was removed")
			assert.NotContains(t, e.rt.calls, "start web", "the Machine started")
		})
	}
}

func TestOSForm_AFailedStartLeavesTheMachineAndARetryStartsIt(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	e.rt.failAt["start web"] = errors.New("boom")
	code, _ := e.do(t, c, "POST", "/v1/microvms", osBody())
	require.Equal(t, 502, code)
	assert.Contains(t, e.rt.machines, "web")
	assert.Contains(t, e.rt.disks, "web.seed")

	delete(e.rt.failAt, "start web")
	e.rt.calls = nil
	code, b := e.do(t, c, "POST", "/v1/microvms", osBody())
	require.Equal(t, 200, code, string(b))
	assert.Equal(t, []string{"start web"}, e.rt.calls, "the retry made nothing again")
	assert.Contains(t, string(b), `"state":"running"`)
}

func TestOSForm_IsIdempotentAndAnotherBodyConflicts(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, first := e.do(t, c, "POST", "/v1/microvms", osBody())
	require.Equal(t, 201, code)
	code, again := e.do(t, c, "POST", "/v1/microvms", osBody())
	require.Equal(t, 200, code)
	assert.JSONEq(t, string(first), string(again))
	assert.Len(t, e.rt.machines, 1)

	for field, value := range map[string]any{
		"podPrefix": "10.200.2.0/24", "bootSet": "other", "cmdline": "x", "dataDisk": 1 << 31, "cpu": 4,
	} {
		other := osBody()
		other[field] = value
		code, _ = e.do(t, c, "POST", "/v1/microvms", other)
		assert.Equal(t, 409, code, field)
	}
}

func TestOSForm_Validation(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	cases := map[string]func(map[string]any){
		"bootSet":   func(b map[string]any) { delete(b, "bootSet") },
		"seed":      func(b map[string]any) { delete(b, "seed") },
		"dataDisk":  func(b map[string]any) { delete(b, "dataDisk") },
		"cpu":       func(b map[string]any) { b["cpu"] = 0 },
		"name":      func(b map[string]any) { b["name"] = "Bad.Name" },
		"podPrefix": func(b map[string]any) { b["podPrefix"] = "10.200.1.1/24" },
		"cmdline":   func(b map[string]any) { b["cmdline"] = "a\nb" },
		"image":     func(b map[string]any) { b["image"] = "registry.example/app@" + digest },
		"disks":     func(b map[string]any) { b["disks"] = []map[string]any{{"name": "a", "sizeBytes": 1}} },
	}
	for field, mutate := range cases {
		t.Run(field, func(t *testing.T) {
			b := osBody()
			mutate(b)
			code, body := e.do(t, c, "POST", "/v1/microvms", b)
			assert.Equal(t, 422, code, string(body))
			assert.Contains(t, string(body), field)
		})
	}
	for _, prefix := range []string{"10.0.0.0/15", "fd00::/64"} {
		b := osBody()
		b["podPrefix"] = prefix
		code, _ := e.do(t, c, "POST", "/v1/microvms", b)
		assert.Equal(t, 422, code, "mvmd routes an IPv4 prefix of /16 to /32: "+prefix)
	}
	assert.Empty(t, e.rt.calls, "a refused request reached mvmd")

	// The control: the unmodified body is accepted.
	code, body := e.do(t, c, "POST", "/v1/microvms", osBody())
	assert.Equal(t, 201, code, string(body))
}

func TestOSForm_ASeedOverWhatMvmdTakesIsRefused(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	// The user-data alone is under 1 MiB; the volume that holds it is not.
	big := osBody()
	big["seed"] = bytes.Repeat([]byte("a"), maxSeedBytes-1024)
	code, body := e.do(t, c, "POST", "/v1/microvms", big)
	assert.Equal(t, 422, code, string(body))
	assert.Contains(t, string(body), "seed")
	assert.Empty(t, e.rt.calls)

	// The control: a seed well under the bound is accepted.
	small := osBody()
	small["seed"] = bytes.Repeat([]byte("a"), maxSeedBytes/2)
	code, body = e.do(t, c, "POST", "/v1/microvms", small)
	assert.Equal(t, 201, code, string(body))
}

func TestOSForm_AHostWithNoMachineGatewayRefusesIt(t *testing.T) {
	e := startWith(t, netip.Addr{})
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", osBody())
	assert.Equal(t, 501, code)
	assert.Empty(t, e.rt.calls)

	// The control: the image form needs no gateway.
	code, _ = e.do(t, c, "POST", "/v1/microvms", createBody())
	assert.Equal(t, 201, code)
}

func TestOSForm_DeleteRemovesTheMachineAndBothDisks(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", osBody())
	require.Equal(t, 201, code)
	code, _ = e.do(t, c, "DELETE", "/v1/microvms/web", nil)
	assert.Equal(t, 204, code)
	assert.Empty(t, e.rt.machines)
	assert.Empty(t, e.rt.disks)
}
