package entities

import "time"

type ProjectStatus string

const (
	StatusPending  ProjectStatus = "pending"
	StatusBuilding ProjectStatus = "building"
	StatusRunning  ProjectStatus = "running"
	StatusStopped  ProjectStatus = "stopped"
	StatusFailed   ProjectStatus = "failed"
)

type Project struct {
	ID             string
	Name           string // original uploaded filename
	Slug           string // public subdomain
	UniqueKey      string // internal ID
	SourceType     string // "zip" for now, room for "git" later
	SourceLocation string
	Status         ProjectStatus
	Port           int // legacy — host-exec runtime only. Docker runtime won't use this once on the shared network.

	CurrentDeploymentID *string // NEW — FK to the deployment currently live for this project, nil if never deployed

	RepoURL *string // set only for SourceType "git" — the origin to re-sync from on every deploy
	RepoRef *string // branch/tag to track; nil means "the remote's default branch"

	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewProject constructs a project in its initial pending state.
// slug should already be validated/uniqued by the caller before this is called.
func NewProject(name, slug, uniqueKey, sourceType, sourceLocation string) *Project {
	return &Project{
		Name:           name,
		Slug:           slug,
		UniqueKey:      uniqueKey,
		SourceType:     sourceType,
		SourceLocation: sourceLocation,
		Status:         StatusPending,
	}
}

// NewGitProject constructs a project whose source is a git repo. sourceLocation
// is the local directory it's cloned into (or will be cloned into); repoURL/ref
// are kept so DeployPipeline can re-sync from origin on every subsequent deploy.
func NewGitProject(name, slug, uniqueKey, sourceLocation, repoURL string, ref *string) *Project {
	return &Project{
		Name:           name,
		Slug:           slug,
		UniqueKey:      uniqueKey,
		SourceType:     "git",
		SourceLocation: sourceLocation,
		Status:         StatusPending,
		RepoURL:        &repoURL,
		RepoRef:        ref,
	}
}