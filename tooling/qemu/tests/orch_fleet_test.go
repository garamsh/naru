//go:build qemu_real

package tests_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/seed"
)

// The orch scenarios: three control hosts that run the signer and orchd as one cluster, and hosts that
// enroll and are admitted (docs/architecture/orch.md, signer.md). Each test is named for the criterion
// it covers.

// get is a request to the orch of control Node i as root, over mutual TLS: the status and the body.
func (f *fleet) get(i int, method, path string) (int, []byte) {
	f.t.Helper()
	pair, err := tls.LoadX509KeyPair(f.rootCertFile, f.rootKey)
	require.NoError(f.t, err)
	pool := x509.NewCertPool()
	require.True(f.t, pool.AppendCertsFromPEM([]byte(f.rootCert)))
	cl := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{pair}, RootCAs: pool, MinVersion: tls.VersionTLS13,
	}}}
	req, err := http.NewRequest(method, f.orchURL(i)+path, nil)
	require.NoError(f.t, err)
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

type hostJSON struct {
	ID       string            `json:"id"`
	Region   string            `json:"region"`
	Labels   map[string]string `json:"labels"`
	LastSeen time.Time         `json:"lastSeen"`
}

func (f *fleet) hostList(i int) []hostJSON {
	code, b := f.get(i, http.MethodGet, "/v1/hosts")
	if code != http.StatusOK {
		return nil
	}
	var hs []hostJSON
	_ = json.Unmarshal(b, &hs)
	return hs
}

type groupJSON struct {
	Spec struct {
		Count int `json:"count"`
	} `json:"spec"`
	Hash     string `json:"hash"`
	Observed bool   `json:"observed"`
	Members  []struct {
		ID           string `json:"id"`
		Host         string `json:"host"`
		TemplateHash string `json:"templateHash"`
		Phase        string `json:"phase"`
	} `json:"members"`
}

func (f *fleet) group(i int, name string) (groupJSON, bool) {
	code, b := f.get(i, http.MethodGet, "/v1/groups/"+name)
	var g groupJSON
	return g, code == http.StatusOK && json.Unmarshal(b, &g) == nil
}

// running is the members of the group that run, by host ID.
func (g groupJSON) running() map[string]int {
	out := map[string]int{}
	for _, m := range g.Members {
		if m.Phase == "running" {
			out[m.Host]++
		}
	}
	return out
}

func (g groupJSON) count(phase string) int {
	n := 0
	for _, m := range g.Members {
		if m.Phase == phase {
			n++
		}
	}
	return n
}

// groupFile is the group's spec as the operator's file, with the knobs the scenarios change.
func (f *fleet) groupFile(name string, count, memoryMiB int, extra string) string {
	return f.file(fmt.Sprintf("%s-%d-%d.yaml", name, count, memoryMiB), fmt.Sprintf(`name: %s
count: %d
region: eu-1
template:
  form: image
  cpus: 1
  memoryMiB: %d
  version: %s@%s
  rootDiskBytes: 1073741824
rollout: {maxUnavailable: 1, maxSurge: 1}
minAvailable: %d
%s`, name, count, memoryMiB, inputs.Image.Repository, inputs.Image.Digest, max(count-1, 0), extra))
}

func TestFleet_OrchLeaderLossElectsAnotherAndEnrollmentKeepsTheRegionLabelsAndTheCodeIsTheConsoles(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 3, hosts: 1})
	f.startOrch()
	h := f.hosts()[0]

	// Enrollment: the pairing code orch lists as pending is the code the host's own console shows, and
	// the admission records the region and the labels.
	key := f.enrollmentKey()
	code := f.enroll(h, key)
	id := f.hostID(code)
	require.NotEmpty(t, id)
	assert.Contains(t, f.must(f.orch("host", "pending"), "host pending"), code, "orch lists the code the console shows")
	f.admit(code, "eu-1", "disk=ssd", "rack=r7")
	require.NoError(t, f.node(h).WaitFor(t.Context(), bootTimeout, "admitted"), "the host says it was admitted")
	eventually(t, "orch lists the host with its region and labels", func() bool {
		for _, hs := range f.hostList(1) {
			if hs.ID == id {
				return hs.Region == "eu-1" && hs.Labels["disk"] == "ssd" && hs.Labels["rack"] == "r7"
			}
		}
		return false
	})

	// Killing the leader elects another, which keeps the host and the enrollment.
	var leader int
	eventually(t, "a member leads", func() bool { leader = f.leader(f.controls()); return leader != 0 })
	f.node(leader).Stop()
	var alive []int
	for _, i := range f.controls() {
		if i != leader {
			alive = append(alive, i)
		}
	}
	var next int
	eventually(t, "another member leads", func() bool { next = f.leader(alive); return next != 0 && next != leader })
	eventually(t, "the new leader keeps the host's region and labels", func() bool {
		for _, hs := range f.hostList(next) {
			if hs.ID == id {
				return hs.Region == "eu-1" && hs.Labels["disk"] == "ssd"
			}
		}
		return false
	})
	f.must(f.orchAs(f.root, next, "enroll-key", "create"), "the surviving cluster still takes a write")
}

