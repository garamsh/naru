package signer

// SetStageHook makes the commit of the authorities call hook before each step, and returns the
// function that puts the hook back.
func SetStageHook(hook func(step string) error) (restore func()) {
	stageHook = hook
	return func() { stageHook = nil }
}

// StatusOf is statusOf, for the tests of the package's answers.
func StatusOf(err error) int { return statusOf(err) }

// SetRenewHook makes RenewHost call hook between its verification and its lock, and returns the
// function that puts the hook back.
func SetRenewHook(hook func()) (restore func()) {
	renewHook = hook
	return func() { renewHook = nil }
}
