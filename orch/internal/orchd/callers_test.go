package orchd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

func TestOrch_GrantsACallerAliasDomainsThatAreDNSNamesApartFromTheZone(t *testing.T) {
	o := startMachineOrch(t)
	ctx := t.Context()
	alice := o.c.CA.Issue(t, "alice", identity.RoleOperator)
	op := o.c.Client(t, 0, &alice)

	got, err := op.ApplyCaller(ctx, "garam", orchapi.CallerSpec{AliasDomains: []string{"b.example", "apps.garam.example", "b.example"}})
	require.NoError(t, err)
	want := orchapi.Caller{Name: "garam", AliasDomains: []string{"apps.garam.example", "b.example"}}
	assert.Equal(t, want, got, "sorted, each once")
	bob := o.c.CA.Issue(t, "bob", identity.RoleReader)
	list, err := o.c.Client(t, 0, &bob).Callers(ctx)
	require.NoError(t, err)
	assert.Equal(t, []orchapi.Caller{want}, list, "a reader lists the grants")

	for domain, what := range map[string]string{
		"example":          "one label",
		"Apps.example":     "a capital",
		"a..example":       "an empty label",
		machineZone:        "the zone",
		"x." + machineZone: "a name in the zone",
		"example.test":     "a domain the zone is in",
	} {
		_, err := op.ApplyCaller(ctx, "garam", orchapi.CallerSpec{AliasDomains: []string{"c.example", domain}})
		assert.Equal(t, http.StatusBadRequest, status(err), "%s: %v", what, err)
		if err != nil {
			assert.Contains(t, err.Error(), "aliasDomains[1]", what)
		}
	}
	_, err = op.ApplyCaller(ctx, "Garam", orchapi.CallerSpec{})
	assert.Equal(t, http.StatusBadRequest, status(err), "a caller's name is a DNS label")
	_, err = o.c.Client(t, 0, &bob).ApplyCaller(ctx, "garam", orchapi.CallerSpec{})
	assert.Equal(t, http.StatusForbidden, status(err), "a reader grants nothing")

	list, err = op.Callers(ctx)
	require.NoError(t, err)
	assert.Equal(t, []orchapi.Caller{want}, list, "no refused grant replaced the one kept")
}