func TestFleet_RolesAndOneRevocationListAcrossTheReplicas(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 3})
	f.startOrch()
	op, rd := f.operator("op", "operator"), f.operator("rd", "reader")

	// Before: an operator writes and a reader reads, on every member's orch.
	for _, i := range f.controls() {
		eventually(t, fmt.Sprintf("the operator is admitted by member %d", i), func() bool { return f.orchAs(op, i, "enroll-key", "create").Err == nil })
		eventually(t, fmt.Sprintf("the reader reads on member %d", i), func() bool { return f.orchAs(rd, i, "host", "pending").Err == nil })
		assert.Error(t, f.orchAs(rd, i, "enroll-key", "create").Err, "a reader does not write on member %d", i)
	}

	// Root revokes both at the first signer; the list reaches the other signers through orch's sync.
	for _, p := range []person{op, rd} {
		f.must(nrun("operator", "revoke", "--admin", f.root.file, "--passphrase-file", f.pass, "--serial", p.serial, "--socket", f.socks[1]), "revoke "+p.serial)
	}
	for _, i := range f.controls() {
		eventually(t, fmt.Sprintf("member %d refuses the revoked operator", i), func() bool { return f.orchAs(op, i, "enroll-key", "create").Err != nil })
		eventually(t, fmt.Sprintf("member %d refuses the revoked reader", i), func() bool { return f.orchAs(rd, i, "host", "pending").Err != nil })
		f.must(f.orchAs(f.root, i, "host", "pending"), fmt.Sprintf("root still reads on member %d", i))
	}
}

var keyProbeLine = regexp.MustCompile(`naru96-keyprobe: scanned=(\d+) found=(\S+)`)

// probed is what the latest naru-keyprobe line of the Node says: the files scanned and the paths found.
func probed(console string) (scanned int, found string) {
	all := keyProbeLine.FindAllStringSubmatch(console, -1)
	if len(all) == 0 {
		return 0, ""
	}
	last := all[len(all)-1]
	scanned, _ = strconv.Atoi(last[1])
	return scanned, last[2]
}

func TestFleet_NoUnencryptedAuthorityKeyLiesOutsideTheSignersDirectory(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 3})
	f.startOrch()

	// The probe looks for the private keys of root and of the revocation list (the recovery key's public
	// half is a JSON document, and its private half is never on a Node here), and for a throwaway key the test plants on the last control host, which is the positive control.
	planted, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(planted)
	require.NoError(t, err)
	plantedKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	pub, err := x509.MarshalPKIXPublicKey(&planted.PublicKey)
	require.NoError(t, err)
	public := f.rootCert + f.revPub + string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}))
	last := f.controls()[len(f.controls())-1]
	const plantedPath = "/run/harness/planted.key"

	for _, i := range f.controls() {
		f.edit(i, func(file string) error {
			if err := seed.Append(file, map[string]any{"path": "/run/harness/public.pem", "content": public}, "spec", "files"); err != nil {
				return err
			}
			if i == last {
				if err := seed.Append(file, map[string]any{"path": plantedPath, "mode": "0600", "content": plantedKey}, "spec", "files"); err != nil {
					return err
				}
			}
			return seed.Append(file, map[string]any{"name": "naru96-keyprobe", "binary": keyProbe, "args": []string{"--public-pem", "/run/harness/public.pem"}}, "spec", "services")
		})
	}
	for _, i := range f.controls() {
		nd := f.node(i)
		eventually(t, nd.Name+" has scanned its file systems", func() bool { n, _ := probed(nd.Console()); return n > 0 })
		_, found := probed(nd.Console())
		if i == last {
			assert.Equal(t, plantedPath, found, "the probe finds the planted key and nothing else on %s", nd.Name)
		} else {
			assert.Equal(t, "none", found, "no authority key is outside the signer's directory on %s", nd.Name)
		}
	}
}

// snapshot is the newest sealed etcd snapshot orch keeps, as the leader's member serves it.
func (f *fleet) snapshot(i int) string {
	var body []byte
	eventually(f.t, "orch serves its sealed snapshot", func() bool {
		code, b := f.get(i, http.MethodGet, "/v1/snapshots/latest")
		body = b
		return code == http.StatusOK && len(b) > 0
	})
	return f.file("orch.snapshot", string(body))
}

