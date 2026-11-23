// Command naru-ccm is naru's cloud-controller-manager (docs/architecture/adr/0085): its service
// controller fulfils a Service of type LoadBalancer with an address orch assigns and a host announces,
// and its node controller names each Node after the microVM it is. It runs the standard
// cloud-controller-manager of k8s.io/cloud-provider with the provider `naru`, so a customer replaces it
// by turning it off and running any other implementation.
package main

import (
	"log/slog"
	"os"

	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/app"
	"k8s.io/cloud-provider/app/config"
	"k8s.io/cloud-provider/names"
	"k8s.io/cloud-provider/options"
	"k8s.io/component-base/cli"
	cliflag "k8s.io/component-base/cli/flag"
	_ "k8s.io/component-base/metrics/prometheus/clientgo" // the client-go metrics
	_ "k8s.io/component-base/metrics/prometheus/version"  // the version metric
	"k8s.io/klog/v2"

	"github.com/garamsh/naru/node/internal/ccm"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ccm.Register(log)
	opts, err := options.NewCloudControllerManagerOptions()
	if err != nil {
		klog.Fatalf("naru-ccm: options: %v", err)
	}
	cmd := app.NewCloudControllerManagerCommand(opts, initialize, app.DefaultInitFuncConstructors, names.CCMControllerAliases(), cliflag.NamedFlagSets{}, wait.NeverStop)
	os.Exit(cli.Run(cmd))
}

func initialize(c *config.CompletedConfig) cloudprovider.Interface {
	shared := c.ComponentConfig.KubeCloudShared.CloudProvider
	cloud, err := cloudprovider.InitCloudProvider(shared.Name, shared.CloudConfigFile)
	if err != nil || cloud == nil {
		klog.Fatalf("naru-ccm: cloud provider %q: %v", shared.Name, err)
	}
	return cloud
}
