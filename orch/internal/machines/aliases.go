package machines

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/garamsh/naru/lib/machineapi"
)

// The failures of an alias the Machine API's transport translates to its codes.
var (
	// ErrAliasTaken is a hostname another caller holds.
	ErrAliasTaken = errors.New("another caller holds the hostname")
	// ErrDomainNotGranted is a hostname outside the zone in no domain granted to the caller.
	ErrDomainNotGranted = errors.New("the hostname is outside the zone and in no domain granted to the caller")
)

// MaxAliasesPerMachine bounds a Machine's aliases, which its delete removes in one transaction.
const MaxAliasesPerMachine = 64

// Alias is an alias as orch stores it, under /naru/orch/aliases/<hostname>, so a hostname is held by
// one caller across the fleet (ADR 0112 item 3). Certificate and Key are an alias outside the zone's
// own pair, which only the routes the proxies' hosts receive carry; Shown is what a read shows of the
// certificate.
type Alias struct {
	Hostname    string                       `json:"hostname"`
	Caller      string                       `json:"caller"`
	Machine     string                       `json:"machine"`
	MachineID   string                       `json:"machineId"`
	Service     string                       `json:"service"`
	Certificate string                       `json:"certificate,omitempty"`
	Key         string                       `json:"key,omitempty"`
	Shown       *machineapi.AliasCertificate `json:"shown,omitempty"`
}

// AliasEntry is a stored alias and the revision of its last write.
type AliasEntry struct {
	Alias
	Rev int64
}

// Answer is the alias as a read answers it: the key never, only that it is set.
func (a Alias) Answer() machineapi.Alias {
	out := machineapi.Alias{Hostname: a.Hostname, Machine: a.Machine, Service: a.Service, Certificate: a.Shown}
	if a.Key != "" {
		out.Key = machineapi.Redacted
	}
	return out
}

// Caller is what the operator grants a caller, under /naru/orch/callers/<name>: the domains outside
// the zone it may publish aliases in.
type Caller struct {
	Name         string   `json:"name"`
	AliasDomains []string `json:"aliasDomains"`
}

// PutAlias publishes hostname for the caller's Machine r.Machine. The first caller to create a
// hostname holds it; the same request again answers the alias as it is, and another one of the
// caller's is ErrConflict. created is true when this call recorded it.
func (s *service) PutAlias(ctx context.Context, caller, hostname string, req machineapi.AliasRequest) (machineapi.Alias, bool, error) {
	inZone, err := machineapi.AliasInZone(hostname, s.zone)
	if err != nil {
		return machineapi.Alias{}, false, fmt.Errorf("%w: %v", machineapi.ErrInvalid, err)
	}
	a := Alias{Hostname: hostname, Caller: caller, Machine: req.Machine, Service: req.Service, Certificate: req.Certificate, Key: req.Key}
	if inZone {
		if req.Certificate != "" || req.Key != "" {
			return machineapi.Alias{}, false, fmt.Errorf("%w: an alias in the zone %s takes no certificate or key", machineapi.ErrInvalid, s.zone)
		}
	} else {
		grant, err := s.store.Caller(ctx, caller)
		if err != nil {
			return machineapi.Alias{}, false, err
		}
		if !slices.ContainsFunc(grant.AliasDomains, func(d string) bool { return machineapi.InDomain(hostname, d) }) {
			return machineapi.Alias{}, false, fmt.Errorf("%w: %s", ErrDomainNotGranted, hostname)
		}
		shown, err := machineapi.CheckAliasCertificate(hostname, req.Certificate, req.Key, s.clock.Now())
		if err != nil {
			return machineapi.Alias{}, false, fmt.Errorf("%w: %v", machineapi.ErrInvalid, err)
		}
		a.Shown = &shown
	}
	for range retries {
		m, mrev, err := s.store.Machine(ctx, caller, req.Machine)
		if errors.Is(err, ErrNotFound) || err == nil && m.Status.Phase == machineapi.PhaseDeleting {
			return machineapi.Alias{}, false, fmt.Errorf("%w: %s", ErrMachineNotFound, req.Machine)
		}
		if err != nil {
			return machineapi.Alias{}, false, err
		}
		if len(m.Spec.Services) == 0 {
			return machineapi.Alias{}, false, fmt.Errorf("%w: the Machine %s has no service", machineapi.ErrInvalid, req.Machine)
		}
		if a.Service == "" {
			a.Service = m.Spec.Services[0].Name
		}
		if !slices.ContainsFunc(m.Spec.Services, func(sv machineapi.Service) bool { return sv.Name == a.Service }) {
			return machineapi.Alias{}, false, fmt.Errorf("%w: service: the Machine %s has no service %q", machineapi.ErrInvalid, req.Machine, a.Service)
		}
		a.MachineID = m.ID
		all, err := s.store.Aliases(ctx)
		if err != nil {
			return machineapi.Alias{}, false, err
		}
		if n := len(slices.DeleteFunc(all, func(e AliasEntry) bool { return e.MachineID != m.ID || e.Hostname == hostname })); n >= MaxAliasesPerMachine {
			return machineapi.Alias{}, false, fmt.Errorf("%w: the Machine %s holds %d aliases", machineapi.ErrInvalid, req.Machine, n)
		}
		held, _, err := s.store.Alias(ctx, hostname)
		switch {
		case err == nil && held.Caller != caller:
			return machineapi.Alias{}, false, fmt.Errorf("%w: %s", ErrAliasTaken, hostname)
		case err == nil && sameAlias(held, a):
			return held.Answer(), false, nil
		case err == nil:
			return machineapi.Alias{}, false, fmt.Errorf("%w: the alias %s exists for another Machine, service or certificate", ErrConflict, hostname)
		case !errors.Is(err, ErrNotFound):
			return machineapi.Alias{}, false, err
		}
		err = s.store.CreateAlias(ctx, a, m, mrev)
		if err == nil {
			return a.Answer(), true, nil
		}
		if !errors.Is(err, ErrRaced) {
			return machineapi.Alias{}, false, err
		}
	}
	return machineapi.Alias{}, false, fmt.Errorf("alias %s: %d writes raced", hostname, retries)
}

