package ccm

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

// Config is the cloud-config file the controller reads: where orch is and the pair the controller
// calls it with (made by `naructl ccm credential`), and the network and pool it serves.
type Config struct {
	// Orch is orch's https URL; CA is the file of the authority its certificate chains to.
	Orch string `json:"orch"`
	CA   string `json:"ca"`
	// Cert and Key are the exposer pair, whose Subject names Network.
	Cert    string `json:"cert"`
	Key     string `json:"key"`
	Network string `json:"network"`
	Pool    string `json:"pool"`
	// Loadbalancer is false when only the node controller is on (ADR 0085 item 12).
	Loadbalancer *bool `json:"loadbalancer,omitempty"`
}

// ParseConfig reads and checks the file.
func ParseConfig(r io.Reader) (Config, error) {
	var c Config
	dec := json.NewDecoder(io.LimitReader(r, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("cloud config: %w", err)
	}
	for name, v := range map[string]string{"orch": c.Orch, "ca": c.CA, "cert": c.Cert, "key": c.Key, "network": c.Network} {
		if v == "" {
			return c, fmt.Errorf("cloud config: %q is required", name)
		}
	}
	if c.LoadBalancerOn() && c.Pool == "" {
		return c, errors.New(`cloud config: "pool" is required while the load balancer is on`)
	}
	return c, nil
}

// LoadBalancerOn is whether the service controller is on.
func (c Config) LoadBalancerOn() bool { return c.Loadbalancer == nil || *c.Loadbalancer }

// Client returns the orch client the config describes. The pair is read once; a replaced pair needs
// a restart of the controller, as the Secret holding it is replaced.
func (c Config) Client() (*orchapi.Client, error) {
	ca, err := os.ReadFile(c.CA)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("%s holds no certificate", c.CA)
	}
	pair, err := tls.LoadX509KeyPair(c.Cert, c.Key)
	if err != nil {
		return nil, err
	}
	if _, ok := identity.ExposerSubject(c.Network); !ok {
		return nil, fmt.Errorf("network %q is not a network name", c.Network)
	}
	return orchapi.New(c.Orch, orchapi.HTTPClient(orchapi.TrustCA(pool, &pair), 15*time.Second))
}
