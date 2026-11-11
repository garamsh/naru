package iso9660_test

import (
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/iso9660"
)

const sector = 2048

var files = []iso9660.File{
	{Name: "user-data", Data: []byte("the user-data\n")},
	{Name: "network-config", Data: []byte(strings.Repeat("n", 3000))},
	{Name: "meta-data", Data: nil},
}

// listing is a root directory as ECMA-119 lays it out: the records after "." and "..".
type listing map[string][]byte

// readTree reads the root directory the descriptor at sector desc points to, decoding names with decode.
func readTree(t *testing.T, img []byte, desc int, decode func([]byte) string) listing {
	t.Helper()
	d := img[desc*sector:]
	rootAt := 156
	extent := int(binary.LittleEndian.Uint32(d[rootAt+2:]))
	dir := img[extent*sector : (extent+1)*sector]
	out := listing{}
	for off := 0; dir[off] != 0; off += int(dir[off]) {
		rec := dir[off : off+int(dir[off])]
		id := rec[33 : 33+int(rec[32])]
		if len(id) == 1 && id[0] <= 1 {
			continue
		}
		at := int(binary.LittleEndian.Uint32(rec[2:]))
		size := int(binary.LittleEndian.Uint32(rec[10:]))
		assert.Equal(t, binary.BigEndian.Uint32(rec[6:]), uint32(at), "the extent is both-endian")
		out[decode(id)] = img[at*sector : at*sector+size]
	}
	return out
}

func ucs2(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.BigEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

func TestWrite_ALabelledVolumeWithItsFilesInBothTrees(t *testing.T) {
	img, err := iso9660.Write("cidata", files)

	require.NoError(t, err)
	require.Zero(t, len(img)%sector)
	pvd, svd := img[16*sector:], img[17*sector:]
	assert.Equal(t, byte(1), pvd[0])
	assert.Equal(t, "CD001", string(pvd[1:6]))
	assert.Equal(t, "cidata"+strings.Repeat(" ", 26), string(pvd[40:72]))
	assert.Equal(t, byte(2), svd[0])
	assert.Equal(t, "%/E", string(svd[88:91]), "the Joliet escape sequence")
	assert.Equal(t, "cidata", strings.TrimRight(ucs2(svd[40:72]), " "))
	assert.Equal(t, byte(255), img[18*sector], "the terminator")
	assert.Equal(t, uint32(len(img)/sector), binary.LittleEndian.Uint32(pvd[80:]), "the volume size in sectors")

	want := listing{"user-data": files[0].Data, "network-config": files[1].Data, "meta-data": {}}
	assert.Equal(t, want, readTree(t, img, 17, ucs2), "the Joliet tree carries the names the kernel shows")
	assert.Equal(t, listing{"USER_DAT.;1": files[0].Data, "NETWORK_.;1": files[1].Data, "META_DAT.;1": {}},
		readTree(t, img, 16, func(b []byte) string { return string(b) }), "the primary tree carries 8.3 names")
}

func TestWrite_IsDeterministic(t *testing.T) {
	a, err := iso9660.Write("cidata", files)
	require.NoError(t, err)
	b, err := iso9660.Write("cidata", []iso9660.File{files[2], files[1], files[0]})
	require.NoError(t, err)

	assert.Equal(t, a, b, "the order the files are given in does not matter")
}

func TestWrite_RefusesWhatItCannotRepresent(t *testing.T) {
	tests := map[string]struct {
		label string
		files []iso9660.File
		want  string
	}{
		"an empty label":          {"", files, "label"},
		"a label with a space":    {"ci data", files, "label"},
		"a label over 16":         {strings.Repeat("a", 17), files, "label"},
		"an empty name":           {"cidata", []iso9660.File{{Name: ""}}, "file name"},
		"a name with a slash":     {"cidata", []iso9660.File{{Name: "a/b"}}, "file name"},
		"a name used twice":       {"cidata", []iso9660.File{{Name: "a"}, {Name: "a"}}, "twice"},
		"names equal in 8.3 form": {"cidata", []iso9660.File{{Name: "user-data-1"}, {Name: "user-data-2"}}, "8.3"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := iso9660.Write(tt.label, tt.files)

			assert.ErrorContains(t, err, tt.want)
		})
	}
}
