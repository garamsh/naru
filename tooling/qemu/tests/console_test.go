//go:build qemu_real

package tests_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startConsole runs `naructl console` as the root person against the orch of control Node 1, as a
// long-running process the test stops, and returns the client that reaches it and its address.
func (f *fleet) startConsole() (*http.Client, string) {
	t := f.t
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, tool.Bin, "console", "--listen", addr, "--orch-url", f.orchURL(1), "--orch-ca", f.rootCertFile,
		"--identity", f.root.file, "--passphrase-file", f.pass)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &http.Client{Timeout: 30 * time.Second, Jar: jar}, "http://" + addr
}

func page(t *testing.T, cl *http.Client, base, path string) string {
	t.Helper()
	var body string
	eventually(t, "the console serves "+path, func() bool {
		resp, err := cl.Get(base + path)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
		return resp.StatusCode == http.StatusOK
	})
	return body
}

var csrfField = regexp.MustCompile(`name="csrf"[^>]*value="([^"]+)"|value="([^"]+)"[^>]*name="csrf"`)

func TestFleet_ConsoleListsHostsMicroVMsGroupsAndEventsAndAdmitsAPendingHost(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 2})
	f.startOrch()
	first, second := f.hosts()[0], f.hosts()[1]
	key := f.enrollmentKey()
	firstID := f.admit(f.enroll(first, key), "eu-1", "disk=ssd")
	code := f.enroll(second, key)
	secondID := f.hostID(code)
	f.must(f.orch("group", "apply", "-f", f.groupFile("shown", 1, 256, "")), "apply shown")
	eventually(t, "the group's member runs", func() bool { g, ok := f.group(1, "shown"); return ok && g.count("running") == 1 })

	cl, base := f.startConsole()
	assert.Contains(t, page(t, cl, base, "/hosts"), firstID, "the hosts page lists the admitted host")
	assert.Contains(t, page(t, cl, base, "/groups"), "shown", "the groups page lists the group")
	g, _ := f.group(1, "shown")
	require.Len(t, g.Members, 1)
	assert.Contains(t, page(t, cl, base, "/microvms"), g.Members[0].Host, "the microVMs page names the host that runs the member")
	assert.Contains(t, page(t, cl, base, "/events"), firstID, "the events page lists the first host's admission")

	// The pending page shows the second host with the code its console shows, and its form admits it.
	pending := page(t, cl, base, "/pending")
	assert.Contains(t, pending, secondID)
	assert.Contains(t, pending, code)
	m := csrfField.FindStringSubmatch(pending)
	require.NotNil(t, m, "the pending page carries a CSRF token")
	token := m[1] + m[2]
	resp, err := cl.PostForm(base+"/pending/admit", url.Values{
		"csrf": {token}, "host": {secondID}, "code": {code}, "region": {"eu-1"}, "labels": {"disk=hdd"},
	})
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Less(t, resp.StatusCode, 400, string(body))
	eventually(t, "the admitted host is listed with its label", func() bool {
		for _, h := range f.hostList(1) {
			if h.ID == secondID {
				return h.Region == "eu-1" && h.Labels["disk"] == "hdd"
			}
		}
		return false
	})
	assert.Contains(t, page(t, cl, base, "/hosts"), secondID, "the hosts page lists the host it admitted")
}

func TestFleet_MetricsAnswerFromInitHostdOrchdAndTheSigner(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1})
	f.startOrch()
	f.enrollAndAdmit("eu-1")
	h := f.hosts()[0]

	// Ports 9100 to 9103: init on every Node, hostd on the host, orchd and the signer on the control host.
	for _, c := range []struct {
		node int
		port string
		what string
	}{
		{1, "9100", "init on the control host"}, {h, "9100", "init on the host"}, {h, "9101", "naru-hostd"},
		{1, "9102", "naru-orchd"}, {1, "9103", "naru-signer"},
	} {
		eventually(t, fmt.Sprintf("/metrics answers from %s", c.what), func() bool {
			resp, err := http.Get("http://" + f.forward(c.node, c.port) + "/metrics") //nolint:noctx // a test's request to a loopback forward
			if err != nil {
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode == http.StatusOK && strings.Contains(string(b), "# TYPE")
		})
	}
}
