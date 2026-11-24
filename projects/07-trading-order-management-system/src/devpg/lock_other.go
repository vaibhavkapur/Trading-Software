//go:build !unix

package devpg

// lockFile is a no-op where flock is unavailable. Start one process at a time
// on these platforms until the binaries have been unpacked once.
func lockFile(string) (unlock func(), err error) {
	return func() {}, nil
}