// lose is control Node i after its disk was lost: a new data disk, and the first user-data without the
// parts that the first boot made (spec.orch, the signer's bootstrap or join). The Node 1 signer starts
// empty, to be restored; the others start as replicas.
func (f *fleet) lose(i int, extraFiles ...map[string]any) {
	nd := f.node(i)
	file := userDataOf(nd)
	require.NoError(f.t, seed.Delete(file, "spec", "orch"))
	require.NoError(f.t, seed.Delete(file, "spec", "signer", "bootstrap"))
	require.NoError(f.t, seed.Delete(file, "spec", "signer", "replica"))
	if i > 1 {
		require.NoError(f.t, seed.Set(file, map[string]any{}, "spec", "signer", "replica"))
	}
	for _, e := range extraFiles {
		require.NoError(f.t, seed.Append(file, e, "spec", "files"))
	}
	ud, err := os.ReadFile(file)
	require.NoError(f.t, err)
	nc, err := os.ReadFile(filepath.Join(filepath.Dir(file), "network-config"))
	require.NoError(f.t, err)
	require.NoError(f.t, nd.ReplaceUserData(ud, nc))
	require.NoError(f.t, nd.ResetData(inputs.Dir))
}

func TestFleet_RestoresTheControlHostsFromTheSignersBundleAndOrchsSealedSnapshot(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 3, hosts: 1})
	f.startOrch()
	hostIDs := f.enrollAndAdmit("eu-1", "disk=ssd")
	f.must(f.orch("group", "apply", "-f", f.groupFile("keep", 1, 256, "")), "group apply")
	eventually(t, "the group's member runs", func() bool { g, ok := f.group(1, "keep"); return ok && g.count("running") == 1 })

	// Taken before the loss: the signer's bundle, and the sealed snapshot of orch.
	bundle := filepath.Join(f.dir, "signer.bundle")
	f.must(nrun("signer", "bundle", "--admin", f.root.file, "--passphrase-file", f.pass, "--socket", f.socks[1], "--out", bundle), "signer bundle")
	f.must(f.orch("orch", "backup"), "orch backup")
	snap := f.snapshot(1)

	// Every control host loses its disk. The recovery key and its passphrase go to the first one as
	// files, for the signer's restore, which reads them on the signer's host.
	recKey, err := os.ReadFile(f.rec.key)
	require.NoError(t, err)
	pass, err := os.ReadFile(f.pass)
	require.NoError(t, err)
	for _, i := range f.controls() {
		if i == 1 {
			f.lose(i,
				map[string]any{"path": "/run/harness/recovery.key", "mode": "0600", "content": string(recKey)},
				map[string]any{"path": "/run/harness/recovery.pass", "mode": "0600", "content": string(pass)})
		} else {
			f.lose(i)
		}
	}
	eventually(t, "the signer restores from the bundle with the recovery key", func() bool {
		return nrun("signer", "restore", "--admin", f.root.file, "--passphrase-file", f.pass, "--bundle", bundle,
			"--recovery-key", "/run/harness/recovery.key", "--recovery-passphrase-file", "/run/harness/recovery.pass", "--socket", f.socks[1]).Err == nil
	})

	// The first host's orch starts empty with spec.orch.restore, alone in its cluster, and takes the
	// snapshot pushed by the operator, who decrypts it locally.
	crt := f.issueOrch(1)
	f.edit(1, func(file string) error {
		spec := f.orchSpec(1, []int{1}, "new", crt)
		spec["restore"] = true
		return seed.Set(file, spec, "spec", "orch")
	})
	eventually(t, "the empty orch takes the snapshot", func() bool {
		return nrun("orch", "restore", "--in", snap, "--recovery-key", f.rec.key, "--passphrase-file", f.pass,
			"--push", "https://"+f.forward(1, orchPort), "--orch-ca", f.rootCertFile, "--admin", f.root.file).Err == nil
	})

	// The other two join as new members, their signers as replicas of the restored one.
	auth := map[int]string{}
	for _, i := range f.controls()[1:] {
		auth[i] = f.authorise(i)
	}
	for _, i := range f.controls()[1:] {
		crt := f.issueOrch(i)
		f.edit(i, func(file string) error {
			if err := f.joinSigner(file, auth[i]); err != nil {
				return err
			}
			return seed.Set(file, f.orchSpec(i, f.controls(), "existing", crt), "spec", "orch")
		})
	}
	for _, i := range f.controls() {
		eventually(t, fmt.Sprintf("restored member %d serves the group and the host", i), func() bool {
			g, ok := f.group(i, "keep")
			return ok && g.Spec.Count == 1 && len(f.hostList(i)) == 1
		})
	}
	// The host reconnects with the certificate it had: orch records a heartbeat after the restore.
	var before time.Time
	for _, hs := range f.hostList(1) {
		before = hs.LastSeen
	}
	eventually(t, "the admitted host heartbeats to the restored orch", func() bool {
		for _, hs := range f.hostList(1) {
			if _, ok := hostIDs[f.hosts()[0]]; ok && hs.ID == hostIDs[f.hosts()[0]] {
				return hs.LastSeen.After(before)
			}
		}
		return false
	})
	assert.Contains(t, strings.TrimSpace(f.must(f.orch("group", "get", "keep"), "group get")), "keep")
}

