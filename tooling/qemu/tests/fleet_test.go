//go:build qemu_real

package tests_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/consolepem"
	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/seed"
	"github.com/garamsh/naru/tooling/qemu/internal/unixfwd"
)

// The fleet is the control hosts and the hosts of the multi-host scenarios (docs/architecture/harness.md,
// "Fleets"): control Nodes 1..controls run the signer and orchd, the Nodes after them run naru-hostd,
// and every Node is on the QEMU multicast network at 10.78.0.<n>. The scenarios drive the product the
// way an operator does: the keys are throwaway ones the test makes with naructl, the root certificate
// and the revocation key are read from the signer's console, orch's pair is issued by root through the
// signer's socket, and the hosts enroll with an enrollment key and are admitted with the pairing code
// their console shows.

// The ports of the fleet's services.
const (
	orchPort       = "7500"
	orchPeerPort   = "7501"
	orchClientPort = "7502"
	signerPeerPort = "7600"
	sockProxyPort  = "7700"
)

// metricsPorts are the fixed ports of init, naru-hostd, naru-orchd and naru-signer.
var metricsPorts = []string{"9100", "9101", "9102", "9103"}

// The management prefix of the QEMU network, which every host's spec.host.network names.
const managementPrefix = "10.78.0.0/24"

var (
	sockProxy = path.Join(fixture.GuestRoot, "bin/naru-sockproxy")
	nftProbe  = path.Join(fixture.GuestRoot, "bin/naru-nftprobe")
	keyProbe  = path.Join(fixture.GuestRoot, "bin/naru-keyprobe")
)

type fleetOpts struct {
	controls, hosts int
	// capture records the shared network at every Node.
	capture bool
	// services are added to every Node.
	services []node.ExtraService
}

type fleet struct {
	t     *testing.T
	c     *node.Cluster
	dir   string
	opts  fleetOpts
	pass  string
	root  person
	rec   recovery
	csrs  map[int]string // orch's request of each control Node
	keys  map[int]string // and its key
	socks map[int]string // the signer's socket of each control Node, forwarded on the test host
	// rootCert is the first root certificate, PEM, as the signer logged it; revPub the revocation key.
	rootCert, revPub string
	rootCertFile     string
	caSHA            string
	rootKey, rootCSR string
}

type recovery struct{ key, pub string }

// person is an operator's person file and the serial of the certificate in it.
type person struct {
	file   string
	serial string
}

func (f *fleet) controls() []int { return seq(1, f.opts.controls) }
func (f *fleet) hosts() []int    { return seq(f.opts.controls+1, f.opts.controls+f.opts.hosts) }

func seq(from, to int) []int {
	var s []int
	for i := from; i <= to; i++ {
		s = append(s, i)
	}
	return s
}

func nodeIP(i int) string { return fmt.Sprintf("10.78.0.%d", i) }

func (f *fleet) node(i int) *node.Node { return f.c.Nodes[i-1] }

// forward is the loopback address of the test host that reaches the guest port of Node i's second NIC.
func (f *fleet) forward(i int, port string) string { return f.c.Forwards[f.node(i).Name][port] }

func (f *fleet) must(res result, what string) string {
	f.t.Helper()
	require.NoError(f.t, res.Err, what+": "+strings.TrimSpace(res.Stderr+res.Stdout))
	return res.Stdout
}

func nrun(args ...string) result {
	r := tool.Run(args...)
	return result{Stdout: r.Stdout, Stderr: r.Stderr, Err: r.Err}
}

