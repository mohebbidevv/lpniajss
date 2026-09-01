package application

import (
	"context"
	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/utils"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

type UploadProjectUseCase struct {
	ProjectRepo repository.ProjectRepository
	Storage     repository.Storage
	UploadDir   string
	WorkDir     string
}

type UploadInput struct {
	// UserID   domain.UserID
	File     io.Reader
	Filename string
}

type UploadOutput struct {
	ProjectID string
	UniqueKey string
}

func NewUploadProjectUseCase(
	projectRepo repository.ProjectRepository,
	storageRepo repository.Storage,
	uploadDir string, // Added
	workDir string, // Added
) *UploadProjectUseCase {

	return &UploadProjectUseCase{
		ProjectRepo: projectRepo,
		Storage:     storageRepo,
		UploadDir:   uploadDir, // Assign
		WorkDir:     workDir,   // Assign
	}
}

func (uc *UploadProjectUseCase) Execute(
	ctx context.Context, input UploadInput) (*UploadOutput, error) {

	storageID := utils.NewID()
	zipPath := filepath.Join(uc.UploadDir, storageID+".zip")
	extractPath := filepath.Join(uc.WorkDir, storageID)

	if err := uc.Storage.Save(zipPath, input.File); err != nil {
		return nil, err
	}

	if err := uc.Storage.Unzip(zipPath, extractPath); err != nil {
		return nil, err
	}

	project := entities.NewProject(
		input.Filename,
		slugify(input.Filename),
		storageID,
		"zip",
		extractPath,
	)

	projID, err := uc.ProjectRepo.Create(ctx, project)

	if err != nil {
		return nil, err
	}

	return &UploadOutput{
		ProjectID: projID,
		UniqueKey: project.UniqueKey,
	}, nil
}

func slugify(filename string) string {
	s := strings.TrimSuffix(filename, filepath.Ext(filename))
	s = strings.ToLower(s)
	s = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "app"
	}
	return s
}