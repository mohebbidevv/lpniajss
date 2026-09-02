package hostexec

import (
	"fmt"
	"net"
)

// allocatePort asks the kernel for a free port and immediately releases it.
// There is an inherent race between releasing and the app binding, but it is
// far smaller than the failure mode it replaces: a monotonic counter never
// reuses a port, so every restart leaks the whole range it burned through
// and eventually collides with something else on the host.
func allocatePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
