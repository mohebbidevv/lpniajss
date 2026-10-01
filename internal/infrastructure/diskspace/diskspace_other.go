//go:build !linux

package diskspace

import "errors"

// Available is unimplemented off Linux. Callers treat an error as "cannot
// tell" and proceed, so a developer on another OS is never blocked by a
// check the platform can't answer.
func Available(path string) (uint64, error) {
	return 0, errors.New("diskspace: unsupported platform")
}
