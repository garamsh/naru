package machines

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
)

const gib = 1 << 30

// fits is a spec of 2 CPUs, 4 GiB of memory and 30 GiB of disk.
func fits() machineapi.Spec {
	return machineapi.Spec{CPU: 2, MemoryBytes: 4 * gib, RootDiskBytes: 10 * gib, Volumes: []machineapi.Volume{{Name: "data", SizeBytes: 20 * gib}}}
}

// roomy is a candidate that answers, in kr-seoul, with the room left given.
func roomy(id string, cpu, memory, disk int64) candidate {
	return candidate{
		Host:        Host{ID: id, Region: "kr-seoul", Answering: true, Labels: map[string]string{"naru.garam.sh/site": "a"}, Reported: map[string]string{"naru.garam.sh/virtualization": "metal"}},
		allocatable: amounts{64, 256 * gib, 4096 * gib},
		left:        amounts{cpu, memory, disk},
	}
}

func TestAccount_TakesTheReserveTheAllocationTheDiskSnapshotsAndEveryMicroVMNotBound(t *testing.T) {
	h := Host{
		Capacity:      hostapi.Resources{CPU: 32, Memory: 64 * gib, Disk: 1000 * gib},
		DiskSnapshots: []hostapi.DiskSnapshot{{Name: "s-1-0", SizeBytes: 50 * gib, AllocatedBytes: 7 * gib}},
		MicroVMs: []hostapi.MicroVM{
			{Name: "m-0123456789abcdef-1", CPU: 4, Memory: 8 * gib, RootDiskBytes: 10 * gib, Disks: []hostapi.DiskSpec{{Name: "data", SizeBytes: 20 * gib}}},
			{Name: "web-1", CPU: 2, Memory: 2 * gib, RootDiskBytes: 5 * gib, Disks: []hostapi.DiskSpec{{Name: "d", SizeBytes: 1 * gib}}},
		},
	}
	allocatable, left := account(h, Reserve{CPU: 1, Memory: 2 * gib}, hostapi.Resources{CPU: 4, Memory: 8 * gib, Disk: 30 * gib}, map[string]bool{"m-0123456789abcdef-1": true})

	assert.Equal(t, amounts{31, 62 * gib, 1000 * gib}, allocatable, "capacity less the reserve")
	assert.Equal(t, amounts{31 - 4 - 2, (62 - 8 - 2) * gib, (1000 - 30 - 7 - 6) * gib}, left,
		"less the allocation (the bound microVM through it, not again), the snapshot's allocated bytes and the group's member")
}

func TestPlace_NamesTheFirstConditionNoHostMeets(t *testing.T) {
	gpu := fits()
	gpu.Shape.GPU = &machineapi.GPU{Model: "h100", Count: 1}
	big := fits()
	big.CPU = 48
	hungry := fits()
	hungry.MemoryBytes = 300 * gib
	greedy := fits()
	greedy.RootDiskBytes = 5000 * gib
	elsewhere := fits()
	elsewhere.Shape.Region = "us-east"
	labelled := fits()
	labelled.Shape.Labels = map[string]string{"naru.garam.sh/site": "b"}

	down := roomy("h-a", 64, 256*gib, 4096*gib)
	down.Answering = false
	draining := roomy("h-a", 64, 256*gib, 4096*gib)
	draining.Draining = true
	for _, c := range []struct {
		what           string
		spec           machineapi.Spec
		cs             []candidate
		reason, figure string
	}{
		{"no host", fits(), nil, "no host answers", "no host answers"},
		{"none answers", fits(), []candidate{down}, "no host answers", "no host answers"},
		{"all draining", fits(), []candidate{draining}, "every host that answers is draining", "every host that answers is draining"},
		{"region", elsewhere, []candidate{roomy("h-a", 64, 256*gib, 4096*gib)}, "no host is in the shape's region", "no host is in the shape's region"},
		{"labels", labelled, []candidate{roomy("h-a", 64, 256*gib, 4096*gib)}, "no host carries the shape's labels", "no host carries the shape's labels"},
		{"gpu", gpu, []candidate{roomy("h-a", 64, 256*gib, 4096*gib)}, "no host has the shape's GPUs", "no host has the shape's GPUs"},
		{"cpu", big, []candidate{roomy("h-a", 20, 256*gib, 4096*gib), roomy("h-b", 31, 256*gib, 4096*gib)}, "no host fits the CPU", "no host has 48 CPUs free; the most free is 31"},
		{"memory", hungry, []candidate{roomy("h-a", 64, 100*gib, 4096*gib), roomy("h-b", 64, 3*gib/2, 4096*gib)}, "no host fits the memory", "no host has 300.0 GiB of memory free; the most free is 100.0 GiB"},
		{"disk", greedy, []candidate{roomy("h-a", 64, 256*gib, 2048*gib)}, "no host fits the disk", "no host has 4.9 TiB of disk free; the most free is 2.0 TiB"},
	} {
		p := place("0123456789abcdef", c.spec, c.cs, "")
		assert.Empty(t, p.host, c.what)
		assert.Equal(t, c.reason, p.reason, c.what)
		assert.Equal(t, c.figure, p.operatorReason, c.what)
		assert.NotContains(t, p.reason, "GiB", "%s: the caller's reason holds no figure of the fleet", c.what)
	}
}

