package linux

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/ext4/ext4test"
	"github.com/garamsh/naru/lib/init/netcfg"
)

type mountCall struct{ source, target, fstype string }

// testHost is a Host over a temporary /sys/class/block, /dev and /proc/sys,
// whose mount records its calls. devices maps a device name to the image or
// file that stands for it.
func testHost(t *testing.T, devices map[string]string) (*Host, *[]mountCall) {
	t.Helper()
	root := t.TempDir()
	h := &Host{
		sysClassBlock: filepath.Join(root, "sys/class/block"),
		devDir:        filepath.Join(root, "dev"),
		procSys:       filepath.Join(root, "proc/sys"),

		seedMountPoint: filepath.Join(root, "run/naru/cidata"),
		resolvConf:     filepath.Join(root, "run/naru/resolv.conf"),
		links:          &fakeLinks{},
	}
	for _, d := range []string{h.sysClassBlock, h.devDir, filepath.Join(h.procSys, "net/ipv4"), filepath.Join(h.procSys, "net/ipv6/conf/all")} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	for name, file := range devices {
		require.NoError(t, os.WriteFile(filepath.Join(h.sysClassBlock, name), nil, 0o644))
		require.NoError(t, os.Symlink(file, filepath.Join(h.devDir, name)))
	}
	var calls []mountCall
	h.mount = func(source, target, fstype string, _ uintptr, _ string) error {
		calls = append(calls, mountCall{source, target, fstype})
		return nil
	}
	return h, &calls
}

func notExt4(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "zero")
	require.NoError(t, os.WriteFile(p, make([]byte, 8192), 0o644))
	return p
}

// isoImage returns a file that holds an ISO 9660 primary volume descriptor with the label.
func isoImage(t *testing.T, label string) string {
	t.Helper()
	img := make([]byte, 20*2048)
	pvd := img[16*2048:]
	pvd[0] = 1
	copy(pvd[1:], "CD001")
	pvd[6] = 1
	copy(pvd[40:72], strings.Repeat(" ", 32))
	copy(pvd[40:], label)
	p := filepath.Join(t.TempDir(), "iso")
	require.NoError(t, os.WriteFile(p, img, 0o644))
	return p
}

