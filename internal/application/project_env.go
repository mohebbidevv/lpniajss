package application

import (
	"context"
	"fmt"
	"regexp"

	"golaunch/internal/domain/repository"
)

// envKeyPattern matches a standard POSIX env var name — the same shape a
// shell or a Dockerfile ENV line accepts.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedEnvKeys can never be set by a project's own env vars — PORT is
// runtime-injected (the container port is fixed platform-side, not user
// choosable), so it's excluded here rather than left to silently collide.
var reservedEnvKeys = map[string]bool{"PORT": true}

const (
	maxEnvVars     = 50
	maxEnvKeyLen   = 128
	maxEnvValueLen = 4096
)

func validateEnvVars(vars map[string]string) error {
	if len(vars) > maxEnvVars {
		return fmt.Errorf("too many env vars (max %d)", maxEnvVars)
	}
	for k, v := range vars {
		if len(k) > maxEnvKeyLen || !envKeyPattern.MatchString(k) {
			return fmt.Errorf("invalid env var name %q — use letters, digits, underscore, not starting with a digit", k)
		}
		if reservedEnvKeys[k] {
			return fmt.Errorf("%q is reserved and can't be set", k)
		}
		if len(v) > maxEnvValueLen {
			return fmt.Errorf("value for %q is too long (max %d characters)", k, maxEnvValueLen)
		}
	}
	return nil
}

// ListProjectEnvUseCase returns a project's currently-stored env vars.
type ListProjectEnvUseCase struct {
	ProjectRepo repository.ProjectRepository
	EnvVarRepo  repository.EnvVarRepository
}

func NewListProjectEnvUseCase(projectRepo repository.ProjectRepository, envVarRepo repository.EnvVarRepository) *ListProjectEnvUseCase {
	return &ListProjectEnvUseCase{ProjectRepo: projectRepo, EnvVarRepo: envVarRepo}
}

func (uc *ListProjectEnvUseCase) Execute(ctx context.Context, projectID, userID string) (map[string]string, error) {
	project, err := uc.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %w", err)
	}
	if err := mustOwnProject(project, userID); err != nil {
		return nil, err
	}
	return uc.EnvVarRepo.ListForProject(ctx, projectID)
}

// SetProjectEnvUseCase replaces a project's entire env var set. It does
// NOT redeploy — the new values take effect on the next /run, same as any
// other pending change. Whoever calls this needs to know that.
type SetProjectEnvUseCase struct {
	ProjectRepo repository.ProjectRepository
	EnvVarRepo  repository.EnvVarRepository
}

func NewSetProjectEnvUseCase(projectRepo repository.ProjectRepository, envVarRepo repository.EnvVarRepository) *SetProjectEnvUseCase {
	return &SetProjectEnvUseCase{ProjectRepo: projectRepo, EnvVarRepo: envVarRepo}
}

func (uc *SetProjectEnvUseCase) Execute(ctx context.Context, projectID, userID string, vars map[string]string) (map[string]string, error) {
	project, err := uc.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %w", err)
	}
	if err := mustOwnProject(project, userID); err != nil {
		return nil, err
	}
	if err := validateEnvVars(vars); err != nil {
		return nil, fmt.Errorf("invalid env: %w", err)
	}

	if err := uc.EnvVarRepo.ReplaceForProject(ctx, projectID, vars); err != nil {
		return nil, err
	}
	return vars, nil
}
