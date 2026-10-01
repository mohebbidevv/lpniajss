package application

import (
	"context"

	"golaunch/internal/domain/repository"
)

type LogoutUserUseCase struct {
	SessionRepo repository.SessionRepository
}

func NewLogoutUserUseCase(sessionRepo repository.SessionRepository) *LogoutUserUseCase {
	return &LogoutUserUseCase{SessionRepo: sessionRepo}
}

func (uc *LogoutUserUseCase) Execute(ctx context.Context, rawToken string) error {
	return uc.SessionRepo.Delete(ctx, HashToken(rawToken))
}
