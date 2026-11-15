package signer

// SetStageHook makes the commit of the authorities call hook before each step, and returns the
// function that puts the hook back.
func SetStageHook(hook func(step string) error) (restore func()) {
	stageHook = hook
	return func() { stageHook = nil }
}
