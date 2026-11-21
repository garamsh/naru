package fat

// Allocate, Free and Probes let the tests drive the allocator and count the table entries it reads.
func (v *Volume) Allocate() (uint32, error) { return v.allocate() }

func (v *Volume) Free(cluster uint32) { v.table[cluster] = 0 }

func (v *Volume) Probes() uint64 { return v.probes }

// LowestFree is the allocator this package used before it kept a hint: the lowest free cluster.
func (v *Volume) LowestFree() uint32 {
	for c := uint32(2); c < v.clusters; c++ {
		if v.table[c]&entryMask == 0 {
			return c
		}
	}
	return 0
}
