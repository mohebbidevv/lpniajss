package application

import (
	"time"

	"golaunch/internal/domain/entities"
)

// DeploymentSummary is what a project's deployment history exposes — a
// rollback picker's raw material. ImageRef is deliberately omitted: it's
// an internal registry tag, not something a client needs to make the
// choice of which deployment to roll back to.
type DeploymentSummary struct {
	ID            string     `json:"id"`
	Status        string     `json:"status"`
	IsCurrent     bool       `json:"is_current"`
	FailureReason string     `json:"failure_reason,omitempty"`
	ExitCode      *int       `json:"exit_code,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	StoppedAt     *time.Time `json:"stopped_at,omitempty"`
	// CanRollBackTo is false when this deployment never produced a usable
	// image (failed before the build finished) — nothing to start from.
	CanRollBackTo bool `json:"can_roll_back_to"`
}

func newDeploymentSummary(d *entities.Deployment, currentID *string) DeploymentSummary {
	return DeploymentSummary{
		ID:            d.ID,
		Status:        string(d.Status),
		IsCurrent:     currentID != nil && *currentID == d.ID,
		FailureReason: d.FailureReason,
		ExitCode:      d.ExitCode,
		CreatedAt:     d.CreatedAt,
		StartedAt:     d.StartedAt,
		StoppedAt:     d.StoppedAt,
		CanRollBackTo: d.ImageRef != "",
	}
}
