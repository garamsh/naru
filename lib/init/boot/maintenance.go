package boot

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/netcfg"
	"github.com/garamsh/naru/lib/wire"
)

// maintenanceKeyFile is where init writes the revocation public key naru-hostd's maintenance mode
// checks the list against. The root file system is still writable when a boot is refused.
const maintenanceKeyFile = "/run/naru-hostd/maintenance-revocation.pub"

// hostdBinary is naru-hostd's path in the image, the one config's derived services use.
const hostdBinary = "/bin/naru-hostd"

// reasonMax bounds the refusal's reason as naru-hostd and a rolled-back upgrade carry it.
const reasonMax = 300

// Anchor is what a refused host's maintenance mode trusts (ADR 0082): orch's URL, the SHA-256 of
// the authority orch's certificate chains to, which is the authority the host's and an operator's
// certificates chain to, and the public key that verifies the revocation list.
type Anchor struct {
	OrchURL             string `json:"orchURL"`
	OrchCASHA256        string `json:"orchCASHA256"`
	RevocationPublicKey string `json:"revocationPublicKey,omitempty"`
}

// anchorOf is the anchor a configuration carries: orch of spec.join, or of spec.host.orch.
func anchorOf(spec config.Spec) Anchor {
	var a Anchor
	switch {
	case spec.Join != nil:
		a.OrchURL, a.OrchCASHA256 = spec.Join.Orch.URL, spec.Join.Orch.CASHA256
	case spec.Host != nil && spec.Host.Orch != nil:
		a.OrchURL, a.OrchCASHA256 = spec.Host.Orch.URL, spec.Host.Orch.CASHA256
	}
	if spec.Host != nil {
		a.RevocationPublicKey = spec.Host.RevocationPublicKey
	}
	return a
}

// seedAnchor reads the anchor from the seed's user-data without validating it, because the seed is
// what a refusal was about: a plain parse of the three places that carry it. It returns the zero
// Anchor when the seed does not parse.
func seedAnchor(userData []byte) Anchor {
	var doc struct {
		Spec config.Spec `yaml:"spec"`
	}
	if err := yaml.Load(userData, &doc); err != nil {
		return Anchor{}
	}
	return anchorOf(doc.Spec)
}

// saveAnchor stores the anchor of a configuration that booted, for a boot that has no seed to read
// it from. It stores nothing when the configuration names no orch, and a store that cannot take it
// is logged: the boot goes on.
func saveAnchor(configs Configs, spec config.Spec, log *slog.Logger) {
	a := anchorOf(spec)
	if a.OrchURL == "" || a.OrchCASHA256 == "" {
		return
	}
	data, err := json.Marshal(a)
	if err == nil {
		err = configs.SaveAnchor(data)
	}
	if err != nil {
		log.Error("store the maintenance anchor; a refused boot will not have it", "err", err)
	}
}

// watchedHost records what maintenance mode needs to know of the boot that was refused: the seed it
// read, whether the network was brought up and whether the data disk is mounted.
type watchedHost struct {
	Host
	userData, networkConfig []byte
	networked, dataMounted  bool
}

func (h *watchedHost) MountDataDisk(d config.DataDisk) error {
	err := h.Host.MountDataDisk(d)
	h.dataMounted = err == nil
	return err
}

func (h *watchedHost) WipeDataDisk(d config.DataDisk) error {
	h.dataMounted = false
	return h.Host.WipeDataDisk(d)
}

// mountDataDisk mounts the data disk by its default label and mount point, as a boot with no seed
// does, when the refusal came before it was mounted, so that the stored anchor and network-config
// can be read. It never carries out a reset. A disk that cannot be mounted is logged and the
// stored files stay unread.
func (h *watchedHost) mountDataDisk(log *slog.Logger) {
	if h.dataMounted {
		return
	}
	if err := h.MountDataDisk(config.DataDisk{Label: wire.DataLabel, MountPoint: wire.DataMount}); err != nil {
		log.Error("maintenance mode: the data disk cannot be mounted, so the stored anchor and network-config are not read", "err", err)
	}
}

func (h *watchedHost) ReadSeed() ([]byte, []byte, []byte, error) {
	u, n, m, err := h.Host.ReadSeed()
	h.userData, h.networkConfig = u, n
	return u, n, m, err
}

func (h *watchedHost) ApplyNetwork(c netcfg.Config, log *slog.Logger) (netcfg.Applied, error) {
	a, err := h.Host.ApplyNetwork(c, log)
	if err == nil {
		h.networked = true
	}
	return a, err
}

// watchedServices records which services were started and whether cgroups were prepared.
type watchedServices struct {
	Services
	prepared bool
	started  map[string]bool
}

