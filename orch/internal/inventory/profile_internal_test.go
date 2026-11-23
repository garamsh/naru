package inventory

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/layout"
)

func TestShownKeepsOnlyWhatIsListedAndRedactsEveryPassword(t *testing.T) {
	const secret = "s3cret-value"
	p := layout.HostProfile{
		FirecrackerSHA256: "abc",
		MachinePool:       netip.MustParsePrefix("10.200.0.0/16"),
		Registries: []layout.Registry{
			{Host: "r.example", Username: "robot", Password: secret, CA: "ca-pem"},
			{Host: "ecr.example", Helper: "ecr-login"},
		},
	}
	got := shown(p)
	require.Equal(t, []layout.Registry{
		{Host: "r.example", Username: "robot", Password: layout.Redacted, CA: "ca-pem"},
		{Host: "ecr.example", Helper: "ecr-login"},
	}, got.Registries)
	b, err := got.Marshal()
	require.NoError(t, err)
	require.False(t, strings.Contains(string(b), secret))
	require.Equal(t, secret, p.Registries[0].Password, "the stored profile is not altered")
}

// A field the profile gains must be listed in shown on purpose: this fails until it is, and the
// person who adds the field decides whether it may be shown.
func TestEveryFieldOfTheProfileIsDecidedForShown(t *testing.T) {
	var shownFields = map[string]bool{
		"FirecrackerSHA256": true, "TimeServers": true, "Registries": true, "AWS": true,
		"MetricsListen": true, "Network": true, "Storage": true, "Extensions": true, "Orch": true,
		"APIClientCA": true, "MachinePool": true,
	}
	typ := reflect.TypeFor[layout.HostProfile]()
	for i := range typ.NumField() {
		require.True(t, shownFields[typ.Field(i).Name], "HostProfile.%s is not decided for shown: list it there, or leave it out on purpose", typ.Field(i).Name)
	}
	require.Equal(t, len(shownFields), typ.NumField())
}
