// Package hostcli is the harness's client of a host's API for the calls naructl has no command for
// (snapshot, resume, networks, the tunnel): lib/hostapi's client over the mutual TLS of a client
// file `naructl gen host-client` wrote.
package hostcli

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/hostapi"
)

// file is the part of naructl's client file the harness reads.
type file struct {
	Endpoints   []string `yaml:"endpoints"`
	CA          string   `yaml:"ca"`
	Certificate string   `yaml:"certificate"`
	Key         string   `yaml:"key"`
}

// New returns a client of the host API that the client file at path names, at its first endpoint,
// presenting the file's certificate and trusting its CA. A malformed file is reported without its
// text, which carries a key.
func New(path string) (*hostapi.Client, error) {
	return Dial(path, "")
}

// Dial is New with the endpoint replaced by endpoint, "127.0.0.1:port", when it is not empty: a
// host the harness reaches through a forward other than the one the file names.
func Dial(path, endpoint string) (*hostapi.Client, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f file
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s is not a client file", path)
	}
	if endpoint == "" && len(f.Endpoints) > 0 {
		endpoint = f.Endpoints[0]
	}
	if endpoint == "" || f.CA == "" || f.Certificate == "" || f.Key == "" {
		return nil, fmt.Errorf("%s is not a client file: endpoints, ca, certificate and key are all required", path)
	}
	pair, err := tls.X509KeyPair([]byte(f.Certificate), []byte(f.Key))
	if err != nil {
		return nil, fmt.Errorf("%s: the certificate and key do not make a pair", path)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(f.CA)) {
		return nil, errors.New(path + ": the CA holds no certificate")
	}
	hc := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}},
			DisableKeepAlives: true,
		},
	}
	return hostapi.NewClient("https://"+endpoint, hc), nil
}
