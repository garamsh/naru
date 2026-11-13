package api

import "github.com/garamsh/naru/host/internal/mvmd"

func mvmdDisk(id string) mvmd.Disk { return mvmd.Disk{ID: id, SizeBytes: 1} }
