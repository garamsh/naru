package machines

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"fmt"
	"maps"
	"math/bits"
	"slices"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
)

// Reserve is what each host keeps for itself, apart from what orch places (ADR 0110 item 1:
// --host-reserve-cpu and --host-reserve-memory).
type Reserve struct {
	CPU    uint32
	Memory uint64
}

// DefaultReserve is the reserve the flags default to: 1 CPU and 2 GiB.
var DefaultReserve = Reserve{CPU: 1, Memory: 2 << 30}

// amounts are CPUs, memory and disk bytes, signed so an account that is over spent reads below zero
// and never wraps.
type amounts struct{ cpu, memory, disk int64 }

func (a amounts) plus(b amounts) amounts {
	return amounts{a.cpu + b.cpu, a.memory + b.memory, a.disk + b.disk}
}

func (a amounts) minus(b amounts) amounts {
	return amounts{a.cpu - b.cpu, a.memory - b.memory, a.disk - b.disk}
}

func of(r hostapi.Resources) amounts {
	return amounts{int64(r.CPU), int64(r.Memory), int64(r.Disk)}
}

func (a amounts) resources() hostapi.Resources {
	return hostapi.Resources{CPU: uint32(max(a.cpu, 0)), Memory: uint64(max(a.memory, 0)), Disk: uint64(max(a.disk, 0))}
}

// need is what the spec takes of a host: its CPUs and memory, and its root disk and volumes.
func need(s machineapi.Spec) amounts {
	disk := s.RootDiskBytes
	for _, v := range s.Volumes {
		disk += v.SizeBytes
	}
	return amounts{int64(s.CPU), int64(s.MemoryBytes), int64(disk)}
}

// takes is what a microVM the host reports takes of it.
func takes(m hostapi.MicroVM) amounts {
	disk := m.RootDiskBytes
	for _, d := range m.Disks {
		disk += d.SizeBytes
	}
	return amounts{int64(m.CPU), int64(m.Memory), int64(disk)}
}

// account is a host's allocatable resources, its capacity less the reserve, and what of them is
// left: less what orch gave away there (alloc, the Machines bound there, stopped ones included),
// every disk snapshot the host keeps, and every microVM it reports that is not of a binding in
// bound, a group's member included (ADR 0110 item 1).
func account(h Host, reserve Reserve, alloc hostapi.Resources, bound map[string]bool) (allocatable, left amounts) {
	allocatable = of(h.Capacity).minus(amounts{int64(reserve.CPU), int64(reserve.Memory), 0})
	left = allocatable.minus(of(alloc))
	for _, d := range h.DiskSnapshots {
		left.disk -= int64(d.AllocatedBytes)
	}
	for _, m := range h.MicroVMs {
		if !bound[m.Name] {
			left = left.minus(takes(m))
		}
	}
	return allocatable, left
}

// candidate is a host with its account in one pass.
type candidate struct {
	Host
	allocatable, left amounts
}

// placement is where a Machine goes, or why it goes nowhere: Reason for its caller, OperatorReason
// with the fleet's figures.
type placement struct {
	host                   string
	reason, operatorReason string
}

// stage is one condition of ADR 0110 item 4, in its order: none is the reason when no host meets
// it, and figures the operator's, given the candidates that reached it.
type stage struct {
	holds   func(c candidate) bool
	none    string
	figures func(cs []candidate) string
}

