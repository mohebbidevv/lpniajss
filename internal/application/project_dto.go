package application

import (
	"time"

	"golaunch/internal/domain/entities"
)

// ProjectSummary is what /projects responses expose for a project — never
// the raw entity, since entities.Project carries fields (SourceLocation,
// UniqueKey, Port) that should never reach a client.
type ProjectSummary struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Slug       string    `json:"slug"`
	Status     string    `json:"status"`
	SourceType string    `json:"source_type"`
	LiveURL    string    `json:"live_url"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func newProjectSummary(p *entities.Project, domain string) ProjectSummary {
	return ProjectSummary{
		ID:         p.ID,
		Name:       p.Name,
		Slug:       p.Slug,
		Status:     string(p.Status),
		SourceType: p.SourceType,
		LiveURL:    "http://" + p.Slug + "." + domain,
		CreatedAt:  p.CreatedAt,
		UpdatedAt:  p.UpdatedAt,
	}
}

// ProjectDetail extends ProjectSummary with the current deployment's
// status; Deployment is nil for a project that has never been deployed.
type ProjectDetail struct {
	ProjectSummary
	Deployment *DeploymentStatusInfo `json:"deployment,omitempty"`
}

type DeploymentStatusInfo struct {
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason,omitempty"`
	ExitCode      *int   `json:"exit_code,omitempty"`
}

func newDeploymentStatusInfo(d *entities.Deployment) *DeploymentStatusInfo {
	return &DeploymentStatusInfo{
		Status:        string(d.Status),
		FailureReason: d.FailureReason,
		ExitCode:      d.ExitCode,
	}
}
