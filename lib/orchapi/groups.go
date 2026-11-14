package orchapi

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// GroupDisk is an extra disk of a group's microVMs.
type GroupDisk struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// GroupRule is one network policy rule of a group's microVMs.
type GroupRule struct {
	Prefix   string `json:"prefix"`
	Protocol string `json:"protocol"`
	Ports    []int  `json:"ports,omitempty"`
}

// GroupPolicy is the egress and ingress rules of a group's microVMs.
type GroupPolicy struct {
	Egress  []GroupRule `json:"egress,omitempty"`
	Ingress []GroupRule `json:"ingress,omitempty"`
}

// GroupTemplate is the microVM every member of a group is made from. Form is "image" or "os".
// Version names what boots: for the image form an image as repository@sha256:digest. NodeVersion
// is the naru-node version an OS-form microVM boots.
//
// Network names the virtual network the members join, or is empty for none. PodPrefixBits above
// zero gives each member a Pod prefix of that length out of the network's prefix. RootDiskBytes
// sizes the root disk of an image-form microVM and is required for it. UserData is the user-data
// of an OS-form microVM, used as written; empty gives "#cloud-config\n".
type GroupTemplate struct {
	Form          string      `json:"form"`
	CPUs          int         `json:"cpus"`
	MemoryMiB     int         `json:"memoryMiB"`
	Version       string      `json:"version,omitempty"`
	Disks         []GroupDisk `json:"disks,omitempty"`
	Policy        GroupPolicy `json:"policy,omitzero"`
	NodeVersion   string      `json:"nodeVersion,omitempty"`
	Network       string      `json:"network,omitempty"`
	PodPrefixBits int         `json:"podPrefixBits,omitempty"`
	RootDiskBytes int64       `json:"rootDiskBytes,omitempty"`
	UserData      string      `json:"userData,omitempty"`
}

// PreferredLabel scores a host that carries Key=Value by Weight when a member is placed.
type PreferredLabel struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Weight int    `json:"weight"`
}

// GroupRollout bounds how many members a template change or a drain moves at once.
type GroupRollout struct {
	MaxUnavailable int `json:"maxUnavailable"`
	MaxSurge       int `json:"maxSurge"`
}

// GroupSpec is what an operator declares for a group of microVMs.
type GroupSpec struct {
	Name            string            `json:"name"`
	Count           int               `json:"count"`
	Region          string            `json:"region"`
	RequiredLabels  map[string]string `json:"requiredLabels,omitempty"`
	PreferredLabels []PreferredLabel  `json:"preferredLabels,omitempty"`
	Template        GroupTemplate     `json:"template"`
	Rollout         GroupRollout      `json:"rollout"`
	MinAvailable    int               `json:"minAvailable"`
	Stateful        bool              `json:"stateful,omitempty"`
	// FenceTTLSeconds is how far ahead a stateful member's fence is renewed.
	FenceTTLSeconds int `json:"fenceTTLSeconds,omitempty"`
}

// The phases of a group member.
const (
	PhasePending       = "pending"
	PhaseRunning       = "running"
	PhaseDraining      = "draining"
	PhaseFencedWaiting = "fenced-waiting"
	PhaseGone          = "gone"
)

// GroupMember is one microVM of a group.
type GroupMember struct {
	ID           string `json:"id"`
	Host         string `json:"host"`
	TemplateHash string `json:"templateHash"`
	// Phase is computed from what the hosts report when Group.Observed is true, and is the stored
	// phase (pending or gone) when it is false.
	Phase    string    `json:"phase"`
	LastSeen time.Time `json:"lastSeen,omitzero"`
	// PodPrefix is the Pod prefix of the member's network that the member holds, or empty.
	PodPrefix string `json:"podPrefix,omitempty"`
}

// Group is a group with its members.
type Group struct {
	Spec GroupSpec `json:"spec"`
	// Hash identifies the template: a member whose hash differs is outdated.
	Hash string `json:"hash"`
	// Invalid is why the engine cannot run the spec, or empty.
	Invalid string `json:"invalid,omitempty"`
	// Observed is whether the members' phases were computed from the hosts just now.
	Observed bool          `json:"observed"`
	Members  []GroupMember `json:"members"`
}

// ApplyGroup creates the group or replaces its spec. spec.Name names it.
func (c *Client) ApplyGroup(ctx context.Context, spec GroupSpec) (Group, error) {
	var out Group
	return out, c.do(ctx, http.MethodPut, "/v1/groups/"+url.PathEscape(spec.Name), nil, spec, &out)
}

// Groups lists the groups with their members.
func (c *Client) Groups(ctx context.Context) ([]Group, error) {
	var out []Group
	return out, c.do(ctx, http.MethodGet, "/v1/groups", nil, nil, &out)
}

// Group returns one group with its members.
func (c *Client) Group(ctx context.Context, name string) (Group, error) {
	var out Group
	return out, c.do(ctx, http.MethodGet, "/v1/groups/"+url.PathEscape(name), nil, nil, &out)
}

// MicroVM is a microVM on an admitted host, as that host reports it.
type MicroVM struct {
	Host  string `json:"host"`
	Name  string `json:"name"`
	State string `json:"state"`
	// Group is the group whose member the microVM is; empty for one no group made.
	Group string `json:"group,omitempty"`
	// Image is the digest of the image an image-form microVM runs and BootSet the boot set an
	// OS-form one boots, as the host reports them; Network is the network the microVM is on.
	Image   string `json:"image,omitempty"`
	BootSet string `json:"bootSet,omitempty"`
	Network string `json:"network,omitempty"`
}

// MicroVMs lists the microVMs of every admitted host that answers.
func (c *Client) MicroVMs(ctx context.Context) ([]MicroVM, error) {
	var out []MicroVM
	return out, c.do(ctx, http.MethodGet, "/v1/microvms", nil, nil, &out)
}

// DeleteGroup removes a group that has no member left.
func (c *Client) DeleteGroup(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/groups/"+url.PathEscape(name), nil, nil, nil)
}
