package config

// StatusKey is the top-level key of the document GET /v1/config returns that holds Status.
const StatusKey = "status"

// Status is what GET /v1/config reports of the Node beside its configuration: observed, never
// declared, so no file and no apply may carry it.
type Status struct {
	Version string `yaml:"version"`
	BootID  string `yaml:"bootId"`
	// Slot is "default" or "trial".
	Slot string `yaml:"slot"`
	// LastUpgrade is how the last upgrade ended, and absent when none has.
	LastUpgrade *UpgradeStatus `yaml:"lastUpgrade,omitempty"`
	// Revision is the revision of the configuration the document carries.
	Revision int             `yaml:"revision"`
	Services []ServiceStatus `yaml:"services"`
}

// UpgradeStatus is the last upgrade: the versions it went from and to, what became of it, why
// when it was rolled back, and when init decided (RFC 3339).
type UpgradeStatus struct {
	From   string `yaml:"from"`
	To     string `yaml:"to"`
	Result string `yaml:"result"`
	Reason string `yaml:"reason,omitempty"`
	Time   string `yaml:"time"`
}

// ServiceStatus is the state init supervises one service in and how often it restarted it.
type ServiceStatus struct {
	Name     string `yaml:"name"`
	State    string `yaml:"state"`
	Restarts int    `yaml:"restarts"`
}
