package layout

import (
	"errors"
	"slices"

	"github.com/garamsh/naru/naructl/internal/pki"
)

// Mode is how a Node comes by its certificates. It is the one place the Node's role and the way it
// is credentialed are decided together; plan is the one place that branches on it.
type Mode int

const (
	// WorkerToken is a worker whose agent presents a bootstrap token for a certificate.
	WorkerToken Mode = iota
	// WorkerFiles is a worker whose agent's pair is an operator's file.
	WorkerFiles
	// ControlPlaneToken is the control-plane Node that issues its certificates from spec.pki and signs
	// the agents' requests: the only Node that carries the CA's key.
	ControlPlaneToken
	// ControlPlaneFiles is a control-plane Node whose every pair is an operator's file, with no token,
	// no spec.pki and no signing key.
	ControlPlaneFiles
)

// ModeOf is the Mode of a control-plane or worker Node whose pairs are files or not.
func ModeOf(controlPlane, pairsAsFiles bool) Mode {
	switch {
	case controlPlane && pairsAsFiles:
		return ControlPlaneFiles
	case controlPlane:
		return ControlPlaneToken
	case pairsAsFiles:
		return WorkerFiles
	}
	return WorkerToken
}

// ControlPlane reports whether the Node runs etcd and gaon's control plane.
func (m Mode) ControlPlane() bool { return m == ControlPlaneToken || m == ControlPlaneFiles }

// PairsAsFiles reports whether the Node's pairs are operator-issued files in place of a token.
func (m Mode) PairsAsFiles() bool { return m == WorkerFiles || m == ControlPlaneFiles }

// Signs reports whether the Node issues its certificates and signs the agents' requests.
func (m Mode) Signs() bool { return m == ControlPlaneToken }

// pairSpec is a certificate pair issued to the Node as files: its name, which names the files, and CN.
type pairSpec struct{ name, cn string }

// plan is everything that differs between the modes. A reader of the layout asks the plan, and
// nothing else switches on a Mode.
type plan struct {
	// pairs are issued here and carried as files, in this order.
	pairs []pairSpec
	// caFile is set when the CA's certificate is a file; otherwise init writes it from spec.pki.
	caFile bool
	// spec.pki is the Node's: it issues from the CA it carries.
	specPKI bool
	// token is set when the Node has a bootstrap token and a file for it; tokenList when it also has
	// the list of every Node's token.
	token, tokenList bool
	// agent, apiserverTokens and signer are the credential arguments of the Node Agent, the API
	// server and the controllers; the last two are empty where the Node does not sign.
	agent, apiserverTokens, signer []string
}

// tlsClient are the arguments that give a service the CA and its own pair.
func tlsClient(name string) []string {
	return []string{"--tls-ca", gaonPath("ca.pem"), "--tls-cert", gaonPath(name + ".pem"), "--tls-key", gaonPath(name + ".key")}
}

func (m Mode) plan(name string) plan {
	controlPlanePairs := []pairSpec{{"apiserver", "gaon-apiserver"}, {"scheduler", "gaon-scheduler"}, {"controllers", Gaon.Signer}, etcdServer, etcdPeer, apiserverTo}
	tokenAgent := []string{"--tls-ca", gaonPath("ca.pem"), "--bootstrap-token-file", BootstrapTokenFile, "--cert-dir", AgentCertDir}
	switch m {
	case WorkerFiles:
		return plan{pairs: []pairSpec{{"agent", name}}, caFile: true, agent: tlsClient("agent")}
	case ControlPlaneToken:
		return plan{
			specPKI: true, token: true, tokenList: true, agent: tokenAgent,
			apiserverTokens: []string{"--bootstrap-token-file", BootstrapTokensFile},
			signer:          []string{"--signing-cert", gaonPath("ca.pem"), "--signing-key", gaonPath("ca.key")},
		}
	case ControlPlaneFiles:
		return plan{pairs: append([]pairSpec{{"agent", name}}, controlPlanePairs...), caFile: true, agent: tlsClient("agent")}
	}
	return plan{caFile: true, token: true, agent: tokenAgent}
}

// check refuses a token the Node cannot use: one missing, one that is not in the form gaon reads, a
// list on a worker, or a token with the pairs as files; and a Node that signs without the service
// that signs.
func (m Mode) check(token string, tokens, disable []string) error {
	p := m.plan("")
	if !p.token {
		if m.PairsAsFiles() && (token != "" || len(tokens) > 0) {
			return errors.New("a Node whose pairs are files has no use for a bootstrap token")
		}
	} else {
		switch {
		case token == "":
			return errors.New("the Node has no bootstrap token (and its pairs are not files)")
		case !pki.ValidToken(token):
			return errors.New("the bootstrap token is not <id>.<secret>, [a-z0-9]{6}.[a-z0-9]{16}")
		case !p.tokenList && len(tokens) > 0:
			return errors.New("only the control-plane Node lists tokens: its API server reads them")
		case p.tokenList && !slices.Contains(tokens, token):
			return errors.New("the control-plane Node's token list does not hold its own token")
		case m.Signs() && slices.Contains(disable, Gaon.Signer):
			return errors.New(Gaon.Signer + " signs the agents' requests: a control-plane Node that is not left to operator pairs (--pairs-as-files) cannot disable it")
		}
	}
	for _, t := range tokens {
		if !pki.ValidToken(t) {
			return errors.New("a token in the list is not <id>.<secret>, [a-z0-9]{6}.[a-z0-9]{16}")
		}
	}
	return nil
}

// LoadTokens is the Node's own token, made if there is none, and, where the Node lists them, the
// token of every Node whose file is in caDir. A Node with no token has none.
func (m Mode) LoadTokens(caDir, name string) (token string, tokens []string, err error) {
	p := m.plan(name)
	if !p.token {
		return "", nil, nil
	}
	if token, err = pki.EnsureToken(caDir, name); err != nil {
		return "", nil, err
	}
	if p.tokenList {
		if tokens, err = pki.ReadTokens(caDir); err != nil {
			return "", nil, err
		}
	}
	return token, tokens, nil
}
