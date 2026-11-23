package cpiolist_test

import (
	"bytes"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/disk/internal/cpiolist"
)

func parse(t *testing.T, list string) []cpiolist.Entry {
	t.Helper()
	entries, err := cpiolist.Parse(strings.NewReader(list))
	require.NoError(t, err)
	return entries
}

func format(t *testing.T, entries []cpiolist.Entry) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, cpiolist.Format(&b, entries))
	return b.String()
}

// The list of garamsh/naru#356: the add-ons' directory comes before /usr/lib/naru and /usr, which are
// named after it.
const unordered = `# the add-ons
dir /usr/lib/naru/addons 0755 0 0
file /usr/lib/naru/addons/dns.yaml /stage/dns.yaml 0644 0 0
dir /usr 0755 0 0
dir /usr/lib 0755 0 0
dir /usr/lib/naru 0755 0 0
file /init /stage/init 0755 0 0
`

func TestCheck_RefusesAnEntryThatComesBeforeItsParentDirectory(t *testing.T) {
	err := cpiolist.Check(parse(t, unordered))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "line 2: /usr/lib/naru/addons comes before its parent directory /usr/lib/naru")
}

func TestCheck_AcceptsTheSameEntriesOnceTheirParentsComeFirst(t *testing.T) {
	ordered := `dir /usr 0755 0 0
dir /usr/lib 0755 0 0
dir /usr/lib/naru 0755 0 0
dir /usr/lib/naru/addons 0755 0 0
file /usr/lib/naru/addons/dns.yaml /stage/dns.yaml 0644 0 0
file /init /stage/init 0755 0 0
`
	require.NoError(t, cpiolist.Check(parse(t, ordered)), "the control: the refusal above is the order's")
}

func TestCheck_RefusesAParentThatIsNoDirectoryAndOneThatNothingMakes(t *testing.T) {
	err := cpiolist.Check(parse(t, "file /etc /stage/etc 0644 0 0\nfile /etc/hosts /stage/hosts 0644 0 0\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/etc/hosts is in /etc")
	assert.Contains(t, err.Error(), "which is a file")

	err = cpiolist.Check(parse(t, "file /etc/hosts /stage/hosts 0644 0 0\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "comes before its parent directory /etc", "a parent that is made by no entry is the same refusal")

	require.NoError(t, cpiolist.Check(parse(t, "dir /etc 0755 0 0\nfile /etc/hosts /stage/hosts 0644 0 0\nslink /run2 /run 0777 0 0\n")), "the control: a directory first, and an entry directly under /")
}

func TestOrder_PutsEveryDirectoryBeforeItsEntries(t *testing.T) {
	ordered, err := cpiolist.Order(parse(t, unordered))
	require.NoError(t, err)

	require.NoError(t, cpiolist.Check(ordered))
	assert.Equal(t, `dir /usr 0755 0 0
dir /usr/lib 0755 0 0
dir /usr/lib/naru 0755 0 0
dir /usr/lib/naru/addons 0755 0 0
file /usr/lib/naru/addons/dns.yaml /stage/dns.yaml 0644 0 0
file /init /stage/init 0755 0 0
`, format(t, ordered), "the directories the list names are emitted as it wrote them, once, before their first entry")
}

func TestOrder_MakesAnUnnamedParentAndKeepsTheModeOfANamedOne(t *testing.T) {
	ordered, err := cpiolist.Order(parse(t, "file /tmp/x/y /stage/y 0644 0 0\ndir /tmp 1777 0 0\n"))
	require.NoError(t, err)

	assert.Equal(t, "dir /tmp 1777 0 0\ndir /tmp/x 0755 0 0\nfile /tmp/x/y /stage/y 0644 0 0\n", format(t, ordered))
}

func TestOrder_IsTheSameForTheSameInputAndLeavesAnOrderedListAlone(t *testing.T) {
	first, err := cpiolist.Order(parse(t, unordered))
	require.NoError(t, err)
	again, err := cpiolist.Order(parse(t, format(t, first)))
	require.NoError(t, err)

	assert.Equal(t, format(t, first), format(t, again))
}

func TestOrder_RefusesAPathNamedTwiceAndAFileThatHoldsEntries(t *testing.T) {
	_, err := cpiolist.Order(parse(t, "dir /a 0755 0 0\ndir /a 0755 0 0\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/a is named again")

	_, err = cpiolist.Order(parse(t, "file /a /stage/a 0644 0 0\nfile /a/b /stage/b 0644 0 0\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "which line 1 makes a file")
}

func TestParse_RefusesALineWithNoPathAndARelativePath(t *testing.T) {
	_, err := cpiolist.Parse(strings.NewReader("dir\n"))
	require.Error(t, err)
	_, err = cpiolist.Parse(strings.NewReader("dir usr 0755 0 0\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not absolute")
}

// The lists the images are built from. The Host's list is completed by assemble.sh with the guest
// files' directories, so ordering is what makes it valid; the check is what the build runs on the
// result.
func TestImageLists_AreOrderedBeforeTheyAreArchived(t *testing.T) {
	const addons = "/usr/lib/naru/addons"
	for _, list := range []string{"initramfs-host", "initramfs-node"} {
		t.Run(list, func(t *testing.T) {
			raw, err := os.ReadFile("../../../../image/pkgs/" + list + "/initramfs.list")
			require.NoError(t, err)
			entries := parse(t, strings.ReplaceAll(string(raw), "@OUT@", "/stage"))
			require.NotEmpty(t, entries)

			ordered, err := cpiolist.Order(entries)
			require.NoError(t, err)

			require.NoError(t, cpiolist.Check(ordered), "every entry's parent is made before it")
			index := map[string]int{}
			for i, e := range ordered {
				index[e.Path] = i
			}
			for p, i := range index {
				for d := path.Dir(p); d != "/"; d = path.Dir(d) {
					assert.Less(t, index[d], i, "%s is made before %s", d, p)
				}
			}
			assert.Contains(t, index, addons+"/images/coredns.tar", "the add-on image is in the list")
			assert.Less(t, index["/usr/lib/naru"], index[addons], "/usr/lib/naru is made before the add-ons' directory")
			assert.Len(t, ordered, len(index), "no path is in the list twice")
		})
	}
}

func TestImageLists_TheHostsListAsWrittenIsTheDefectOrderingRemoves(t *testing.T) {
	raw, err := os.ReadFile("../../../../image/pkgs/initramfs-host/initramfs.list")
	require.NoError(t, err)
	entries := parse(t, string(raw))

	require.Error(t, cpiolist.Check(entries), "the control for the test above: the Host's list alone names the add-ons before /usr/lib/naru")
	ordered, err := cpiolist.Order(entries)
	require.NoError(t, err)
	require.NoError(t, cpiolist.Check(ordered))
}
