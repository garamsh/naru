package apply

import (
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

func TestSecretKinds_TheFixtureHoldsAValueInEveryKindOfTheTable(t *testing.T) {
	cfg := everySecret()

	for i, k := range secretKinds {
		places := k.places(&cfg)
		require.NotEmpty(t, places, "kind %d of the table has no place in the fixture, so the tests below do not reach it", i)
		for _, s := range places {
			assert.NotEmpty(t, *s.value, s.field)
		}
	}
}

func TestRedact_HidesEveryPlaceTheTableListsAndLeavesTheRest(t *testing.T) {
	cfg := everySecret()

	got := Redact(cfg)

	assert.Equal(t, fieldsOf(cfg), fieldsOf(got))
	for _, s := range secrets(&got) {
		assert.Equal(t, Redacted, *s.value, s.field)
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
		assert.Empty(t, *s.value, s.field)
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
