package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"
)

// The files of a Kubernetes cluster's directory beside the cluster's authority (ADR 0031).
const (
	// FrontProxyDir holds the front-proxy authority, which signs the aggregation layer's client certificate.
	FrontProxyDir = "front-proxy"
	// ServiceAccountKeyFile and ServiceAccountPubFile are the key the API server signs ServiceAccount
	// tokens with, and the public key that verifies them.
	ServiceAccountKeyFile = "sa.key"
	ServiceAccountPubFile = "sa.pub"
	// AdminKubeconfigFile is the administrator's kubeconfig.
	AdminKubeconfigFile = "admin.conf"
	// AdminName and AdminOrganization are the subject of the administrator's certificate; the
	// organization is the group that RBAC binds to cluster-admin.
	AdminName         = "kubernetes-admin"
	AdminOrganization = "system:masters"
)

// ServiceAccountKey returns a new key for signing ServiceAccount tokens, as the PEM of the private
// key and of the public key.
func ServiceAccountKey() (keyPEM, pubPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), nil
}

// Kubeconfig is a kubeconfig that names server, trusts caPEM and presents pair, both inline.
func Kubeconfig(user, server string, caPEM []byte, pair Pair) []byte {
	b64 := base64.StdEncoding.EncodeToString
	return []byte(strings.TrimLeft(fmt.Sprintf(`
apiVersion: v1
kind: Config
clusters:
- name: naru
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: %s
  user:
    client-certificate-data: %s
    client-key-data: %s
contexts:
- name: %s@naru
  context:
    cluster: naru
    user: %s
current-context: %s@naru
`, server, b64(caPEM), user, b64(pair.CertPEM), b64(pair.KeyPEM), user, user, user), "\n"))
}
