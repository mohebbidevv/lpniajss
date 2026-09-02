package hostexec

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"golaunch/internal/domain/entities"
)

const (
	readyPollInterval = 200 * time.Millisecond
	readyDialTimeout  = time.Second
)

// Endpoint is a loopback host port: host-exec processes run directly on the
// host, so the control plane and the proxy reach them the same way.
func (r *HostExecRuntime) Endpoint(ctx context.Context, handle entities.RuntimeHandle) (string, error) {
	tp, ok := r.get(string(handle))
	if !ok {
		return "", fmt.Errorf("no such process: %s", handle)
	}
	if tp.Port == 0 {
		return "", fmt.Errorf("process %s has no port", handle)
	}
	return net.JoinHostPort("localhost", strconv.Itoa(tp.Port)), nil
}

// WaitReady dials the process's port until it accepts a connection. A real
// probe is possible here — unlike the Docker runtime, the port is on this
// host — so readiness means the app is genuinely listening.
func (r *HostExecRuntime) WaitReady(ctx context.Context, handle entities.RuntimeHandle, timeout time.Duration) error {
	tp, ok := r.get(string(handle))
	if !ok {
		return fmt.Errorf("no such process: %s", handle)
	}

	addr, err := r.Endpoint(ctx, handle)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(readyPollInterval)
	defer ticker.Stop()

	dialer := net.Dialer{Timeout: readyDialTimeout}

	for {
		if exited, code := tp.IsExited(); exited {
			return fmt.Errorf("process %s exited during startup with code %v", handle, derefCode(code))
		}

		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			return nil
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("process %s did not become ready within %s", handle, timeout)
		}
	}
}

func derefCode(code *int) int {
	if code == nil {
		return -1
	}
	return *code
}
