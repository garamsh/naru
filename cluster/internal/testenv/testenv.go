// Package testenv runs the tests of this unit against a real kube-apiserver and etcd (envtest) with
// naru's CRDs and Cluster API's installed. The binaries come from KUBEBUILDER_ASSETS, which
// `make envtest` fills (ADR 0033); a test run without them fails, it does not skip.
package testenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	// The CRDs of Cluster API's core types are files in its root module, which only this blank
	// import keeps in go.mod: the api module does not carry them.
	_ "sigs.k8s.io/cluster-api/version"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/garamsh/naru/cluster/internal/scheme"
)

var restConfig *rest.Config

// Main starts the API server the tests of one package share, runs them and stops it. A package
// calls it from TestMain.
func Main(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set: run the tests through make test, which fetches the envtest binaries")
		os.Exit(1)
	}
	capi, err := capiCRDs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot find Cluster API's CRDs:", err)
		os.Exit(1)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{naruCRDs(), capi},
		ErrorIfCRDPathMissing: true,
	}
	restConfig, err = env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot start envtest:", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "cannot stop envtest:", err)
		code = 1
	}
	os.Exit(code)
}

// naruCRDs is the directory of the generated CRDs of this unit.
func naruCRDs() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "config", "crd", "bases")
}

// capiCRDs is the directory of Cluster API's core CRDs in the module cache, where the module the
// build uses has them. It runs go list offline: the module is already in the cache.
func capiCRDs() (string, error) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/cluster-api")
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return filepath.Join(strings.TrimSpace(string(out)), "core", "config", "crd", "bases"), nil
}

// Client is a client straight to the API server, with every type of the scheme.
func Client(t *testing.T) client.Client {
	t.Helper()
	c, err := client.New(restConfig, client.Options{Scheme: scheme.New()})
	require.NoError(t, err)
	return c
}

// Manager returns a manager on the shared API server. Its controllers are named by their callers,
// and name validation is off because every test builds its own. start runs it until the test
// ends; set up controllers on it before.
func Manager(t *testing.T) (mgr manager.Manager, start func()) {
	t.Helper()
	mgr, err := manager.New(restConfig, manager.Options{
		Scheme:     scheme.New(),
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	require.NoError(t, err)
	return mgr, func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = mgr.Start(ctx)
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("the manager did not stop")
			}
		})
	}
}

// Namespace creates a namespace for the test and returns its name.
func Namespace(t *testing.T, c client.Client) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "t-"}}
	require.NoError(t, c.Create(context.Background(), ns))
	return ns.Name
}
