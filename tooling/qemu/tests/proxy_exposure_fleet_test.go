//go:build qemu_real

package tests_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The proxy exposure scenario (#388, ADR 0112 item 8, ADR 0117): both hosts run naru-proxy with its
// TLS listener on proxyTLS, orch leases one proxy exposure's address to each, and the client on the
// LAN reaches each proxy through the address: on 443, which only the holder's redirect to proxyTLS
// takes to the proxy (it refuses the handshake for a name it has no route for, which is the
// answer), and on 80, its redirect to https. When one host stops, both addresses are held by the
// other within ADR 0085's grace, lease and skew. hostd's API, which listens on every address, is not
// reached through a leased one.
const (
	edgeA    = "10.78.0.80"
	edgeB    = "10.78.0.81"
	proxyTLS = "8443"
	// noRoute is the alert the proxy refuses a handshake with while it has no route for the name.
	noRoute = "unrecognized_name"
	// hostAPI is the port of hostd's API, which listens on every address of the host (#437).
	hostAPI = "7443"
)

// zonePair is a throwaway wildcard certificate and its key, PEM, for the proxies' zone.
func zonePair(t *testing.T) (cert, key string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "*.m.naru94.example"}, DNSNames: []string{"*.m.naru94.example"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	require.NoError(t, err)
	kd, err := x509.MarshalPKCS8PrivateKey(k)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}))
}

// withProxy is withLANInterface, and on every Node but the client's a spec.proxy section with the
// zone's pair, so init runs naru-proxy and gives hostd its proxy flags.
func withProxy(t *testing.T) func(c node.Config, file string) error {
	cert, key := zonePair(t)
	return func(c node.Config, file string) error {
		if err := withLANInterface(c, file); err != nil || c.Index == lanClient {
			return err
		}
		return addProxy(file, cert, key)
	}
}

// addProxy adds to the user-data in file a spec.proxy section with the pair, its TLS listener on
// proxyTLS, and the key as the proxy's file.
func addProxy(file, cert, key string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	spec := mappingValue(doc.Content[0], "spec")
	var proxy, entry yaml.Node
	if err := proxy.Encode(map[string]any{"certificate": cert, "listen": []string{"0.0.0.0:" + proxyTLS}}); err != nil {
		return err
	}
	if err := entry.Encode(map[string]any{"path": "/run/naru-proxy/zone.key", "mode": "0600", "content": key, "owner": map[string]int{"uid": 6003, "gid": 6003}}); err != nil {
		return err
	}
	spec.Content = append(spec.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "proxy"}, &proxy)
	files := mappingValue(spec, "files")
	if files == nil {
		files = &yaml.Node{Kind: yaml.SequenceNode}
		spec.Content = append(spec.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "files"}, files)
	}
	files.Content = append(files.Content, &entry)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o600)
}

// mappingValue is the value of key in the mapping m, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// proxyExposureOf reads one proxy exposure with naructl; ok is false when orch does not show it.
func (f *fleet) proxyExposureOf(id string) (e orchapi.ProxyExposure, ok bool) {
	got := f.orch("proxy-exposure", "get", id)
	if got.Err != nil {
		return orchapi.ProxyExposure{}, false
	}
	return e, json.Unmarshal([]byte(got.Stdout), &e) == nil
}

