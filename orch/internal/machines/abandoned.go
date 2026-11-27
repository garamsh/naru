package machines

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/garamsh/naru/lib/orchapi"
)

// AbandonedVolumes lists every Machine's volumes left on an abandoned host.
func (s *service) AbandonedVolumes(ctx context.Context) ([]orchapi.AbandonedVolumes, error) {
	rs, err := s.store.All(ctx)
	if err != nil {
		return nil, err
	}
	out := []orchapi.AbandonedVolumes{}
	for _, r := range rs {
		for _, a := range r.Abandoned {
			out = append(out, orchapi.AbandonedVolumes{Caller: r.Caller, Name: r.Name, ID: r.ID, Host: a.Host, Epoch: a.Epoch, Volumes: slices.Clone(a.Volumes), At: a.At})
		}
	}
	return out, nil
}

// DeleteAbandonedVolumes forgets the volumes the caller's Machine left at the epoch on the host.
// Nothing on an abandoned host is reached: the operator who fenced it attested that it comes back
// only reset or reinstalled, so its disks go with that.
func (s *service) DeleteAbandonedVolumes(ctx context.Context, caller, name, host string, epoch uint64) error {
	for range retries {
		r, rev, err := s.store.Machine(ctx, caller, name)
		if err != nil {
			return notFound(err, "Machine", caller+"/"+name)
		}
		i := slices.IndexFunc(r.Abandoned, func(a AbandonedEpoch) bool { return a.Host == host && a.Epoch == epoch })
		if i < 0 {
			return fmt.Errorf("%w: %s/%s left no volumes at epoch %d on %s", ErrNotFound, caller, name, epoch, host)
		}
		r.Abandoned = slices.Delete(slices.Clone(r.Abandoned), i, i+1)
		_, err = s.store.Update(ctx, r, rev, nil)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrRaced) {
			return err
		}
	}
	return fmt.Errorf("delete the abandoned volumes of %s/%s: %d writes raced", caller, name, retries)
}
