package apply

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/store"
)

// everySecret is a configuration that holds a value in every place the table lists. It is not
// validated: Redact and refuse read the struct.
func everySecret() config.NodeConfig {
	return config.NodeConfig{Spec: config.Spec{
		API:   &config.API{Key: "api-key"},
		PKI:   config.PKI{Authorities: []config.Authority{{Name: "ca", Key: "ca-key"}, {Name: "other", Key: "other-key"}}},
		Files: []config.File{{Path: "/run/public", Content: "public"}, {Path: "/run/private", Mode: "0600", Content: "private"}},
		Registries: []config.Registry{
			{Host: "r.example", Credential: &config.Credential{Username: "alice", Password: "registry-password"}},
		},
		Host: &config.Host{Orch: &config.HostOrch{EnrollKey: "host-enroll-key"}},
		Join: &config.Join{EnrollKey: "join-enroll-key"},
	}}
}

func storedOf(cfg config.NodeConfig) store.Stored { return store.Stored{Config: cfg} }

func fieldsOf(cfg config.NodeConfig) []string {
	var out []string
	for _, s := range secrets(&cfg) {
		out = append(out, s.field)
	}
	return out
}

func TestSecrets_TheFixtureHoldsAValueInEveryPlaceTheDenyListNamedBefore(t *testing.T) {
	cfg := everySecret()

	got := fieldsOf(cfg)

	assert.ElementsMatch(t, []string{
		"spec.api.key", "spec.pki.authorities[0].key", "spec.pki.authorities[1].key", "spec.files[1].content",
		"spec.registries[0].credential.password", "spec.host.orch.enrollKey", "spec.join.enrollKey",
	}, got)
}

func TestRedact_HidesEveryPlaceTheTableListsAndLeavesTheRest(t *testing.T) {
	cfg := everySecret()

	got := Redact(cfg)

	assert.Equal(t, fieldsOf(cfg), fieldsOf(got))
	for _, s := range secrets(&got) {
		assert.Equal(t, Redacted, s.value.String(), s.field)
	}
	assert.Equal(t, "public", got.Spec.Files[0].Content, "the control: a file group and other can read keeps its content")
	assert.Equal(t, "alice", got.Spec.Registries[0].Credential.Username)
}

func TestRedact_LeavesTheConfigurationItWasGivenAlone(t *testing.T) {
	cfg := everySecret()

	Redact(cfg)

	assert.Equal(t, everySecret(), cfg)
}

func TestRedact_LeavesAnEmptyValueEmpty(t *testing.T) {
	cfg := config.NodeConfig{Spec: config.Spec{API: &config.API{}, Host: &config.Host{Orch: &config.HostOrch{}}}}

	got := Redact(cfg)

	for _, s := range secrets(&got) {
		assert.Empty(t, s.value.String(), s.field)
	}
}

func TestRefuse_NamesEveryPlaceRedactHidesWhenNoneWasStoredRedacted(t *testing.T) {
	cfg := everySecret()
	e := &Engine{stored: storedOf(cfg), running: cfg}

	err := e.refuse(Redact(cfg), Staged)

	var invalid *InvalidError
	require.ErrorAs(t, err, &invalid)
	assert.Equal(t, fieldsOf(cfg), invalid.Fields, "a place Redact hides and refuse does not name would let REDACTED be stored")
}

func TestRefuse_AcceptsTheSamePlaceholdersWhereTheyWereStored(t *testing.T) {
	redacted := Redact(everySecret())
	e := &Engine{stored: storedOf(redacted), running: redacted}

	assert.NoError(t, e.refuse(redacted, Staged), "the control: a placeholder over a stored placeholder is no new one")
}

func TestRefuse_MatchesAListElementByItsKeyAndNotItsIndex(t *testing.T) {
	cfg := everySecret()
	cfg.Spec.PKI.Authorities[0].Key = Redacted // "ca" was stored redacted, "other" was not
	e := &Engine{stored: storedOf(cfg), running: cfg}
	next := Redact(everySecret())
	next.Spec.PKI.Authorities = []config.Authority{next.Spec.PKI.Authorities[1], next.Spec.PKI.Authorities[0]}

	err := e.refuse(next, Staged)

	var invalid *InvalidError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Fields, "spec.pki.authorities[0].key", "other moved to index 0 and was not redacted in the store")
	assert.NotContains(t, invalid.Fields, "spec.pki.authorities[1].key", "ca moved to index 1 and was redacted in the store")
}

// fill sets every string of v, whatever its place, to marker, making each pointer, slice and map.
func fill(v reflect.Value, marker string) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fill(v.Elem(), marker)
	case reflect.String:
		v.SetString(marker)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0), marker)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		e := reflect.New(v.Type().Elem()).Elem()
		fill(e, marker)
		v.SetMapIndex(reflect.ValueOf("k"), e)
	case reflect.Struct:
		if v.Type().PkgPath() != reflect.TypeFor[config.NodeConfig]().PkgPath() {
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), marker)
			}
		}
	}
}

func TestRedact_ShowsOnlyWhatTheAllowListNamesWhateverFieldTheConfigurationHas(t *testing.T) {
	var cfg config.NodeConfig
	fill(reflect.ValueOf(&cfg).Elem(), "marker")
	cfg.Spec.Files[0].Mode = "0600" // a file group and other cannot read: its content is private

	got := Redact(cfg)

	var left []string
	walkAll(reflect.ValueOf(got), "", &left)
	for _, path := range left {
		assert.True(t, shown[path], "%s is shown in clear and is not on the allow-list", path)
	}
	for path := range shown {
		assert.Contains(t, left, path, "%s is on the allow-list and names no string of the configuration: a stale entry", path)
	}
}

// walkAll lists the allow-list path of each string of v that is not Redacted.
func walkAll(v reflect.Value, place string, out *[]string) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			walkAll(v.Elem(), place, out)
		}
	case reflect.String:
		if v.String() != Redacted && !slices.Contains(*out, place) {
			*out = append(*out, place)
		}
	case reflect.Slice:
		for i := range v.Len() {
			walkAll(v.Index(i), place+"[]", out)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			walkAll(it.Value(), place+"{}", out)
		}
	case reflect.Struct:
		if v.Type().PkgPath() != reflect.TypeFor[config.NodeConfig]().PkgPath() {
			return
		}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if name := strings.Split(f.Tag.Get("yaml"), ",")[0]; f.IsExported() && name != "-" {
				walkAll(v.Field(i), place+"."+name, out)
			}
		}
	}
}