func TestFleet_TwoProxyExposuresAreHeldByTwoProxyHostsAndBothByTheOneLeftWhenOneStops(t *testing.T) {
	targets := []string{
		"tls://" + edgeA + ":443", "tls://" + edgeB + ":443", "http://" + edgeA + ":80", "http://" + edgeB + ":80",
		"http://" + edgeA + ":81", "tls://" + nodeIP(2) + ":443", "tls://" + nodeIP(2) + ":" + proxyTLS,
		"tls://" + edgeA + ":" + hostAPI, "tls://" + nodeIP(2) + ":" + hostAPI,
	}
	f := newFleet(t, fleetOpts{controls: 1, hosts: 2, capture: true, userData: withProxy(t),
		services: []node.ExtraService{
			{Name: "naru94-nftprobe", Binary: nftProbe, Env: map[string]string{"PATH": "/bin"}},
			{Name: "naru94-lanclient", Binary: lanClientBinary, Args: targets, Env: map[string]string{"PATH": "/bin"}},
		}})
	f.startOrch()
	hostIDs := f.enrollAndAdmit("eu-1")
	h1, h2 := 2, 3
	// A proxy host is a candidate only once the operator grants it the role (ADR 0119).
	for _, i := range []int{h1, h2} {
		f.must(f.orch("host", "grant-proxy", hostIDs[i]), "host grant-proxy")
	}
	node := map[string]int{hostIDs[h1]: h1, hostIDs[h2]: h2}
	f.must(f.orch("network", "apply", "-f", f.networkFile(netA, prefixA)), "network apply "+netA)
	f.must(f.orch("address-pool", "apply", "-f", f.file("pool.yaml", "name: lan\ncidrs: ["+lbPool+"]\nnetworks: ["+netA+"]\n")), "address-pool apply")
	for id, addr := range map[string]string{"edge-a": edgeA, "edge-b": edgeB} {
		f.must(f.orch("proxy-exposure", "apply", "-f", f.file(id+".yaml", "id: "+id+"\npool: lan\nrequestedAddress: "+addr+"\nports: [443, 80]\n")), "proxy-exposure apply "+id)
	}

	client := f.node(lanClient)
	redirected := func(addr string) bool {
		return answerOf(client, "tls://"+addr+":443") == noRoute && answerOf(client, "http://"+addr+":80") == "308"
	}
	holds := func(i int, addr string) bool { return strings.Contains(lanLine(f.node(i), "lan-addr"), addr+"/32") }
	holders := func() (a, b string, ok bool) {
		ea, okA := f.proxyExposureOf("edge-a")
		eb, okB := f.proxyExposureOf("edge-b")
		return ea.Host, eb.Host, okA && okB && ea.Phase == orchapi.ExposureAnnounced && eb.Phase == orchapi.ExposureAnnounced
	}

	// Each address is announced by a host of its own, which holds it and redirects port 80 to its
	// proxy's redirect listener.
	var hostA, hostB string
	eventually(t, "each address is announced by a host of its own", func() bool {
		a, b, ok := holders()
		hostA, hostB = a, b
		return ok && a != b && node[a] != 0 && node[b] != 0
	})
	eventually(t, "the client reaches the proxy on 443 and is redirected to https on 80 through each address", func() bool { return redirected(edgeA) && redirected(edgeB) })
	assert.True(t, holds(node[hostA], edgeA) && !holds(node[hostA], edgeB), "the first host holds the first address alone")
	assert.True(t, holds(node[hostB], edgeB) && !holds(node[hostB], edgeA), "the second host holds the second address alone")
	assert.Equal(t, "fail", answerOf(client, "http://"+edgeA+":81"), "the control: a port the exposure does not carry is not redirected")
	assert.Equal(t, "fail", answerOf(client, "tls://"+nodeIP(2)+":443"), "the control: no proxy listens on 443, so 443 reached it only through the redirect")
	assert.Equal(t, noRoute, answerOf(client, "tls://"+nodeIP(2)+":"+proxyTLS), "and the proxy answers so on its own listener")
	assert.Equal(t, "fail", answerOf(client, "tls://"+edgeA+":"+hostAPI), "hostd's API does not answer on a leased address (#437)")
	assert.NotContains(t, []string{"", "fail"}, answerOf(client, "tls://"+nodeIP(2)+":"+hostAPI), "the control: it answers on the host's own")

	// The first host stops: both addresses are held by the other, and the client is redirected
	// through both again.
	f.node(node[hostA]).Stop()
	eventually(t, "both addresses are announced by the host left", func() bool {
		a, b, ok := holders()
		return ok && a == hostB && b == hostB
	})
	eventually(t, "the host left holds both and the client is redirected through each", func() bool {
		return holds(node[hostB], edgeA) && holds(node[hostB], edgeB) && redirected(edgeA) && redirected(edgeB)
	})
}