func TestPlace_FiguresCountOnlyTheHostsThatPassedTheEarlierFilters(t *testing.T) {
	big := fits()
	big.CPU = 48
	far := roomy("h-far", 63, 256*gib, 4096*gib)
	far.Region = "us-east"
	big.Shape.Region = "kr-seoul"
	p := place("0123456789abcdef", big, []candidate{far, roomy("h-a", 20, 256*gib, 4096*gib)}, "")
	assert.Equal(t, "no host has 48 CPUs free; the most free is 20", p.operatorReason, "not the 63 of a host in another region")
}

func TestPlace_MatchesALabelTheHostReportsOrTheOperatorSet(t *testing.T) {
	for _, labels := range []map[string]string{
		{"naru.garam.sh/virtualization": "metal"},
		{"naru.garam.sh/site": "a"},
		{"naru.garam.sh/site": "a", "naru.garam.sh/virtualization": "metal"},
	} {
		s := fits()
		s.Shape.Labels = labels
		assert.Equal(t, "h-a", place("0123456789abcdef", s, []candidate{roomy("h-a", 64, 256*gib, 4096*gib)}, "").host, "%v", labels)
	}
	s := fits()
	s.Shape.Labels = map[string]string{"naru.garam.sh/virtualization": "nested"}
	assert.Empty(t, place("0123456789abcdef", s, []candidate{roomy("h-a", 64, 256*gib, 4096*gib)}, "").host, "a value the host does not carry")
}

func TestPlace_SpreadsToTheLargestShareOfMemoryLeft(t *testing.T) {
	small := roomy("h-small", 64, 40*gib, 4096*gib)
	small.allocatable.memory = 50 * gib // 36 of 50 left after it: 72%
	large := roomy("h-large", 64, 150*gib, 4096*gib)
	large.allocatable.memory = 256 * gib // 146 of 256: 57%
	assert.Equal(t, "h-small", place("0123456789abcdef", fits(), []candidate{large, small}, "").host, "the share, not the bytes")
	assert.Equal(t, "h-small", place("0123456789abcdef", fits(), []candidate{small, large}, "").host, "whatever the order")
}

func TestPlace_BreaksATieByTheMachinesID(t *testing.T) {
	cs := []candidate{roomy("h-a", 64, 100*gib, 4096*gib), roomy("h-b", 64, 100*gib, 4096*gib), roomy("h-c", 64, 100*gib, 4096*gib)}
	seen := map[string]bool{}
	for _, id := range []string{"0000000000000001", "0000000000000002", "0000000000000003", "0000000000000004", "0000000000000005", "0000000000000006"} {
		h := place(id, fits(), cs, "")
		assert.Equal(t, h, place(id, fits(), []candidate{cs[2], cs[0], cs[1]}, ""), "the same host for the same id, whatever the order")
		seen[h.host] = true
	}
	assert.Greater(t, len(seen), 1, "ids spread over tied hosts")
}

func TestPlace_PutsARestoreOnTheSnapshotsHostOnly(t *testing.T) {
	cs := []candidate{roomy("h-a", 64, 200*gib, 4096*gib), roomy("h-b", 64, 10*gib, 4096*gib)}
	assert.Equal(t, "h-b", place("0123456789abcdef", fits(), cs, "h-b").host, "the snapshot's host, though the other has more left")

	big := fits()
	big.MemoryBytes = 20 * gib
	p := place("0123456789abcdef", big, cs, "h-b")
	assert.Empty(t, p.host, "it waits rather than go elsewhere")
	assert.Equal(t, "the snapshot's host does not meet it: no host fits the memory", p.reason)
	assert.Equal(t, "the snapshot's host h-b does not meet it: no host has 20.0 GiB of memory free; the most free is 10.0 GiB", p.operatorReason)

	p = place("0123456789abcdef", fits(), cs, "h-gone")
	assert.Empty(t, p.host)
	assert.Equal(t, "the snapshot's host is not admitted", p.reason)
}

func TestSize_WritesTheLargestBinaryUnit(t *testing.T) {
	for n, want := range map[int64]string{512: "512 bytes", 3 << 20: "3.0 MiB", 3 * gib / 2: "1.5 GiB", 2 << 40: "2.0 TiB", -5: "0 bytes"} {
		assert.Equal(t, want, size(n))
	}
}
