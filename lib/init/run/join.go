package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/lib/init/apply"
	"github.com/garamsh/naru/lib/init/config"
)

// joinService is the derived service that joins the host to an orch (config.joinService).
const joinService = "naru-hostd"

// exits tells when a service has exited with status 0 and stays stopped.
type exits interface {
	Exited(name string) <-chan struct{}
}

// applier is the apply engine as the handoff reaches it.
type applier interface {
	Config() (int, config.NodeConfig)
	Apply(apply.Request) (apply.Result, error)
}

// forbiddenAfterJoin are the sections a configuration that joining brings back may not carry: they
// make the host an orch, a signer or a joiner again.
var forbiddenAfterJoin = []string{"signer", "orch", "join"}

// handOff waits for the join service to exit with status 0 and applies the configuration it wrote
// to path as the whole configuration in reboot mode (ADR 0058), then calls reboot. Whatever it
// cannot apply is logged and set aside as path+".refused", and the join service is not started
// again before the next boot. It returns when the handoff is done or ctx ends.
func handOff(ctx context.Context, services exits, engine applier, path string, reboot func(), log *slog.Logger) {
	select {
	case <-services.Exited(joinService):
	case <-ctx.Done():
		return
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		log.Info("the join service exited and wrote no configuration", "path", path)
		return
	}
	if err == nil {
		var rev int
		if rev, err = applyJoined(engine, data); err == nil {
			if err := os.Remove(path); err != nil {
				log.Error("remove the join configuration", "path", path, "err", err)
			}
			log.Info(fmt.Sprintf("join configuration applied revision=%d", rev))
			reboot()
			return
		}
	}
	log.Error("join configuration refused", "reason", err.Error())
	if err := os.Rename(path, path+".refused"); err != nil {
		log.Error("set the join configuration aside", "path", path, "err", err)
	}
}

// applyJoined applies data as a whole configuration in reboot mode and returns the revision stored.
func applyJoined(engine applier, data []byte) (int, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return 0, fmt.Errorf("not a YAML mapping: %w", err)
	}
	spec, _ := doc["spec"].(map[string]any)
	for _, section := range forbiddenAfterJoin {
		if _, ok := spec[section]; ok {
			return 0, fmt.Errorf("spec.%s is not allowed in the configuration a join brings back", section)
		}
	}
	rev, _ := engine.Config()
	res, err := engine.Apply(apply.Request{Revision: rev, Config: doc, Mode: apply.Reboot})
	if err != nil {
		return 0, err
	}
	return res.Revision, nil
}
