package application

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/utils"
)

// githubRepoURLPattern only allows a plain https://github.com/<owner>/<repo>
// URL (optionally with a .git suffix). This is a security boundary, not just
// validation: it's what stops an arbitrary host from being handed to
// GitSource.Sync (no SSRF to internal networks via a "repo" URL) and stops a
// value starting with "-" from being parsed as a git flag.
var githubRepoURLPattern = regexp.MustCompile(`^https://github\.com/([\w.-]+)/([\w.-]+?)(\.git)?$`)

type ImportGithubInput struct {
	UserID  string
	RepoURL string
	Ref     string // "" = the remote's default branch
}

type ImportGithubOutput struct {
	ProjectID string
	UniqueKey string
}

type ImportGithubProjectUseCase struct {
	ProjectRepo repository.ProjectRepository
	GitSource   repository.GitSource
	WorkDir     string
}

func NewImportGithubProjectUseCase(
	projectRepo repository.ProjectRepository,
	gitSource repository.GitSource,
	workDir string,
) *ImportGithubProjectUseCase {
	return &ImportGithubProjectUseCase{
		ProjectRepo: projectRepo,
		GitSource:   gitSource,
		WorkDir:     workDir,
	}
}

func (uc *ImportGithubProjectUseCase) Execute(ctx context.Context, input ImportGithubInput) (*ImportGithubOutput, error) {
	match := githubRepoURLPattern.FindStringSubmatch(input.RepoURL)
	if match == nil {
		return nil, fmt.Errorf("repo url must look like https://github.com/<owner>/<repo>")
	}

	// only an already-logged-in submission has an owner to count against;
	// an anonymous one is checked later, at claim time
	if input.UserID != "" {
		if err := enforceProjectLimit(ctx, uc.ProjectRepo, input.UserID); err != nil {
			return nil, err
		}
	}

	owner, repo := match[1], match[2]
	name := fmt.Sprintf("%s/%s", owner, repo)

	slug, err := uniqueSlug(ctx, uc.ProjectRepo, slugify(name))
	if err != nil {
		return nil, err
	}

	storageID := utils.NewID()
	destDir := filepath.Join(uc.WorkDir, storageID)

	if err := uc.GitSource.Sync(ctx, input.RepoURL, input.Ref, destDir); err != nil {
		return nil, fmt.Errorf("clone failed: %w", err)
	}

	var ref *string
	if input.Ref != "" {
		ref = &input.Ref
	}

	project := entities.NewGitProject(input.UserID, name, slug, storageID, destDir, input.RepoURL, ref)

	projID, err := uc.ProjectRepo.Create(ctx, project)
	if err != nil {
		return nil, err
	}

	return &ImportGithubOutput{
		ProjectID: projID,
		UniqueKey: project.UniqueKey,
	}, nil
}