func (f *fleet) file(name, content string) string {
	f.t.Helper()
	p := filepath.Join(f.dir, name)
	require.NoError(f.t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func userDataOf(nd *node.Node) string {
	return filepath.Join(filepath.Dir(nd.ConsolePath()), "naru.yaml")
}

// newFleet makes the keys, boots every Node with its first user-data (the signer of the control Nodes
// and spec.host.network of the hosts, with metrics on the second NIC) and has root's certificate read
// from the console of the first signer, so that root can act.
func newFleet(t *testing.T, o fleetOpts) *fleet {
	t.Helper()
	f := &fleet{t: t, dir: t.TempDir(), opts: o, csrs: map[int]string{}, keys: map[int]string{}, socks: map[int]string{}}
	secret := make([]byte, 16)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	f.pass = f.file("passphrase", hex.EncodeToString(secret)+"\n")

	f.rootKey, f.rootCSR = filepath.Join(f.dir, "root.key"), filepath.Join(f.dir, "root.csr")
	f.must(nrun("operator", "request", "--name", "root", "--key-out", f.rootKey, "--csr-out", f.rootCSR), "root's request")
	recDir := filepath.Join(f.dir, "recovery")
	f.must(nrun("orch", "init", "--out-dir", recDir, "--passphrase-file", f.pass), "orch init")
	f.rec = recovery{key: filepath.Join(recDir, "recovery.key"), pub: filepath.Join(recDir, "recovery.pub")}
	for _, i := range f.controls() {
		f.keys[i], f.csrs[i] = filepath.Join(f.dir, fmt.Sprintf("orch-%d.key", i)), filepath.Join(f.dir, fmt.Sprintf("orch-%d.csr", i))
		f.must(nrun("orch", "csr", "--name", orchName(i), "--dns", orchName(i)+".naru.test", "--ip", "127.0.0.1,"+nodeIP(i)+","+node.HostNICAddress,
			"--key-out", f.keys[i], "--csr-out", f.csrs[i]), "orch's request")
	}

	services := append([]node.ExtraService{{Name: "naru95-sockproxy", Binary: sockProxy, Args: []string{"0.0.0.0:" + sockProxyPort, "/run/naru-signer/signer.sock"}}}, o.services...)
	forward := append([]string{orchPort, sockProxyPort}, metricsPorts...)
	c := start(t, node.ClusterSpec{
		N: o.controls + o.hosts, Gaon: true, NodeAPI: true, Host: true, Hosts: f.hosts(), Capture: o.capture,
		Disable: []string{"etcd", "gaon-apiserver", "gaon-scheduler", "gaon-controllers", "gaon-node-agent"},
		Forward: forward, MemoryMiB: 2048, EditUserData: f.firstUserData, ExtraServices: services,
	})
	f.c = c

	// The signer logs its first root certificate and the revocation key on the console; the operator
	// takes them from there.
	first := f.node(1)
	eventually(t, "the first signer logs root's certificate and the revocation key", func() bool {
		return len(consolepem.Extract(first.Console(), "CERTIFICATE")) > 0 && len(consolepem.Extract(first.Console(), "PUBLIC KEY")) > 0
	})
	f.rootCert = consolepem.Extract(first.Console(), "CERTIFICATE")[0]
	f.revPub = consolepem.Extract(first.Console(), "PUBLIC KEY")[0]
	f.rootCertFile = f.file("root.pem", f.rootCert)
	block, _ := pem.Decode([]byte(f.rootCert))
	require.NotNil(t, block, "root's certificate is PEM")
	sum := sha256.Sum256(block.Bytes)
	f.caSHA = hex.EncodeToString(sum[:])
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	f.root = f.personOf("root", f.rootKey, f.rootCertFile, cert)

	for _, i := range f.controls() {
		sock := filepath.Join(f.dir, fmt.Sprintf("signer-%d.sock", i))
		stop, err := unixfwd.Listen(sock, f.forward(i, sockProxyPort))
		require.NoError(t, err)
		t.Cleanup(stop)
		f.socks[i] = sock
	}
	return f
}

func orchName(i int) string { return fmt.Sprintf("orch-%d", i) }

func (f *fleet) personOf(name, key, certFile string, cert *x509.Certificate) person {
	out := filepath.Join(f.dir, name+".person")
	f.must(nrun("operator", "person", "--key", key, "--cert", certFile, "--passphrase-file", f.pass, "--out", out), "person file of "+name)
	return person{file: out, serial: cert.SerialNumber.Text(16)}
}

// firstUserData is ClusterSpec.EditUserData: the signer of the control Nodes, the host's network, the metrics.
func (f *fleet) firstUserData(c node.Config, file string) error {
	if err := seed.Set(file, map[string]any{"listen": node.HostNICAddress}, "spec", "metrics"); err != nil {
		return err
	}
	if c.Index > f.opts.controls {
		return seed.Set(file, map[string]any{
			"managementPrefix": managementPrefix, "hostAddrs": []string{nodeIP(c.Index), node.HostNICAddress}, "tunnelPort": 51820,
		}, "spec", "host", "network")
	}
	var names []string
	for _, i := range f.controls() {
		names = append(names, fmt.Sprintf("node-%d", i))
	}
	signer := map[string]any{"peerListen": nodeIP(c.Index) + ":" + signerPeerPort, "peerNames": names}
	if c.Index == 1 {
		pub, err := os.ReadFile(f.rec.pub)
		if err != nil {
			return err
		}
		csr, err := os.ReadFile(f.rootCSR)
		if err != nil {
			return err
		}
		signer["bootstrap"] = map[string]any{"recoveryPublicKey": string(pub), "rootCSR": string(csr)}
	} else {
		signer["replica"] = map[string]any{}
	}
	return seed.Set(file, signer, "spec", "signer")
}

// edit changes Node i's user-data and boots it again with it: the operator's edit of the spec in the
// hypervisor's console and a restart, which stands in for naructl apply on a Node whose Node API the
// harness does not forward.
func (f *fleet) edit(i int, change func(file string) error) {
	f.t.Helper()
	nd := f.node(i)
	file := userDataOf(nd)
	require.NoError(f.t, change(file))
	ud, err := os.ReadFile(file)
	require.NoError(f.t, err)
	nc, err := os.ReadFile(filepath.Join(filepath.Dir(file), "network-config"))
	require.NoError(f.t, err)
	require.NoError(f.t, nd.ReplaceUserData(ud, nc))
	require.NoError(f.t, nd.Reboot())
}

var base64Token = regexp.MustCompile(`[A-Za-z0-9+/=]{40,}`)

// joinKeyOn is the public join key a replica signer logged on its first boot: the longest base64
// token of the console line that names a join key.
func joinKeyOn(console string) string {
	best := ""
	for _, line := range strings.Split(console, "\n") {
		low := strings.ToLower(line)
		if !strings.Contains(low, "join") || !strings.Contains(low, "key") {
			continue
		}
		for _, tok := range base64Token.FindAllString(line, -1) {
			if len(tok) > len(best) {
				best = tok
			}
		}
	}
	return best
}

// orchSpec is spec.orch of control Node i in a cluster of the members: root's certificate for its pair,
// the authority, the revocation key and the recovery key's public half.
func (f *fleet) orchSpec(i int, members []int, state string, crt string) map[string]any {
	var cluster []string
	for _, m := range members {
		cluster = append(cluster, fmt.Sprintf("%s=https://%s:%s", orchName(m), nodeIP(m), orchPeerPort))
	}
	key, err := os.ReadFile(f.keys[i])
	require.NoError(f.t, err)
	recPub, err := os.ReadFile(f.rec.pub)
	require.NoError(f.t, err)
	return map[string]any{
		"name": orchName(i), "peerURL": "https://" + nodeIP(i) + ":" + orchPeerPort, "clientURL": "https://" + nodeIP(i) + ":" + orchClientPort,
		"initialCluster": strings.Join(cluster, ","), "initialClusterState": state, "listen": "0.0.0.0:" + orchPort,
		"sans": []string{"127.0.0.1", nodeIP(i), node.HostNICAddress}, "certificate": crt, "key": string(key), "ca": f.rootCert,
		"revocationPublicKey": f.revPub, "recoveryPublicKey": string(recPub),
	}
}

// issueOrch has root issue orch's certificate for control Node i through the first signer's socket.
func (f *fleet) issueOrch(i int) string {
	out := filepath.Join(f.dir, orchName(i)+".crt")
	eventually(f.t, "root has the signer issue "+orchName(i)+"'s certificate", func() bool {
		return nrun("orch", "issue", "--admin", f.root.file, "--passphrase-file", f.pass, "--csr", f.csrs[i], "--name", orchName(i), "--socket", f.socks[1], "--out", out).Err == nil
	})
	b, err := os.ReadFile(out)
	require.NoError(f.t, err)
	return string(b)
}

// authorise has root authorise the signer of replica Node i from the join key its first boot logged,
// and returns the authorisation text.
func (f *fleet) authorise(i int) string {
	nd := f.node(i)
	eventually(f.t, nd.Name+"'s signer logs its join key", func() bool { return joinKeyOn(nd.Console()) != "" })
	jk := f.file(fmt.Sprintf("join-%d.key", i), joinKeyOn(nd.Console())+"\n")
	out := filepath.Join(f.dir, fmt.Sprintf("authorisation-%d", i))
	f.must(nrun("signer", "authorise-replica", "--admin", f.root.file, "--passphrase-file", f.pass, "--host-id", nd.Name, "--join-key", jk, "--out", out), "authorise "+nd.Name)
	b, err := os.ReadFile(out)
	require.NoError(f.t, err)
	return string(b)
}

// joinSigner is the patch that has the signer of replica Node i join the first one.
func (f *fleet) joinSigner(file string, authorisation string) error {
	return seed.Set(file, map[string]any{"peer": nodeIP(1) + ":" + signerPeerPort, "authorisation": authorisation}, "spec", "signer", "replica", "join")
}

// startOrch has the replica signers join the first signer, and starts orchd on every control Node as
// one cluster of the members.
func (f *fleet) startOrch() {
	members := f.controls()
	auth := map[int]string{}
	for _, i := range members[1:] {
		auth[i] = f.authorise(i)
	}
	for _, i := range members {
		crt := f.issueOrch(i)
		f.edit(i, func(file string) error {
			if i > 1 {
				if err := f.joinSigner(file, auth[i]); err != nil {
					return err
				}
			}
			return seed.Set(file, f.orchSpec(i, members, "new", crt), "spec", "orch")
		})
	}
	eventually(f.t, "orch answers root on every control Node", func() bool {
		for _, i := range members {
			if f.orchAs(f.root, i, "host", "pending").Err != nil {
				return false
			}
		}
		return true
	})
}

var leadingLine = regexp.MustCompile(`"msg":"leading","member":"([^"]+)"`)

// leader is the number of the control Node among alive whose console shows the latest "leading" line
// of its own member: orchd logs it when its member starts to lead. It is 0 when none does.
func (f *fleet) leader(alive []int) int {
	best, bestAt := 0, -1
	for _, i := range alive {
		cons := f.node(i).Console()
		all := leadingLine.FindAllStringSubmatchIndex(cons, -1)
		if len(all) == 0 {
			continue
		}
		last := all[len(all)-1]
		if cons[last[2]:last[3]] == orchName(i) && last[0] > bestAt {
			best, bestAt = i, last[0]
		}
	}
	return best
}

// operator has root add an operator of the role, and returns its person file.
func (f *fleet) operator(name, role string) person {
	key, csr, crt := filepath.Join(f.dir, name+".key"), filepath.Join(f.dir, name+".csr"), filepath.Join(f.dir, name+".crt")
	f.must(nrun("operator", "request", "--name", name, "--key-out", key, "--csr-out", csr), name+"'s request")
	f.must(nrun("operator", "add", "--admin", f.root.file, "--passphrase-file", f.pass, "--csr", csr, "--name", name, "--role", role, "--socket", f.socks[1], "--out", crt), "add "+name)
	b, err := os.ReadFile(crt)
	require.NoError(f.t, err)
	block, _ := pem.Decode(b)
	require.NotNil(f.t, block, name+"'s certificate is PEM")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(f.t, err)
	return f.personOf(name, key, crt, cert)
}

var (
	pairingCode = regexp.MustCompile(`waits for admission, pairing code (\S+)`)
	secretLine  = regexp.MustCompile(`(?m)^secret\s+(\S+)`)
)

// enroll has host Node i enroll with the enrollment key: its spec.host.orch carries orch's address, the
// authority's SHA-256 and the key. It returns the pairing code the host's console shows.
func (f *fleet) enroll(i int, key string) string {
	f.edit(i, func(file string) error {
		return seed.Set(file, map[string]any{"url": "https://" + nodeIP(1) + ":" + orchPort, "caSHA256": f.caSHA, "enrollKey": key}, "spec", "host", "orch")
	})
	nd := f.node(i)
	eventually(f.t, nd.Name+" shows a pairing code", func() bool { return pairingCode.MatchString(nd.Console()) })
	all := pairingCode.FindAllStringSubmatch(nd.Console(), -1)
	return all[len(all)-1][1]
}

// enrollmentKey has root make an enrollment key and returns its secret.
func (f *fleet) enrollmentKey() string {
	out := f.must(f.orch("enroll-key", "create"), "enroll-key create")
	m := secretLine.FindStringSubmatch(out)
	require.NotNil(f.t, m, out)
	return m[1]
}

// hostID is the ID of the pending host that shows the code, from `naructl host pending`.
func (f *fleet) hostID(code string) string {
	var id string
	eventually(f.t, "orch lists the host with the code pending", func() bool {
		r := f.orch("host", "pending")
		for _, line := range strings.Split(r.Stdout, "\n") {
			if fs := strings.Fields(line); len(fs) > 1 && fs[1] == code {
				id = fs[0]
				return true
			}
		}
		return false
	})
	return id
}

// admit has root admit the host that shows the code, with its region and labels (key=value).
func (f *fleet) admit(code, region string, labels ...string) string {
	id := f.hostID(code)
	args := []string{"host", "admit", id, "--code", code, "--region", region}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	f.must(f.orch(args...), "admit "+id)
	return id
}

// enrollAndAdmit takes every host Node through the enrollment and the admission, in the region with
// the labels, and returns the host IDs by Node number once each host says it was admitted.
func (f *fleet) enrollAndAdmit(region string, labels ...string) map[int]string {
	key := f.enrollmentKey()
	ids := map[int]string{}
	for _, i := range f.hosts() {
		ids[i] = f.admit(f.enroll(i, key), region, labels...)
	}
	for _, i := range f.hosts() {
		nd := f.node(i)
		require.NoError(f.t, nd.WaitFor(context.Background(), bootTimeout, "admitted"), nd.Name+" is admitted")
	}
	return ids
}

func (f *fleet) orchURL(i int) string { return "https://" + f.forward(i, orchPort) }

// orchAs runs naructl as the person against the orch of control Node i.
func (f *fleet) orchAs(p person, i int, args ...string) result {
	return nrun(append(append([]string{}, args...), "--orch-url", f.orchURL(i), "--orch-ca", f.rootCertFile, "--identity", p.file, "--passphrase-file", f.pass)...)
}

func (f *fleet) orch(args ...string) result { return f.orchAs(f.root, 1, args...) }
