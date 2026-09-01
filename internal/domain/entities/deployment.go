package entities

import "time"

type DeploymentStatus string

const (
	DeploymentBuilding DeploymentStatus = "building"
	DeploymentRunning  DeploymentStatus = "running"
	DeploymentStopped  DeploymentStatus = "stopped"
	DeploymentFailed   DeploymentStatus = "failed"
	DeploymentCrashed  DeploymentStatus = "crashed" // exited unexpectedly, not via a stop request
)

// Deployment represents one attempt at building+running a project.
// A project can have many deployments over its lifetime; Project.CurrentDeploymentID
// points at whichever one is (or was last) live. This is what gives you rollback
// and crash history instead of overwriting a single row's status/port in place.
type Deployment struct {
	ID        string
	ProjectID string

	ImageRef    string // what got built, e.g. "launchpad/launchpad-test:7f3a9c"
	ContainerID string // what's running — empty until Start succeeds

	Status ProjectDeployStatusAlias // see note below

	Port int // only meaningful if runtime publishes host ports; unused on shared-network setups

	FailureReason string // human-readable, set on Failed/Crashed
	ExitCode      *int   // nil while running/building

	CreatedAt time.Time
	StartedAt *time.Time
	StoppedAt *time.Time
}

// ProjectDeployStatusAlias exists only so Deployment.Status has a distinct
// type name from Project.Status in code review / autocomplete X— same
// underlying type as DeploymentStatus. If that's more confusing than
// helpful, just use DeploymentStatus directly instead.
type ProjectDeployStatusAlias = DeploymentStatus

func NewDeployment(projectID, imageRef string) *Deployment {
	return &Deployment{
		ProjectID: projectID,
		ImageRef:  imageRef,
		Status:    DeploymentBuilding,
	}
}