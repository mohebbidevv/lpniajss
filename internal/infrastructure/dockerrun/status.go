package dockerrun

import (
	"github.com/docker/docker/api/types/container"

	"golaunch/internal/domain/entities"
)

// statusFromState maps an inspect result. OOM is checked before the exit
// code because an OOM kill also reports as a plain exit, and the distinction
// is the difference between "the app crashed" and "the app needs more memory".
func statusFromState(s *container.State) entities.RuntimeStatus {
	if s == nil {
		return entities.RuntimeStatus{State: entities.RuntimeStateNotFound}
	}
	if s.Running {
		return entities.RuntimeStatus{State: entities.RuntimeStateRunning}
	}

	code := s.ExitCode
	if s.OOMKilled {
		return entities.RuntimeStatus{State: entities.RuntimeStateOOMKilled, ExitCode: &code}
	}
	return entities.RuntimeStatus{State: entities.RuntimeStateExited, ExitCode: &code}
}

// statusFromSummary maps a list result. The list endpoint carries no exit
// code or OOM flag, so callers that need either must follow up with Status;
// List is for cheaply answering "what is alive", which it does exactly.
func statusFromSummary(state container.ContainerState) entities.RuntimeStatus {
	switch state {
	case container.StateRunning, container.StateRestarting, container.StatePaused:
		return entities.RuntimeStatus{State: entities.RuntimeStateRunning}
	default:
		return entities.RuntimeStatus{State: entities.RuntimeStateExited}
	}
}
