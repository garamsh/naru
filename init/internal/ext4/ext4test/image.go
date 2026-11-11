// Package ext4test builds ext4 images with mkfs.ext4, without root, for tests.
package ext4test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const mkfs = "/sbin/mkfs.ext4"

// Image builds a 16 MiB ext4 image labelled label, with the extra features
// ("-O" argument, empty for none) and holding files (name to content), and
// returns its path. A missing mkfs.ext4 fails the test.
func Image(t testing.TB, label, features string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	img := filepath.Join(dir, "disk.img")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(16 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	args := []string{"-q", "-L", label, "-d", src}
	if features != "" {
		args = append(args, "-O", features)
	}
	if out, err := exec.Command(mkfs, append(args, img)...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", mkfs, args, err, out)
	}
	return img
}