// place chooses the host of the spec of the Machine id among the candidates: the filters of ADR 0110
// item 4, then the one with the largest share of its allocatable memory left after the placement,
// a tie broken by rendezvous hashing of id over the hosts' IDs (item 5). A restore goes only to the
// snapshot's host, onto, and its reasons name that host.
func place(id string, spec machineapi.Spec, cs []candidate, onto string) placement {
	n := need(spec)
	stages := []stage{
		{func(c candidate) bool { return c.Answering }, "no host answers", nil},
		{func(c candidate) bool { return !c.Draining }, "every host that answers is draining", nil},
		{func(c candidate) bool { return spec.Shape.Region == "" || c.Region == spec.Shape.Region }, "no host is in the shape's region", nil},
		{func(c candidate) bool { return carries(c.Host, spec.Shape.Labels) }, "no host carries the shape's labels", nil},
		// No host reports GPUs before the later goal's GPU hosts (ADR 0111), so a GPU Machine waits.
		{func(c candidate) bool { return spec.Shape.GPU == nil }, "no host has the shape's GPUs", nil},
		{func(c candidate) bool { return c.left.cpu >= n.cpu }, "no host fits the CPU", func(cs []candidate) string {
			return fmt.Sprintf("no host has %d CPUs free; the most free is %d", n.cpu, most(cs, func(a amounts) int64 { return a.cpu }))
		}},
		{func(c candidate) bool { return c.left.memory >= n.memory }, "no host fits the memory", func(cs []candidate) string {
			return fmt.Sprintf("no host has %s of memory free; the most free is %s", size(n.memory), size(most(cs, func(a amounts) int64 { return a.memory })))
		}},
		{func(c candidate) bool { return c.left.disk >= n.disk }, "no host fits the disk", func(cs []candidate) string {
			return fmt.Sprintf("no host has %s of disk free; the most free is %s", size(n.disk), size(most(cs, func(a amounts) int64 { return a.disk })))
		}},
	}
	if onto != "" {
		cs = slices.DeleteFunc(slices.Clone(cs), func(c candidate) bool { return c.ID != onto })
		if len(cs) == 0 {
			return placement{reason: "the snapshot's host is not admitted", operatorReason: "the snapshot's host " + onto + " is not admitted"}
		}
	}
	for _, s := range stages {
		next := slices.DeleteFunc(slices.Clone(cs), func(c candidate) bool { return !s.holds(c) })
		if len(next) == 0 {
			p := placement{reason: s.none, operatorReason: s.none}
			if s.figures != nil {
				p.operatorReason = s.figures(cs)
			}
			if onto != "" {
				p.reason, p.operatorReason = "the snapshot's host does not meet it: "+p.reason, "the snapshot's host "+onto+" does not meet it: "+p.operatorReason
			}
			return p
		}
		cs = next
	}
	best := cs[0]
	for _, c := range cs[1:] {
		if better(id, n, c, best) {
			best = c
		}
	}
	return placement{host: best.ID}
}

// better reports whether a is the better host than b for a Machine id that needs n: the larger share
// of allocatable memory left after it, and on a tie the higher rendezvous hash.
func better(id string, n amounts, a, b candidate) bool {
	// share(a) > share(b) compared as fractions, which floats could round into a false tie, with
	// 128-bit products, since two byte counts of a host overflow 64 bits. Every candidate that
	// passed the filters has left at least n, and allocatable at least left.
	if c := mul(a.left.memory-n.memory, b.allocatable.memory).cmp(mul(b.left.memory-n.memory, a.allocatable.memory)); c != 0 {
		return c > 0
	}
	ha, hb := sha256.Sum256([]byte(id+"/"+a.ID)), sha256.Sum256([]byte(id+"/"+b.ID))
	return bytes.Compare(ha[:], hb[:]) > 0
}

// u128 is an unsigned 128-bit product.
type u128 struct{ hi, lo uint64 }

// mul is the product of two amounts that are not negative.
func mul(a, b int64) u128 {
	hi, lo := bits.Mul64(uint64(max(a, 0)), uint64(max(b, 0)))
	return u128{hi, lo}
}

func (x u128) cmp(y u128) int {
	if c := cmp.Compare(x.hi, y.hi); c != 0 {
		return c
	}
	return cmp.Compare(x.lo, y.lo)
}

// carries reports whether the host has every label, as the operator set it or the host reports it.
func carries(h Host, labels map[string]string) bool {
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		v, ok := h.Labels[k]
		if !ok {
			v, ok = h.Reported[k]
		}
		if !ok || v != labels[k] {
			return false
		}
	}
	return true
}

func most(cs []candidate, pick func(amounts) int64) int64 {
	m := int64(0)
	for _, c := range cs {
		m = max(m, pick(c.left))
	}
	return m
}

// size is a byte count in the largest binary unit that keeps one decimal.
func size(n int64) string {
	for _, u := range []struct {
		name  string
		bytes int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}} {
		if n >= u.bytes {
			return fmt.Sprintf("%.1f %s", float64(n)/float64(u.bytes), u.name)
		}
	}
	return fmt.Sprintf("%d bytes", max(n, 0))
}
