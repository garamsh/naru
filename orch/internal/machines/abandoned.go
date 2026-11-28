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
			out = append(out, orchapi.AbandonedVolumes{Caller: r.Caller, Name: r.Name, ID: r.ID, Host: a.Host, Epoch: a.Epoch, Volumes: slices.Clone(a.Volumes), At: a.At, Role: a.role(), Deleting: a.Deleting})
		}
	}
	return out, nil
}

// DeleteAbandonedVolumes forgets the volumes the caller's Machine left at the epoch on the host,
// or, for a source copy, asks the reconciler to delete it.
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
		left := slices.Clone(r.Abandoned)
		switch {
		case left[i].role() != RoleSource:
			// Nothing on an abandoned host is reached: the operator who fenced it attested that it
			// comes back only reset or reinstalled, so its disks go with that.
			left = slices.Delete(left, i, i+1)
		case left[i].Deleting:
			return nil
		default:
			// A source copy is on a host that answers: the reconciler deletes it there, then frees
			// its disk and forgets the entry.
			left[i].Deleting = true
		}
		r.Abandoned = left
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

// deleteCopies carries out each operator's delete of a source copy whose host answers and has no
// work of an earlier pass running: the copy's microVM goes with its disks, then the entry and the
// disk its host's allocation kept for it, in one write.
func (r *Reconciler) deleteCopies(ctx context.Context, p *pass) []error {
	var errs []error
	for _, e := range p.entries {
		if e.Move != nil {
			continue
		}
		for _, a := range slices.Clone(e.Abandoned) {
			if a.role() != RoleSource || !a.Deleting || p.busy[a.Host] {
				continue
			}
			if _, ok := p.answering(a.Host); !ok {
				continue
			}
			if done, err := r.unrouted(ctx, e, a); err != nil || !done {
				if err != nil {
					errs = append(errs, err)
				}
				continue
			}
			name := Binding{Host: a.Host, Epoch: a.Epoch}.MicroVM(e.ID)
			if err := r.cfg.Hosts.Delete(ctx, a.Host, name); err != nil {
				errs = append(errs, fmt.Errorf("delete the source copy %s on %s: %w", name, a.Host, err))
				continue
			}
			if err := r.forgetCopy(ctx, p, e, a); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// unrouted reports whether no proxy forwards on a table that routes the Machine to the source copy
// a, so its address may be given to another microVM once it is deleted.
func (r *Reconciler) unrouted(ctx context.Context, e *Entry, a AbandonedEpoch) (bool, error) {
	if a.role() != RoleSource || a.Unrouted == nil || r.cfg.Routes == nil {
		return true, nil
	}
	done, err := r.cfg.Routes.Delivered(ctx, a.Unrouted.Rev, a.Unrouted.At)
	if err != nil {
		return false, fmt.Errorf("ask whether the proxies stopped routing %s/%s to %s: %w", e.Caller, e.Name, a.Host, err)
	}
	return done, nil
}

// forgetCopy removes the source copy a from the record and frees the disk its host kept for it.
func (r *Reconciler) forgetCopy(ctx context.Context, p *pass, e *Entry, a AbandonedEpoch) error {
	return r.allocate(ctx, p, e, a.Host, amounts{}.minus(diskOf(e.Spec)), func(rec *Record) {
		rec.Abandoned = slices.DeleteFunc(slices.Clone(rec.Abandoned), func(b AbandonedEpoch) bool {
			return b.role() == RoleSource && b.Host == a.Host && b.Epoch == a.Epoch
		})
	})
}
