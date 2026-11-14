package inventory

import (
	"fmt"
	"regexp"
	"strings"
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

// validAttributes checks a region (a DNS label, or empty) and labels (Kubernetes label syntax).
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

// normalizeCode folds what an operator typed to the form of a pairing code without its hyphen.
func normalizeCode(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))
}
