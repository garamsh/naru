package orchd_test

import (
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

const joinDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func hardware() *orchapi.Hardware {
	return &orchapi.Hardware{
		CPUs: 4, MemoryBytes: 8 << 30,
		Disks: []orchapi.Disk{{Name: "vda", Size: 100 << 30, Model: "virtio", Partitions: 1}},
		Links: []orchapi.Link{{Name: "eth0", MAC: "52:54:00:12:34:56", Carrier: true}},
	}
}

// seeded is the enrollment of a host that booted from the install seed.
func (h host) seeded(secret string) orchapi.EnrollRequest {
	r := h.request(secret)
	r.Hardware = hardware()
	return r
}

func profileYAML(t *testing.T, pool string, registries ...layout.Registry) []byte {
	t.Helper()
	f := layout.File{Path: "/x", SHA256: joinDigest}
	p := layout.HostProfile{
		Guest: layout.Manifest{
			Kernels: map[string]layout.File{"naru": f}, DefaultKernel: "naru", GuestInitrd: f, ConverterInitrd: f,
		},
		FirecrackerSHA256: joinDigest,
		Orch:              layout.ProfileOrch{URL: "https://orch.example:8443", CASHA256: joinDigest},
		MachinePool:       netip.MustParsePrefix(pool),
		Registries:        registries,
	}
	b, err := p.Marshal()
	require.NoError(t, err)
	return b
}

// admitSeeded enrolls h from the install seed and admits it, and returns the host as the operator sees it.
func (o *one) admitSeeded(t *testing.T, h host) orchapi.Host {
	t.Helper()
	_, err := o.anon.Enroll(t.Context(), h.seeded(o.secret))
	require.NoError(t, err)
	return o.admit(t, h)
}

func (o *one) admit(t *testing.T, h host) orchapi.Host {
	t.Helper()
	got, err := o.op.Admit(t.Context(), orchapi.AdmitRequest{
		HostID: h.id, Code: identity.PairingCode(h.pub), Statement: orchtest.Statement(t, o.operator, h.id, h.pub),
		Region: "kr-seoul", Labels: map[string]string{"disk": "ssd"},
	})
	require.NoError(t, err)
	return got
}

type builtConfig struct {
	Spec map[string]any `yaml:"spec"`
}

func (b builtConfig) files(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	list, _ := b.Spec["files"].([]any)
	for _, f := range list {
		m := f.(map[string]any)
		out[m["path"].(string)], _ = m["content"].(string)
	}
	return out
}

func TestAdmittingAHostFromTheSeedBuildsItsConfigurationAndHandsItOverOnPoll(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	require.NoError(t, o.op.SetProfile(t.Context(), profileYAML(t, "10.200.0.0/16")))
	h := newHost(t, "h-0123456789abcdef")

	_, err := o.anon.Enroll(t.Context(), h.seeded(o.secret))
	require.NoError(t, err)
	pending, err := o.op.Pending(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NotNil(t, pending[0].Hardware, "the operator sees what the host reported beside its pairing code")
	assert.Equal(t, hardware(), pending[0].Hardware)

	st, err := o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	require.NoError(t, err)
	assert.Empty(t, st.Config, "nothing is handed over before the admission")

	o.admit(t, h)
	st, err = o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	require.NoError(t, err)
	require.Equal(t, orchapi.StateAdmitted, st.State)
	require.NotEmpty(t, st.Config)

	var cfg builtConfig
	require.NoError(t, yaml.Unmarshal(st.Config, &cfg))
	for _, section := range []string{"signer", "orch", "join"} {
		assert.NotContains(t, cfg.Spec, section, "the host's configuration is the host's, not orch's or the seed's")
	}
	files := cfg.files(t)
	assert.Equal(t, string(st.Certificate), files["/run/naru-hostd/tls.crt"], "the certificate orch issued is in the configuration")
	assert.Equal(t, string(o.c.CA.PEM()), files["/run/naru-hostd/client-ca.crt"], "naru-hostd trusts the authority orch's callers chain to")
	assert.NotContains(t, string(st.Config), "PRIVATE KEY")
	assert.Contains(t, string(st.Config), "10.200.1.0/24", "the lowest prefix of the pool that is not its first")

	// A host that lost the response enrolls again and is handed the same.
	again, err := o.anon.Enroll(t.Context(), h.seeded(o.secret))
	require.NoError(t, err)
	assert.Equal(t, st.Config, again.Config)
	assert.Equal(t, st.Certificate, again.Certificate)
}

func TestAHostEnrolledWithItsOwnConfigurationIsAdmittedWithoutOne(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	h := newHost(t, "host-a")
	_, err := o.anon.Enroll(t.Context(), h.request(o.secret))
	require.NoError(t, err)
	o.admit(t, h)

	st, err := o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	require.NoError(t, err)
	require.Equal(t, orchapi.StateAdmitted, st.State)
	assert.NotEmpty(t, st.Certificate)
	assert.Empty(t, st.Config)
}

func TestAdmittingAHostFromTheSeedWithNoProfileIsRefusedBeforeTheSignerIsAsked(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	h := newHost(t, "h-0123456789abcdef")
	_, err := o.anon.Enroll(t.Context(), h.seeded(o.secret))
	require.NoError(t, err)

	_, err = o.op.Admit(t.Context(), orchapi.AdmitRequest{HostID: h.id, Code: identity.PairingCode(h.pub), Statement: orchtest.Statement(t, o.operator, h.id, h.pub)})
	assert.Equal(t, http.StatusConflict, status(err))
	assert.Zero(t, o.c.Signer.Calls())
	pending, err := o.op.Pending(t.Context())
	require.NoError(t, err)
	assert.Len(t, pending, 1, "the host is still pending")
}

func TestThePoolGivesTheLowestFreePrefixAnswers409WhenFullAndTakesRemovedHostsBack(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	// A /22 holds four /24s and gives up the first: three are left.
	require.NoError(t, o.op.SetProfile(t.Context(), profileYAML(t, "10.200.0.0/22")))
	var hosts []host
	prefixes := map[string]string{}
	for _, id := range []string{"h-a", "h-b", "h-c"} {
		h := newHost(t, id)
		hosts = append(hosts, h)
		o.admitSeeded(t, h)
		st, err := o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
		require.NoError(t, err)
		for _, p := range []string{"10.200.1.0/24", "10.200.2.0/24", "10.200.3.0/24"} {
			if strings.Contains(string(st.Config), p) {
				prefixes[id] = p
			}
		}
	}
	assert.Equal(t, map[string]string{"h-a": "10.200.1.0/24", "h-b": "10.200.2.0/24", "h-c": "10.200.3.0/24"}, prefixes)
	listed, err := o.op.Hosts(t.Context())
	require.NoError(t, err)
	listedPrefixes := map[string]string{}
	for _, h := range listed {
		listedPrefixes[h.ID] = h.MachinePrefix
	}
	assert.Equal(t, prefixes, listedPrefixes, "GET /v1/hosts carries the prefix each host was given")

	full := newHost(t, "h-d")
	_, err = o.anon.Enroll(t.Context(), full.seeded(o.secret))
	require.NoError(t, err)
	_, err = o.op.Admit(t.Context(), orchapi.AdmitRequest{HostID: full.id, Code: identity.PairingCode(full.pub), Statement: orchtest.Statement(t, o.operator, full.id, full.pub)})
	assert.Equal(t, http.StatusConflict, status(err), "a full pool")

	require.NoError(t, o.op.Remove(t.Context(), "h-b"))
	o.admit(t, full)
	st, err := o.anon.PollEnrollment(t.Context(), full.id, full.key, o.c.Clock.Now())
	require.NoError(t, err)
	assert.Contains(t, string(st.Config), "10.200.2.0/24", "the freed prefix is the lowest free one")
}

func TestRemovingAHostDeletesItsRecordLogsItAndLeavesTheCertificateToBeRevokedByAPerson(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	require.NoError(t, o.op.SetProfile(t.Context(), profileYAML(t, "10.200.0.0/16")))
	h := newHost(t, "h-a")
	o.admitSeeded(t, h)

	assert.Equal(t, http.StatusForbidden, status(o.reader.Remove(t.Context(), h.id)), "a reader may not remove")
	require.NoError(t, o.op.Remove(t.Context(), h.id))
	hosts, err := o.op.Hosts(t.Context())
	require.NoError(t, err)
	assert.Empty(t, hosts)
	assert.Equal(t, http.StatusNotFound, status(o.op.Remove(t.Context(), h.id)))

	events, err := o.reader.Events(t.Context())
	require.NoError(t, err)
	var removed []orchapi.Event
	for _, e := range events {
		if e.Type == inventory.EventRemoved {
			removed = append(removed, e)
		}
	}
	require.Len(t, removed, 1)
	assert.Equal(t, h.id, removed[0].Subject)
	assert.Equal(t, "alice", removed[0].Detail["by"])
}

func TestTheProfileIsReadByAReaderSetByAnOperatorAndRefusedWhenItCarriesSomethingOfOneHost(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	_, err := o.reader.Profile(t.Context())
	assert.Equal(t, http.StatusNotFound, status(err), "no profile yet")

	good := profileYAML(t, "10.200.0.0/16")
	assert.Equal(t, http.StatusForbidden, status(o.reader.SetProfile(t.Context(), good)), "a reader may not set")
	assert.Equal(t, http.StatusBadRequest, status(o.op.SetProfile(t.Context(), append(append([]byte{}, good...), "token: abcdef.0123456789abcdef\n"...))))
	assert.Equal(t, http.StatusBadRequest, status(o.op.SetProfile(t.Context(), []byte("machinePool: 10.0.0.0/8\n"))), "an incomplete profile")
	_, err = o.reader.Profile(t.Context())
	assert.Equal(t, http.StatusNotFound, status(err), "nothing was kept")

	require.NoError(t, o.op.SetProfile(t.Context(), good))
	got, err := o.reader.Profile(t.Context())
	require.NoError(t, err)
	assert.Equal(t, string(good), string(got.Profile), "a profile with no secret reads back as it was set")
	events, err := o.reader.Events(t.Context())
	require.NoError(t, err)
	var set int
	for _, e := range events {
		if e.Type == inventory.EventProfileSet {
			set++
		}
	}
	assert.Equal(t, 1, set)
}

func TestTheProfileNoRoleReadsBackHoldsNoSecretAndOneWithTheRedactionIsRefused(t *testing.T) {
	const password = "hunter2-registry-password"
	o := startOne(t, orchtest.Options{})
	root := o.c.CA.Issue(t, "root", identity.RoleRoot)
	rootClient := o.c.Client(t, 0, &root)
	require.NoError(t, o.op.SetProfile(t.Context(), profileYAML(t, "10.200.0.0/16",
		layout.Registry{Host: "registry.example:5000", Username: "robot", Password: password},
		layout.Registry{Host: "other.example", Helper: "ecr-login"})))

	for name, c := range map[string]*orchapi.Client{"reader": o.reader, "operator": o.op, "root": rootClient} {
		t.Run(name, func(t *testing.T) {
			got, err := c.Profile(t.Context())
			require.NoError(t, err)
			assert.NotContains(t, string(got.Profile), password)
			p, err := layout.ParseHostProfile(got.Profile)
			require.Error(t, err, "what is shown is not a profile that can be set")
			assert.ErrorIs(t, err, layout.ErrRedacted)
			require.Len(t, p.Registries, 2, "the shape stays")
			assert.Equal(t, layout.Registry{Host: "registry.example:5000", Username: "robot", Password: layout.Redacted}, p.Registries[0])
			assert.Equal(t, layout.Registry{Host: "other.example", Helper: "ecr-login"}, p.Registries[1], "no secret, nothing to redact")

			// Setting what was read is refused, and keeps the stored profile.
			assert.Equal(t, http.StatusUnprocessableEntity, status(o.op.SetProfile(t.Context(), got.Profile)))
		})
	}
	// The stored secret is what a host is built from: the refused set changed nothing.
	h := newHost(t, "h-0123456789abcdef")
	_, err := o.anon.Enroll(t.Context(), h.seeded(o.secret))
	require.NoError(t, err)
	o.admit(t, h)
	st, err := o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	require.NoError(t, err)
	assert.Contains(t, string(st.Config), password)
}

func TestARevokedSeedKeyStillRefusesTheHostThatBootedFromIt(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	h := newHost(t, "h-0123456789abcdef")
	_, err := o.anon.Enroll(t.Context(), h.seeded(o.secret))
	require.NoError(t, err, "control: the key works until it is revoked")

	keys, err := o.op.CreateEnrollmentKey(t.Context(), o.c.Clock.Now().Add(time.Hour))
	require.NoError(t, err)
	require.NoError(t, o.op.RevokeEnrollmentKey(t.Context(), keys.ID))
	other := newHost(t, "h-fedcba9876543210")
	_, err = o.anon.Enroll(t.Context(), other.seeded(keys.Secret))
	assert.Equal(t, http.StatusForbidden, status(err))
}
