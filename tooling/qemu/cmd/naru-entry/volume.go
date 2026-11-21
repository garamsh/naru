package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// volume writes text to dir/data and reports "wrote", or, when the file is there, reports "read" and what it
// holds: a Pod that finds its predecessor's data on the volume.
func volume(dir, text string) {
	file := filepath.Join(dir, "data")
	if b, err := os.ReadFile(file); err == nil {
		fmt.Printf("naru-entry: volume read %s\n", b)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Printf("naru-entry: volume %s: %v\n", file, err)
		return
	}
	f, err := os.Create(file)
	if err == nil {
		_, err = f.WriteString(text)
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		fmt.Printf("naru-entry: volume %s: %v\n", file, err)
		return
	}
	_ = f.Close()
	fmt.Printf("naru-entry: volume wrote %s\n", text)
}

// fill writes 1 MiB blocks to dir/fill until a write fails and prints the error (EDQUOT on a volume with a
// quota, whose limit the write meets) and the whole MiB written.
func fill(dir string) {
	f, err := os.Create(filepath.Join(dir, "fill"))
	if err != nil {
		fmt.Printf("naru-entry: fill: %v\n", err)
		return
	}
	defer func() { _ = f.Close() }()
	block, mib := bytes.Repeat([]byte{0xa5}, 1<<20), 0
	for ; mib < 64<<10; mib++ {
		if _, err = f.Write(block); err != nil {
			break
		}
		if err = f.Sync(); err != nil {
			break
		}
	}
	fmt.Printf("naru-entry: fill ended after %d MiB: %v\n", mib, err)
}

// block writes a pattern to the first MiB of the device, reads it back and prints "block verified <n>" with
// the length that matched.
func block(device string) {
	want := bytes.Repeat([]byte("naru-block "), 1<<16)[:1<<20]
	f, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		fmt.Printf("naru-entry: block: %v\n", err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err = f.WriteAt(want, 0); err == nil {
		err = f.Sync()
	}
	got := make([]byte, len(want))
	if err == nil {
		_, err = f.ReadAt(got, 0)
	}
	if err != nil || !bytes.Equal(got, want) {
		fmt.Printf("naru-entry: block mismatch: %v\n", err)
		return
	}
	fmt.Printf("naru-entry: block verified %d\n", len(got))
}
