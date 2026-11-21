package volumes

// Mounter binds and unbinds the directories and device nodes a Pod sees.
type Mounter interface {
	Bind(source, target string, readOnly bool) error
	Unmount(target string) error
	Mounted(target string) (bool, error)
}
