package inventory

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/garamsh/naru/lib/orchapi"
)

const (
	maxLabels     = 64
	maxLabelKey   = 253
	maxLabelValue = 63
)

var (
	hostIDPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	labelKey      = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_./]*[A-Za-z0-9])?$`)
	labelValue    = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func validHostID(id string) error {
	if !hostIDPattern.MatchString(id) {
		return invalid("host ID %q is not a DNS label", id)
	}
	return nil
}

// validAttributes checks a region (a DNS label, or empty) and labels (a key of a name and an optional DNS-subdomain prefix, and a value).
func validAttributes(region string, labels map[string]string) error {
	if region != "" && !hostIDPattern.MatchString(region) {
		return invalid("region %q is not a DNS label", region)
	}
	if len(labels) > maxLabels {
		return invalid("at most %d labels", maxLabels)
	}
	for k, v := range labels {
		if len(k) > maxLabelKey || !labelKey.MatchString(k) {
			return invalid("label key %q is not a label key", k)
		}
		if len(v) > maxLabelValue || !labelValue.MatchString(v) {
			return invalid("label value %q of %q is not a label value", v, k)
		}
	}
	return nil
}

// The bounds of what a joining host reports of its hardware: orch keeps it, and the host is not
// yet trusted.
const (
	maxDevices = 64
	maxText    = 255
)

func checkHardware(h orchapi.Hardware) error {
	if h.CPUs < 0 || len(h.Disks) > maxDevices || len(h.Links) > maxDevices {
		return invalid("the hardware report has a negative count or more than %d disks or links", maxDevices)
	}
	for _, d := range h.Disks {
		if len(d.Name) > maxText || len(d.Model) > maxText {
			return invalid("a disk's name or model is longer than %d bytes", maxText)
		}
	}
	for _, l := range h.Links {
		if len(l.Name) > maxText || len(l.MAC) > maxText {
			return invalid("a link's name or address is longer than %d bytes", maxText)
		}
	}
	return nil
}

// normalizeCode folds what an operator typed to the form of a pairing code without its hyphen.
func normalizeCode(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))
}