// sameAlias reports whether held is the alias a request asks for again.
func sameAlias(held, a Alias) bool {
	return held.Machine == a.Machine && held.MachineID == a.MachineID && held.Service == a.Service &&
		held.Certificate == a.Certificate && held.Key == a.Key
}

// Alias returns the caller's alias; another caller's is ErrNotFound.
func (s *service) Alias(ctx context.Context, caller, hostname string) (machineapi.Alias, error) {
	a, _, err := s.store.Alias(ctx, hostname)
	if err == nil && a.Caller != caller {
		err = ErrNotFound
	}
	if err != nil {
		return machineapi.Alias{}, notFound(err, "alias", hostname)
	}
	return a.Answer(), nil
}

// DeleteAlias removes the caller's alias; another caller's is ErrNotFound.
func (s *service) DeleteAlias(ctx context.Context, caller, hostname string) error {
	for range retries {
		a, rev, err := s.store.Alias(ctx, hostname)
		if err == nil && a.Caller != caller {
			err = ErrNotFound
		}
		if err != nil {
			return notFound(err, "alias", hostname)
		}
		err = s.store.DeleteAlias(ctx, a, rev)
		if !errors.Is(err, ErrRaced) {
			return err
		}
	}
	return fmt.Errorf("delete alias %s: %d writes raced", hostname, retries)
}

// PutCaller records the domains outside the zone the caller may publish aliases in. Each is a
// lowercase DNS name of two labels at least, and neither the zone nor in it. An alias already
// published stays when its domain leaves the grant.
func (s *service) PutCaller(ctx context.Context, c Caller) (Caller, error) {
	if err := machineapi.CheckName(c.Name); err != nil {
		return Caller{}, &machineapi.FieldError{Field: "name", Problem: "is not a DNS label"}
	}
	domains := []string{}
	for i, d := range c.AliasDomains {
		labels := strings.Split(d, ".")
		ok := len(labels) >= 2 && len(d) <= 253
		for _, l := range labels {
			ok = ok && machineapi.CheckName(l) == nil
		}
		if !ok || machineapi.InDomain(d, s.zone) || machineapi.InDomain(s.zone, d) {
			return Caller{}, &machineapi.FieldError{Field: fmt.Sprintf("aliasDomains[%d]", i), Problem: "is not a lowercase DNS name of two labels or more apart from the zone"}
		}
		if !slices.Contains(domains, d) {
			domains = append(domains, d)
		}
	}
	slices.Sort(domains)
	c.AliasDomains = domains
	return c, s.store.PutCaller(ctx, c)
}

// Callers lists every caller the operator granted anything.
func (s *service) Callers(ctx context.Context) ([]Caller, error) { return s.store.Callers(ctx) }