func TestHost_FindsADiskByItsLabel(t *testing.T) {
	h, _ := testHost(t, map[string]string{
		"vda": ext4test.Image(t, "OTHER", "", nil),
		"vdb": notExt4(t),
		"vdc": ext4test.Image(t, "NARUDATA", "", nil),
		"sr0": isoImage(t, "cidata"),
	})
	named := func(want string) func(string) bool { return func(got string) bool { return got == want } }

	dev, err := h.find(ext4Label, named("NARUDATA"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(h.devDir, "vdc"), dev)

	dev, err = h.find(isoLabel, named("cidata"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(h.devDir, "sr0"), dev)

	_, err = h.find(ext4Label, named("cidata"))
	assert.ErrorIs(t, err, errNoDisk, "an ISO volume is not an ext4 disk")

	_, err = h.find(isoLabel, named("NARUDATA"))
	assert.ErrorIs(t, err, errNoDisk, "an ext4 disk is not an ISO volume")
}

func TestHost_ReadSeedMountsTheCidataVolumeAndReadsItsFiles(t *testing.T) {
	h, calls := testHost(t, map[string]string{
		"vda": ext4test.Image(t, "cidata", "", nil),
		"sr0": isoImage(t, "CIDATA"),
	})
	// The fake mount does nothing, so the files are placed where the mount would show them.
	require.NoError(t, os.MkdirAll(h.seedMountPoint, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(h.seedMountPoint, "user-data"), []byte("the naru.yaml"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(h.seedMountPoint, "network-config"), []byte("the network"), 0o644))

	userData, networkConfig, err := h.ReadSeed()

	require.NoError(t, err)
	assert.Equal(t, "the naru.yaml", string(userData))
	assert.Equal(t, "the network", string(networkConfig))
	assert.Equal(t, []mountCall{{filepath.Join(h.devDir, "sr0"), h.seedMountPoint, "iso9660"}}, *calls,
		"the label matches in either case, and an ext4 disk labelled cidata is not the drive")
}

func TestHost_ReadSeedMountsReadOnlyAndHardened(t *testing.T) {
	h, _ := testHost(t, map[string]string{"sr0": isoImage(t, "cidata")})
	var flags uintptr
	h.mount = func(_, _, _ string, f uintptr, _ string) error { flags = f; return nil }

	_, _, _ = h.ReadSeed()

	want := uintptr(syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
	assert.Equal(t, want, flags)
}

func TestHost_ReadSeedRefuses(t *testing.T) {
	t.Run("a Node with no cidata volume", func(t *testing.T) {
		h, calls := testHost(t, map[string]string{"vda": ext4test.Image(t, "NARUCFG", "", nil), "sr0": isoImage(t, "other")})

		_, _, err := h.ReadSeed()

		assert.ErrorIs(t, err, errNoDisk)
		assert.Empty(t, *calls)
	})

	t.Run("a volume without a network-config", func(t *testing.T) {
		h, _ := testHost(t, map[string]string{"sr0": isoImage(t, "cidata")})
		require.NoError(t, os.MkdirAll(h.seedMountPoint, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(h.seedMountPoint, "user-data"), []byte("x"), 0o644))

		_, _, err := h.ReadSeed()

		assert.ErrorIs(t, err, os.ErrNotExist)
	})
}

// fakeLinks is a set of interfaces whose changes are recorded.
type fakeLinks struct {
	all   []link
	calls []string
	fail  string
}

func (f *fakeLinks) List() ([]link, error) { return f.all, nil }

func (f *fakeLinks) do(call string) error {
	f.calls = append(f.calls, call)
	if f.fail == call {
		return errors.New("refused")
	}
	return nil
}

func (f *fakeLinks) SetUp(l link) error { return f.do("up " + l.Name) }
func (f *fakeLinks) AddAddr(l link, p netip.Prefix) error {
	return f.do("addr " + p.String() + " " + l.Name)
}
func (f *fakeLinks) SetDefaultRoute(l link, gw netip.Addr) error {
	return f.do("route " + gw.String() + " " + l.Name)
}

func (f *fakeLinks) ReplaceRoute(to netip.Prefix, via netip.Addr) error {
	return f.do("replace-route " + to.String() + " " + via.String())
}
func (f *fakeLinks) DeleteRoute(to netip.Prefix, via netip.Addr) error {
	return f.do("delete-route " + to.String() + " " + via.String())
}

func TestHost_ApplyNetwork(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0, 0, 0, 0, 0x07}
	nic := func(gw string) netcfg.Interface {
		i := netcfg.Interface{Name: "eth0", MAC: mac, Addresses: []netip.Prefix{netip.MustParsePrefix("10.78.0.7/24")}}
		if gw != "" {
			i.Gateway = netip.MustParseAddr(gw)
		}
		return i
	}
	kernel := []link{{1, "lo", nil}, {2, "ens18", net.HardwareAddr{0x02, 0, 0, 0, 0, 0x08}}, {3, "ens19", mac}}

	t.Run("finds the interface by MAC whatever the kernel named it, and applies address, link and route", func(t *testing.T) {
		h, _ := testHost(t, nil)
		h.links = &fakeLinks{all: kernel}

		err := h.ApplyNetwork(netcfg.Config{Interfaces: []netcfg.Interface{nic("10.78.0.254")}})

		require.NoError(t, err)
		assert.Equal(t, []string{"up lo", "up ens19", "addr 10.78.0.7/24 ens19", "route 10.78.0.254 ens19"}, h.links.(*fakeLinks).calls)
	})

	t.Run("finds the interface by name when the entry has no MAC", func(t *testing.T) {
		h, _ := testHost(t, nil)
		h.links = &fakeLinks{all: kernel}
		byName := nic("")
		byName.MAC, byName.Name = nil, "ens18"

		require.NoError(t, h.ApplyNetwork(netcfg.Config{Interfaces: []netcfg.Interface{byName}}))

		assert.Equal(t, []string{"up lo", "up ens18", "addr 10.78.0.7/24 ens18"}, h.links.(*fakeLinks).calls, "no gateway, no route")
	})

	t.Run("refuses an interface the kernel does not have", func(t *testing.T) {
		h, _ := testHost(t, nil)
		h.links = &fakeLinks{all: kernel[:2]}

		err := h.ApplyNetwork(netcfg.Config{Interfaces: []netcfg.Interface{nic("")}})

		assert.ErrorContains(t, err, "02:00:00:00:00:07")
		assert.Equal(t, []string{"up lo"}, h.links.(*fakeLinks).calls)
		assert.NoFileExists(t, h.resolvConf, "the resolvers are written only after the interfaces are")
	})

	t.Run("stops at the first netlink call that fails", func(t *testing.T) {
		h, _ := testHost(t, nil)
		h.links = &fakeLinks{all: kernel, fail: "addr 10.78.0.7/24 ens19"}

		err := h.ApplyNetwork(netcfg.Config{Interfaces: []netcfg.Interface{nic("10.78.0.254")}})

		assert.ErrorContains(t, err, "10.78.0.7/24")
		assert.Equal(t, []string{"up lo", "up ens19", "addr 10.78.0.7/24 ens19"}, h.links.(*fakeLinks).calls)
	})

	t.Run("refuses a kernel with no loopback", func(t *testing.T) {
		h, _ := testHost(t, nil)
		h.links = &fakeLinks{all: kernel[1:]}

		err := h.ApplyNetwork(netcfg.Config{Interfaces: []netcfg.Interface{nic("")}})

		assert.ErrorContains(t, err, `"lo"`)
	})

	t.Run("writes the resolvers", func(t *testing.T) {
		h, _ := testHost(t, nil)
		h.links = &fakeLinks{all: kernel}
		cfg := netcfg.Config{
			Interfaces:  []netcfg.Interface{nic("")},
			Nameservers: []netip.Addr{netip.MustParseAddr("10.78.0.254"), netip.MustParseAddr("1.1.1.1")},
			Search:      []string{"naru.test", "lan"},
		}

		require.NoError(t, h.ApplyNetwork(cfg))

		assert.Equal(t, "search naru.test lan\nnameserver 10.78.0.254\nnameserver 1.1.1.1\n", read(t, h.resolvConf))
	})
}

func TestHost_WriteFiles(t *testing.T) {
	h, _ := testHost(t, nil)
	dir := t.TempDir()
	files := []config.File{
		{Path: filepath.Join(dir, "mvmd/config.json"), Mode: "0600", Content: "{}"},
		{Path: filepath.Join(dir, "plain"), Content: "x"},
		{Path: filepath.Join(dir, "open"), Mode: "0666", Content: "y"},
	}

	require.NoError(t, h.WriteFiles(files))

	assert.Equal(t, "{}", read(t, files[0].Path))
	info, err := os.Stat(files[0].Path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the mode is exact, whatever the umask")
	info, err = os.Stat(files[1].Path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	info, err = os.Stat(files[2].Path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o666), info.Mode().Perm(), "a mode the umask would clear is still exact")

	err = h.WriteFiles(files[:1])
	assert.ErrorIs(t, err, os.ErrExist, "a file that exists is not replaced")
}

func TestHost_ReplaceFile(t *testing.T) {
	h, _ := testHost(t, nil)
	path := filepath.Join(t.TempDir(), "mvmd/config.json")
	require.NoError(t, h.WriteFiles([]config.File{{Path: path, Mode: "0600", Content: "old"}}))

	require.NoError(t, h.ReplaceFile(config.File{Path: path, Mode: "0640", Content: "new"}))

	assert.Equal(t, "new", read(t, path), "a file that exists is replaced")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm(), "the mode is the new file's")
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file stays beside it")

	fresh := filepath.Join(t.TempDir(), "deep/new")
	require.NoError(t, h.ReplaceFile(config.File{Path: fresh, Content: "x"}))
	assert.Equal(t, "x", read(t, fresh), "a file that does not exist is created with its directories")
}

func TestHost_MountDataDisk(t *testing.T) {
	verity := ext4test.Image(t, "NARUDATA", "verity", nil)
	plain := ext4test.Image(t, "NARUDATA", "", nil)
	mp := filepath.Join(t.TempDir(), "var/lib/mvmd")

	t.Run("mounts an ext4 disk with verity, found by label", func(t *testing.T) {
		h, calls := testHost(t, map[string]string{"vda": verity})

		err := h.MountDataDisk(config.DataDisk{Label: "NARUDATA", MountPoint: mp})

		require.NoError(t, err)
		assert.Equal(t, []mountCall{{filepath.Join(h.devDir, "vda"), mp, "ext4"}}, *calls)
	})

	t.Run("mounts an ext4 disk with verity, named by device", func(t *testing.T) {
		h, calls := testHost(t, map[string]string{"vda": verity})
		dev := filepath.Join(h.devDir, "vda")

		require.NoError(t, h.MountDataDisk(config.DataDisk{Device: dev, MountPoint: mp}))

		assert.Equal(t, []mountCall{{dev, mp, "ext4"}}, *calls)
	})

	t.Run("refuses an ext4 disk without verity", func(t *testing.T) {
		h, calls := testHost(t, map[string]string{"vda": plain})

		err := h.MountDataDisk(config.DataDisk{Label: "NARUDATA", MountPoint: mp})

		assert.ErrorContains(t, err, "verity")
		assert.Empty(t, *calls)
	})

	t.Run("refuses a disk that is not ext4", func(t *testing.T) {
		h, calls := testHost(t, map[string]string{"vda": notExt4(t)})

		err := h.MountDataDisk(config.DataDisk{Device: filepath.Join(h.devDir, "vda"), MountPoint: mp})

		assert.ErrorContains(t, err, "not an ext4")
		assert.Empty(t, *calls)
	})
}

func TestHost_EnableForwardingWritesOnlyTheNamedFamilies(t *testing.T) {
	h, _ := testHost(t, nil)
	v4 := filepath.Join(h.procSys, "net/ipv4/ip_forward")
	v6 := filepath.Join(h.procSys, "net/ipv6/conf/all/forwarding")
	for _, f := range []string{v4, v6} {
		require.NoError(t, os.WriteFile(f, []byte("0"), 0o644))
	}

	require.NoError(t, h.EnableForwarding([]config.Family{config.IPv6}))

	assert.Equal(t, "0", read(t, v4), "a family not named stays as it was")
	assert.Equal(t, "1", read(t, v6))

	require.NoError(t, h.EnableForwarding([]config.Family{config.IPv4}))
	assert.Equal(t, "1", read(t, v4))
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}
