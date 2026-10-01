//go:build linux

package diskspace

import "syscall"

// Available reports the bytes free to an unprivileged writer at path.
//
// Bavail, not Bfree: Bfree counts blocks reserved for root, which the Docker
// daemon's writes cannot use. Reporting Bfree would let a build start with
// space that isn't really there.
func Available(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
