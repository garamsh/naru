// Package upgrade is naructl's side of the in-place upgrade (ADR 0025): it finds a bundle's digest
// in a SHA256SUMS file, checks the bundle against it, and streams the bundle to a Node's
// POST /v1/upgrade. The Node checks the digest again; this check refuses a bundle that was
// damaged on the way to the operator before any byte is sent.
package upgrade

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/garamsh/naru/naructl/internal/client"
)

// maxAnswer bounds what is read of a Node's answer.
const maxAnswer = 1 << 20

// SumFor returns the SHA-256 that sums, the text of a SHA256SUMS file, lists for the file called
// name. A line is a digest, a space, and a file name that may have a "*" before it, as sha256sum
// writes it; a name with a directory is compared by its last element. A file that is not listed,
// or is listed with two digests, is an error.
func SumFor(sums io.Reader, name string) (string, error) {
	found := ""
	sc := bufio.NewScanner(sums)
	for sc.Scan() {
		sum, file, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		file = strings.TrimPrefix(strings.TrimSpace(file), "*")
		if !ok || filepath.Base(file) != name {
			continue
		}
		if len(sum) != sha256.Size*2 || !isHex(sum) {
			return "", fmt.Errorf("the line for %s does not start with a SHA-256", name)
		}
		sum = strings.ToLower(sum)
		if found != "" && found != sum {
			return "", fmt.Errorf("the sums file lists %s with two different digests", name)
		}
		found = sum
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("the sums file has no line for %s", name)
	}
	return found, nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// Check returns nil when the file at path has the SHA-256 want.
func Check(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s has the SHA-256 %s, and the sums file says %s", path, got, want)
	}
	return nil
}

// Node is one Node's API as the upgrade calls it.
type Node struct {
	base string
	http *http.Client
}

// Dial returns the Node at node, an IP address with or without a port; when it is empty, the
// file's first endpoint. It speaks TLS 1.3 only, trusts the file's authority and presents its pair.
// Nothing is sent until a call.
func Dial(f client.File, node string) (*Node, error) {
	if node == "" {
		node = f.Endpoints[0]
	}
	addr, err := client.Address(node)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(f.CA)) {
		return nil, errors.New("the client file's ca is not a PEM certificate")
	}
	pair, err := tls.X509KeyPair([]byte(f.Certificate), []byte(f.Key))
	if err != nil {
		return nil, fmt.Errorf("the client file's certificate and key: %w", err)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}}}
	return &Node{base: "https://" + addr, http: &http.Client{Transport: tr}}, nil
}

// Push streams the bundle at path to the Node, which is told its SHA-256, sum, and returns the
// actions the Node accepted. The Node answers before it reboots.
func (n *Node) Push(ctx context.Context, path, sum string) ([]client.Action, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	target := "/v1/upgrade?" + url.Values{"sha256": {sum}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.base+target, f)
	if err != nil {
		return nil, err
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := n.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST /v1/upgrade: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, statusError(resp)
	}
	var body struct {
		Actions []client.Action `json:"actions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&body); err != nil {
		return nil, fmt.Errorf("POST /v1/upgrade: the answer is not what the Node API sends: %w", err)
	}
	return body.Actions, nil
}

// Outcome is how the Node's last upgrade ended.
type Outcome struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Result string `json:"result"`
	Reason string `json:"reason"`
	Time   string `json:"time"`
}

// Status is what GET /v1/version says of the upgrade: the slot this boot came from, and the last outcome.
type Status struct {
	Version     string   `json:"version"`
	BootID      string   `json:"bootId"`
	Slot        string   `json:"slot"`
	LastUpgrade *Outcome `json:"lastUpgrade"`
}

// Status asks the Node for its slot and the outcome of its last upgrade.
func (n *Node) Status(ctx context.Context) (Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.base+"/v1/version", nil)
	if err != nil {
		return Status{}, err
	}
	resp, err := n.http.Do(req)
	if err != nil {
		return Status{}, fmt.Errorf("GET /v1/version: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return Status{}, statusError(resp)
	}
	var s Status
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&s); err != nil {
		return Status{}, fmt.Errorf("GET /v1/version: the answer is not what the Node API sends: %w", err)
	}
	return s, nil
}

// statusError reads the body of a response that is not 2xx as the Node API's error shape, and as
// its text, cut short, when it is not.
func statusError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Error == "" {
		text := strings.TrimSpace(string(raw))
		if len(text) > 200 {
			text = text[:200] + "..."
		}
		return &client.StatusError{Status: resp.StatusCode, Message: text}
	}
	return &client.StatusError{Status: resp.StatusCode, Message: body.Error}
}