func (s *watchedServices) Prepare() error {
	if s.prepared {
		return nil
	}
	if err := s.Services.Prepare(); err != nil {
		return err
	}
	s.prepared = true
	return nil
}

func (s *watchedServices) Start(svc config.Service) error {
	if s.started == nil {
		s.started = map[string]bool{}
	}
	s.started[svc.Name] = true
	return s.Services.Start(svc)
}

// maintain starts naru-hostd in maintenance mode after a refused boot (ADR 0082): the one service
// that serves a reset to an operator whose certificate chains to the authority orch's certificate
// chains to, checked against the revocation list. It does nothing, and says why, when the host has
// no anchor to trust, no revocation key to check the list with, or already runs hostd; it never
// changes the refusal, which Run still returns.
func maintain(host *watchedHost, services *watchedServices, configs Configs, refusal error, log *slog.Logger) {
	host.mountDataDisk(log)
	anchor := host.anchor(configs)
	switch {
	case services.started[config.HostdService]:
		log.Warn("no maintenance mode: naru-hostd already runs")
		return
	case anchor.OrchURL == "" || anchor.OrchCASHA256 == "":
		log.Warn("no maintenance mode: neither the seed nor the data disk names orch and the fingerprint of its authority")
		return
	case anchor.RevocationPublicKey == "":
		log.Warn("no maintenance mode: no revocation public key is known, so no operator certificate could be checked against the list")
		return
	}
	if !host.networked {
		if err := host.bringUpNetwork(configs, log); err != nil {
			log.Error("no maintenance mode: the network cannot be brought up", "err", err)
			return
		}
	}
	if err := host.WriteFiles([]config.File{{Path: maintenanceKeyFile, Mode: "0644", Content: anchor.RevocationPublicKey}}); err != nil {
		log.Error("no maintenance mode: write the revocation key", "err", err)
		return
	}
	if err := services.Prepare(); err != nil {
		log.Error("no maintenance mode: prepare cgroups", "err", err)
		return
	}
	svc := config.Service{
		Name:   config.HostdService,
		Binary: hostdBinary,
		Args: []string{
			"--maintenance",
			"--maintenance-reason=" + Reason(refusal),
			"--orch-url=" + anchor.OrchURL,
			"--orch-ca-sha256=" + anchor.OrchCASHA256,
			"--revocation-public-key=" + maintenanceKeyFile,
		},
		RestartPolicy: config.RestartAlways,
	}
	if err := services.Start(svc); err != nil {
		log.Error("no maintenance mode: start naru-hostd", "err", err)
		return
	}
	log.Warn("maintenance mode: naru-hostd serves a reset to operators under orch's authority", "orch", anchor.OrchURL)
}

// anchor returns the anchor of the seed, whose orch and fingerprint are the ones the installation
// left, with the revocation key of the stored anchor when the seed carries none.
func (h *watchedHost) anchor(configs Configs) Anchor {
	a := seedAnchor(h.userData)
	if data, err := configs.LoadAnchor(); err == nil {
		var kept Anchor
		if json.Unmarshal(data, &kept) == nil {
			if a.OrchURL == "" || a.OrchCASHA256 == "" {
				a.OrchURL, a.OrchCASHA256 = kept.OrchURL, kept.OrchCASHA256
			}
			if a.RevocationPublicKey == "" && kept.OrchCASHA256 == a.OrchCASHA256 {
				a.RevocationPublicKey = kept.RevocationPublicKey
			}
		}
	}
	return a
}

// bringUpNetwork applies the stored network-config, else the seed's, else DHCPv4 on the first
// link with a carrier: a refusal that came before the network leaves it down.
func (h *watchedHost) bringUpNetwork(configs Configs, log *slog.Logger) error {
	var sources [][]byte
	if stored, err := configs.LoadNetwork(); err == nil {
		sources = append(sources, stored)
	}
	if len(h.networkConfig) > 0 {
		sources = append(sources, h.networkConfig)
	}
	var errs []error
	for _, raw := range sources {
		cfg, err := netcfg.Parse(raw)
		if err == nil {
			if _, err = h.Host.ApplyNetwork(cfg, log); err == nil {
				h.networked = true
				return nil
			}
		}
		errs = append(errs, err)
	}
	if _, err := h.Host.ApplyNetwork(netcfg.Config{Interfaces: []netcfg.Interface{{DHCP: true}}}, log); err != nil {
		return errors.Join(append(errs, err)...)
	}
	h.networked = true
	return nil
}

// Reason is the refusal on one line, cut to reasonMax bytes: what naru-hostd and a rolled-back
// upgrade's outcome carry of it.
func Reason(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > reasonMax {
		s = s[:reasonMax-3] + "..."
	}
	return s
}
