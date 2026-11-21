package volumes

// Quota limits the blocks a directory's tree may use, and measures the room the file system has.
type Quota interface {
	// Enabled returns an error unless the file system holding dir enforces project quotas.
	Enabled(dir string) error
	// Set gives dir the project and limits the project to bytes; dir's files and the ones made in
	// it take the project. A limit of zero lifts it.
	Set(dir string, project uint32, bytes int64) error
	// Free returns the bytes the file system holding dir has left.
	Free(dir string) (int64, error)
}
