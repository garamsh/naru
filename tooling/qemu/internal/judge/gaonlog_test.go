package judge_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
)

// topo is the topology of the gaon Node as naructl prints it.
var topo = naructl.Topology{
	Services: []naructl.Service{
		{Name: "registry"}, {Name: "etcd", ControlPlane: true},
		{Name: "gaon-apiserver", ControlPlane: true, Gaon: true}, {Name: "gaon-scheduler", ControlPlane: true, Gaon: true},
		{Name: "gaon-controllers", ControlPlane: true, Gaon: true}, {Name: "gaon-node-agent", Gaon: true},
	},
	Signer: "gaon-controllers", APIServer: "gaon-apiserver", Agent: "gaon-node-agent",
}

// workerLog is the console of node-2 of a QEMU cluster running gaon, as captured: init logs
// nothing of the data disk's mount, and the stand-in, which logs it, is not in the Node's services.
const workerLog = `time=2026-11-12T02:27:07.000Z level=INFO msg="configuration accepted" node=naru-node-2 services=7
time=2026-11-12T02:27:07.041Z level=INFO msg="network configured" interfaces=1 nameservers=1
time=2026-11-12T02:27:07.048Z level=INFO msg="root remounted read-only"
time=2026-11-12T02:27:07.050Z level=INFO msg="skipping a control-plane service on a Node that is not one" service=etcd
time=2026-11-12T02:27:07.051Z level=INFO msg="skipping a control-plane service on a Node that is not one" service=gaon-apiserver
time=2026-11-12T02:27:07.052Z level=INFO msg="skipping a control-plane service on a Node that is not one" service=gaon-scheduler
time=2026-11-12T02:27:07.054Z level=INFO msg="skipping a control-plane service on a Node that is not one" service=gaon-controllers
time=2026-11-12T02:27:07.052Z level=INFO msg=started service=mvmd cgroup=/sys/fs/cgroup/naru/mvmd
time=2026-11-12T02:27:07.056Z level=INFO msg=started service=registry cgroup=/sys/fs/cgroup/naru/registry
time=2026-11-12T02:27:07.060Z level=INFO msg=started service=gaon-node-agent cgroup=/sys/fs/cgroup/naru/gaon-node-agent
`

func TestGaon_ACapturedWorkerConsoleMeetsEveryExpectation(t *testing.T) {
	assert.Empty(t, judge.Check("node-2", judge.Log{Text: workerLog}, judge.Gaon(topo, "node-2", false, 1)))
}

func TestBoot_TheStandinsDataDiskLineIsExpectedOfNodesThatRunTheStandin(t *testing.T) {
	bad := judge.Check("node-1", judge.Log{Text: workerLog}, judge.Boot("node-2"))

	whats := []string{}
	for _, f := range bad {
		whats = append(whats, f.Expect.What)
	}
	assert.Contains(t, whats, "the data disk is mounted")
}

func TestNodeAPI_AConsoleMeetsTheExpectationsOfTheStateItShows(t *testing.T) {
	off := judge.Log{Text: "level=INFO msg=\"the Node API is off: the configuration has no spec.api\"\n"}
	on := judge.Log{Text: "level=INFO msg=\"the Node API is listening\" addr=[::]:50000\n"}

	assert.Empty(t, judge.Check("node-1", off, judge.NodeAPI(false)))
	assert.Empty(t, judge.Check("node-1", on, judge.NodeAPI(true)))
	assert.Len(t, judge.Check("node-1", on, judge.NodeAPI(false)), 2, "a serving Node is not one with the API off")
	assert.Len(t, judge.Check("node-1", off, judge.NodeAPI(true)), 2)
}