func TestFleet_GroupsKeepTheCountRollDrainReplaceOnALabelChangeAndFenceAStatefulMember(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 2})
	f.startOrch()
	key := f.enrollmentKey()
	ids := map[int]string{}
	for _, i := range f.hosts() {
		ids[i] = f.admit(f.enroll(i, key), "eu-1", "tier=a")
	}
	h1, h2 := f.hosts()[0], f.hosts()[1]

	// The count is kept: three members run, and a member lost to a delete on its host is made again.
	f.must(f.orch("group", "apply", "-f", f.groupFile("web", 3, 256, "")), "apply web")
	eventually(t, "three members run", func() bool { g, ok := f.group(1, "web"); return ok && g.count("running") == 3 })

	// A template change replaces the members one at a time and never takes more than maxUnavailable.
	f.must(f.orch("group", "apply", "-f", f.groupFile("web", 3, 320, "")), "apply web again")
	var lowest = 3
	eventually(t, "every member runs the new template", func() bool {
		g, ok := f.group(1, "web")
		if !ok {
			return false
		}
		lowest = min(lowest, g.count("running"))
		n := 0
		for _, m := range g.Members {
			if m.Phase == "running" && m.TemplateHash == g.Hash {
				n++
			}
		}
		return n == 3
	})
	assert.GreaterOrEqual(t, lowest, 2, "a rolling replace keeps at least count minus maxUnavailable running")

	// A smaller count drains the surplus member and leaves the others running.
	f.must(f.orch("group", "apply", "-f", f.groupFile("web", 2, 320, "")), "apply web smaller")
	eventually(t, "the surplus member is gone and two run", func() bool {
		g, ok := f.group(1, "web")
		return ok && g.count("running") == 2 && g.count("draining") == 0
	})
	f.must(f.orch("group", "apply", "-f", f.groupFile("web", 0, 320, "")), "apply web to none")
	eventually(t, "no member runs", func() bool { g, ok := f.group(1, "web"); return ok && g.count("running") == 0 })

	// A label change re-places the member: it requires tier=a and sits on the host that carries it, and
	// when the labels swap it moves to the other host.
	f.must(f.orch("host", "label", ids[h2], "--region", "eu-1", "--label", "tier=b"), "label the second host b")
	f.must(f.orch("group", "apply", "-f", f.groupFile("placed", 1, 256, "requiredLabels: {tier: a}\n")), "apply placed")
	eventually(t, "the member runs on the host labelled a", func() bool { g, ok := f.group(1, "placed"); return ok && g.running()[ids[h1]] == 1 })
	f.must(f.orch("host", "label", ids[h1], "--region", "eu-1", "--label", "tier=b"), "label the first host b")
	f.must(f.orch("host", "label", ids[h2], "--region", "eu-1", "--label", "tier=a"), "label the second host a")
	eventually(t, "the member is re-placed on the host that now carries a", func() bool {
		g, ok := f.group(1, "placed")
		return ok && g.running()[ids[h2]] == 1 && g.running()[ids[h1]] == 0
	})
	f.must(f.orch("group", "apply", "-f", f.groupFile("placed", 0, 256, "requiredLabels: {tier: a}\n")), "apply placed to none")

	// A stateful member is fenced: it runs on the host labelled a; when that host stops answering the
	// member waits for the fence to lapse, and only then is made on the other host.
	f.must(f.orch("group", "apply", "-f", f.groupFile("db", 1, 256, "requiredLabels: {tier: a}\nstateful: true\nfenceTTLSeconds: 30\n")), "apply db")
	eventually(t, "the stateful member runs on the host labelled a", func() bool { g, ok := f.group(1, "db"); return ok && g.running()[ids[h2]] == 1 })
	f.node(h2).Stop()
	eventually(t, "the member waits for its fence", func() bool { g, ok := f.group(1, "db"); return ok && g.count("fenced-waiting") == 1 })
	f.must(f.orch("host", "label", ids[h1], "--region", "eu-1", "--label", "tier=a"), "label the first host a")
	eventually(t, "the member runs on the other host once the fence lapsed", func() bool {
		g, ok := f.group(1, "db")
		return ok && g.running()[ids[h1]] == 1
	})
}
