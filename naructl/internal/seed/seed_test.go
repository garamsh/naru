package seed_test

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/naructl/internal/seed"
)

const sector = 2048

var good = seed.Join{
	OrchURL:   "https://orch.example:8443",
	CASHA256:  strings.Repeat("ab", 32),
	EnrollKey: "naru-ek-" + strings.Repeat("cd", 32),
	Name:      "rack-3",
}

// root reads the Joliet root directory of an ISO 9660 volume, and the volume's label.
func root(t *testing.T, img []byte) (string, map[string][]byte) {
	t.Helper()
	label := strings.TrimRight(string(img[16*sector+40:16*sector+72]), " ")
	d := img[17*sector:]
	require.Equal(t, byte(2), d[0], "a Joliet descriptor")
	extent := int(binary.LittleEndian.Uint32(d[156+2:]))
	dir := img[extent*sector : (extent+1)*sector]
	out := map[string][]byte{}
	for off := 0; dir[off] != 0; off += int(dir[off]) {
		rec := dir[off : off+int(dir[off])]
		id := rec[33 : 33+int(rec[32])]
		if len(id) == 1 && id[0] <= 1 {
			continue
		}
		u := make([]uint16, len(id)/2)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(id[2*i:])
		}
		at := int(binary.LittleEndian.Uint32(rec[2:]))
		size := int(binary.LittleEndian.Uint32(rec[10:]))
		out[string(utf16.Decode(u))] = img[at*sector : at*sector+size]
	}
	return label, out
}

func TestISO_IsACidataVolumeWhoseUserDataIsTheJoinFormAndNothingElse(t *testing.T) {
	img, err := good.ISO()
	require.NoError(t, err)
	label, files := root(t, img)
	assert.Equal(t, seed.Label, label)
	require.Len(t, files, 2, "user-data and meta-data: no network-config, so the installer asks DHCP")
	assert.Contains(t, files, "user-data")
	assert.Contains(t, files, "meta-data")

	var form seed.Form
	dec := yaml.NewDecoder(bytes.NewReader(files["user-data"]))
	dec.KnownFields(true)
	require.NoError(t, dec.Decode(&form))
	assert.Equal(t, "NodeConfig", form.Kind)
	// The form generic-decoded has exactly these keys: nothing of a Node's configuration rides along.
	var raw map[string]any
	require.NoError(t, yaml.Unmarshal(files["user-data"], &raw))
	assert.ElementsMatch(t, []string{"apiVersion", "kind", "metadata", "spec"}, keys(raw))
	spec := raw["spec"].(map[string]any)
	assert.Equal(t, []string{"join"}, keys(spec))
	join := spec["join"].(map[string]any)
	assert.ElementsMatch(t, []string{"orch", "enrollKey"}, keys(join))
	assert.Equal(t, map[string]any{"url": good.OrchURL, "caSHA256": good.CASHA256}, join["orch"])
	assert.Equal(t, good.EnrollKey, join["enrollKey"])
	assert.Equal(t, map[string]any{"name": "rack-3"}, raw["metadata"])
}

func TestUserData_LeavesOutTheNameWhenThereIsNone(t *testing.T) {
	j := good
	j.Name = ""
	ud, err := j.UserData()
	require.NoError(t, err)
	assert.NotContains(t, string(ud), "metadata")
}

func TestISO_RefusesWhatTheInstallerCouldNotUse(t *testing.T) {
	for name, mutate := range map[string]func(*seed.Join){
		"http":             func(j *seed.Join) { j.OrchURL = "http://orch.example:8443" },
		"no host":          func(j *seed.Join) { j.OrchURL = "https://" },
		"a path":           func(j *seed.Join) { j.OrchURL = "https://orch.example/v1" },
		"short digest":     func(j *seed.Join) { j.CASHA256 = "abcd" },
		"upper-case":       func(j *seed.Join) { j.CASHA256 = strings.ToUpper(good.CASHA256) },
		"no key":           func(j *seed.Join) { j.EnrollKey = "" },
		"a short key":      func(j *seed.Join) { j.EnrollKey = "naru-ek-ab" },
		"a key with a gap": func(j *seed.Join) { j.EnrollKey = "naru ek" },
		"a bad name":       func(j *seed.Join) { j.Name = "Rack_3" },
	} {
		t.Run(name, func(t *testing.T) {
			j := good
			mutate(&j)
			_, err := j.ISO()
			assert.Error(t, err)
		})
	}
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
