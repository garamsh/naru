// Package groups keeps groups of microVMs at their declared count, template and placement.
package groups

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Form is how a microVM boots.
type Form string

const (
	FormImage Form = "image"
	FormOS    Form = "os"
)

// Disk is an extra disk of a microVM.
type Disk struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
	ReadOnly  bool   `json:"readOnly"`
}

// Rule is one network policy rule.
type Rule struct {
	Prefix   string `json:"prefix"`
	Protocol string `json:"protocol"`
	Ports    []int  `json:"ports"`
}

// NetworkPolicy is a microVM's egress and ingress rules.
type NetworkPolicy struct {
	Egress  []Rule `json:"egress"`
	Ingress []Rule `json:"ingress"`
}

// Template is the microVM spec as the host API takes it.
type Template struct {
	Form      Form          `json:"form"`
	CPUs      int           `json:"cpus"`
	MemoryMiB int           `json:"memoryMiB"`
	Version   string        `json:"version"`
	Disks     []Disk        `json:"disks"`
	Network   NetworkPolicy `json:"network"`
	// NodeVersion is the naru-node version an OS-form microVM boots; a host places it only
	// when the version is in the host's NodeVersions.
	NodeVersion string `json:"nodeVersion"`
}

// Hash identifies the template: two templates with one hash are the same template.
func (t Template) Hash() string {
	b, err := json.Marshal(t)
	if err != nil {
		panic(fmt.Sprintf("groups: marshal template: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// LabelPreference orders placement: a host carrying Key=Value scores Weight.
type LabelPreference struct {
	Key    string
	Value  string
	Weight int
}

// Rollout bounds how a template change or a drain moves members.
type Rollout struct {
	MaxUnavailable int
	MaxSurge       int
}

// GroupSpec is what an operator declares for a group.
type GroupSpec struct {
	Name            string
	Count           int
	Region          string
	RequiredLabels  map[string]string
	PreferredLabels []LabelPreference
	Template        Template
	Rollout         Rollout
	MinAvailable    int
	Stateful        bool
	// FenceTTL is how far ahead a stateful member's fence deadline is renewed.
	FenceTTL time.Duration
}

var dnsLabel = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)

// ErrInvalidSpec is wrapped by every error Validate returns.
var ErrInvalidSpec = errors.New("invalid group spec")

// Validate reports why the spec cannot be run, or nil.
func (g GroupSpec) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidSpec, fmt.Sprintf(format, a...))
	}
	switch {
	case !dnsLabel.MatchString(g.Name):
		return bad("name %q is not a DNS label of at most 40 characters", g.Name)
	case g.Count < 0:
		return bad("count %d is negative", g.Count)
	case g.Region == "":
		return bad("region is empty")
	case g.MinAvailable < 0 || g.MinAvailable > g.Count:
		return bad("minAvailable %d is outside 0..count %d", g.MinAvailable, g.Count)
	case g.Rollout.MaxUnavailable < 0 || g.Rollout.MaxSurge < 0:
		return bad("rollout bounds are negative")
	case g.Template.CPUs <= 0 || g.Template.MemoryMiB <= 0:
		return bad("template needs cpus and memoryMiB above zero")
	case g.Template.Form != FormImage && g.Template.Form != FormOS:
		return bad("template form %q is neither image nor os", g.Template.Form)
	case g.Stateful && g.FenceTTL <= 0:
		return bad("a stateful group needs a fenceTTL")
	case g.Stateful && g.Rollout.MaxSurge != 0:
		return bad("a stateful group may not surge: two instances would run at once")
	case g.floor() >= g.Count && g.Rollout.MaxSurge == 0 && g.Count > 0:
		return bad("rollout can never remove a member: minAvailable and maxUnavailable leave no room and maxSurge is 0")
	}
	return nil
}

// floor is the fewest members that must stay available while members are removed.
func (g GroupSpec) floor() int {
	return max(g.MinAvailable, g.Count-g.Rollout.MaxUnavailable)
}

// Phase is where a member is in its life.
type Phase string

const (
	PhasePending       Phase = "pending"
	PhaseRunning       Phase = "running"
	PhaseDraining      Phase = "draining"
	PhaseFencedWaiting Phase = "fenced-waiting"
	PhaseGone          Phase = "gone"
)

// Member is the record of one microVM of a group. Only pending and gone are stored: the
// other phases are computed from observation on every pass.
type Member struct {
	ID           string
	Group        string
	Host         string
	MachineID    string
	TemplateHash string
	Phase        Phase
	// LastSeen is when the record was last written because the member was seen on its host.
	LastSeen time.Time
	// FenceDeadline is the last deadline orch renewed for a stateful member.
	FenceDeadline time.Time
	// Revision is the store's compare-and-swap token; 0 means the record does not exist.
	Revision int64
}

// ordinal is the number a member's ID carries after its group's name, or 0.
func (m Member) ordinal() int {
	n, err := strconv.Atoi(strings.TrimPrefix(m.ID, m.Group+"-"))
	if err != nil {
		return 0
	}
	return n
}

// Capacity is what a host can still give.
type Capacity struct {
	CPUs      int
	MemoryMiB int
}

// MachineState is a microVM's state on its host.
type MachineState string

const (
	MachineCreated MachineState = "created"
	MachineRunning MachineState = "running"
	MachineStopped MachineState = "stopped"
)

// Machine is a microVM as its host reports it.
type Machine struct {
	ID    string
	State MachineState
}

// Host is a host as orch sees it.
type Host struct {
	ID           string
	Region       string
	Labels       map[string]string
	Ready        bool
	Draining     bool
	Capacity     Capacity
	NodeVersions []string
	Machines     []Machine
}

// Fence is a Machine's fence as its host reports it.
type Fence struct {
	Deadline time.Time
	LapsedAt time.Time
}
