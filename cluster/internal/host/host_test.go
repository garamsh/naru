package host_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/garamsh/naru/cluster/internal/api/v1alpha1"
	"github.com/garamsh/naru/cluster/internal/host"
	"github.com/garamsh/naru/cluster/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

// pki is a throwaway authority held in memory; no key it makes is written anywhere.
type pki struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	next int64
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "naru test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &pki{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), next: 2}
}

// issue returns the PEM of a certificate and its key. orgs becomes the subject's Organization, from
// which the host API reads the role of a client.
func (p *pki) issue(t *testing.T, orgs []string, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	p.next++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(p.next), Subject: pkix.Name{CommonName: "test", Organization: orgs},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.cert, &key.PublicKey, p.key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// document is the body GET /v1/host answers, as the host API's wire format has it.
type document struct {
	Region   string            `json:"region"`
	Labels   map[string]string `json:"labels"`
	Capacity map[string]uint64 `json:"capacity"`
	Free     map[string]uint64 `json:"free"`
}

// hostAPI serves GET /v1/host over mutual TLS 1.3, as the host API does: the role is the client
// certificate's Organization, and a certificate without one gets 403.
type hostAPI struct {
	srv *httptest.Server
	mu  sync.Mutex
	doc document
}

func startHostAPI(t *testing.T, ca *pki, doc document) *hostAPI {
	t.Helper()
	h := &hostAPI{doc: doc}
	certPEM, keyPEM := ca.issue(t, nil, x509.ExtKeyUsageServerAuth)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	h.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orgs := r.TLS.PeerCertificates[0].Subject.Organization
		if !slices.Contains(orgs, "naru:host-reader") && !slices.Contains(orgs, "naru:host-provisioner") {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "the certificate carries no role"})
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		_ = json.NewEncoder(w).Encode(h.doc)
	}))
	h.srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	h.srv.StartTLS()
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hostAPI) set(labels map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.doc.Labels = labels
}

func (h *hostAPI) address() string { return h.srv.Listener.Addr().String() }

// naruHost creates a NaruHost at address with a Secret holding a client pair of orgs, and returns
// its name. A nil orgs makes a pair with no role.
func naruHost(t *testing.T, c client.Client, ca *pki, name, address string, orgs []string) {
	t.Helper()
	ctx := context.Background()
	certPEM, keyPEM := ca.issue(t, orgs, x509.ExtKeyUsageClientAuth)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Data:       map[string][]byte{corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM, "ca.crt": ca.pem},
	}
	require.NoError(t, c.Create(ctx, secret))
	t.Cleanup(func() { _ = c.Delete(ctx, secret) })
	h := &v1alpha1.NaruHost{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.NaruHostSpec{
			Address:         address,
			ClientSecretRef: v1alpha1.SecretReference{Name: name, Namespace: "default"},
		},
	}
	require.NoError(t, c.Create(ctx, h))
	t.Cleanup(func() { _ = c.Delete(ctx, h) })
}

func run(t *testing.T) client.Client {
	t.Helper()
	mgr, start := testenv.Manager(t)
	require.NoError(t, (&host.Reconciler{Client: mgr.GetClient(), Interval: 100 * time.Millisecond}).SetupWithManager(mgr, "host"))
	start()
	return testenv.Client(t)
}

func ready(c client.Client, name string) (*v1alpha1.NaruHost, *metav1.Condition) {
	var h v1alpha1.NaruHost
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &h); err != nil {
		return nil, nil
	}
	return &h, meta.FindStatusCondition(h.Status.Conditions, v1alpha1.HostReady)
}

func waitReady(t *testing.T, c client.Client, name string, status metav1.ConditionStatus, reason string) *v1alpha1.NaruHost {
	t.Helper()
	var got *v1alpha1.NaruHost
	require.Eventually(t, func() bool {
		h, cond := ready(c, name)
		got = h
		return cond != nil && cond.Status == status && cond.Reason == reason
	}, 20*time.Second, 50*time.Millisecond, "NaruHost %s is Ready=%s with reason %s", name, status, reason)
	return got
}

func TestHost_StatusFromTheHostAPI(t *testing.T) {
	c := run(t)
	ca := newPKI(t)
	api := startHostAPI(t, ca, document{
		Region:   "eu",
		Labels:   map[string]string{"tier": "gold"},
		Capacity: map[string]uint64{"cpu": 16, "memory": 64 << 30, "disk": 500 << 30},
		Free:     map[string]uint64{"cpu": 12, "memory": 40 << 30, "disk": 400 << 30},
	})
	naruHost(t, c, ca, "answers", api.address(), []string{"naru:host-reader"})

	h := waitReady(t, c, "answers", metav1.ConditionTrue, v1alpha1.ReasonAnswered)
	require.Equal(t, "eu", h.Status.Region)
	require.Equal(t, map[string]string{"tier": "gold"}, h.Status.Labels)
	require.Zero(t, h.Status.Capacity.CPU.Cmp(resource.MustParse("16")))
	require.Zero(t, h.Status.Capacity.Memory.Cmp(resource.MustParse("64Gi")))
	require.Zero(t, h.Status.Capacity.Disk.Cmp(resource.MustParse("500Gi")))
	require.Zero(t, h.Status.Free.CPU.Cmp(resource.MustParse("12")))
	require.Zero(t, h.Status.Free.Memory.Cmp(resource.MustParse("40Gi")))
	require.Zero(t, h.Status.Free.Disk.Cmp(resource.MustParse("400Gi")))

	// A change on the host shows at the next poll.
	api.set(map[string]string{"tier": "silver"})
	require.Eventually(t, func() bool {
		h, _ := ready(c, "answers")
		return h != nil && h.Status.Labels["tier"] == "silver"
	}, 20*time.Second, 50*time.Millisecond, "the new label reaches the status")
}

func TestHost_UnreachableHostHasAFalseCondition(t *testing.T) {
	c := run(t)
	ca := newPKI(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	naruHost(t, c, ca, "closed", addr, []string{"naru:host-reader"})

	h := waitReady(t, c, "closed", metav1.ConditionFalse, v1alpha1.ReasonUnreachable)
	require.Empty(t, h.Status.Region)
	cond := meta.FindStatusCondition(h.Status.Conditions, v1alpha1.HostReady)
	require.NotEmpty(t, cond.Message)
}

func TestHost_ARoleLessClientIsRejected(t *testing.T) {
	c := run(t)
	ca := newPKI(t)
	api := startHostAPI(t, ca, document{Region: "eu", Labels: map[string]string{}})
	naruHost(t, c, ca, "norole", api.address(), nil)

	h := waitReady(t, c, "norole", metav1.ConditionFalse, v1alpha1.ReasonRejected)
	require.Empty(t, h.Status.Region, "a refused answer writes no host data")
}

func TestHost_MissingSecretIsAnInvalidClientPair(t *testing.T) {
	c := run(t)
	h := &v1alpha1.NaruHost{
		ObjectMeta: metav1.ObjectMeta{Name: "nosecret"},
		Spec: v1alpha1.NaruHostSpec{
			Address:         "127.0.0.1:1",
			ClientSecretRef: v1alpha1.SecretReference{Name: "absent", Namespace: "default"},
		},
	}
	require.NoError(t, c.Create(context.Background(), h))
	t.Cleanup(func() { _ = c.Delete(context.Background(), h) })

	waitReady(t, c, "nosecret", metav1.ConditionFalse, v1alpha1.ReasonClientPairInvalid)
}
