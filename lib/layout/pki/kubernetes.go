package pki

import (
	"encoding/base64"
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
	// ServiceAccountIssuersFile lists, a URL to a line, the issuers the operator set (ADR 0087): gen
	// kubernetes writes it and gen kubernetes-node and gen kubernetes-jwks read it. The first signs.
	ServiceAccountIssuersFile = "service-account-issuers"
	// DisabledAddonsFile lists, a name to a line, the cluster add-ons the cluster does not run (ADR 0083):
	// gen kubernetes writes it and gen kubernetes-node reads it.
	DisabledAddonsFile = "disabled-addons"
	// AdminKubeconfigFile is the administrator's kubeconfig.
	AdminKubeconfigFile = "admin.conf"
	// AdminName and AdminOrganization are the subject of the administrator's certificate; the
	// organization is the group that RBAC binds to cluster-admin.
	AdminName         = "kubernetes-admin"
	AdminOrganization = "system:masters"
)

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
