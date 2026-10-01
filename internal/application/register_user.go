package application

import (
	"context"
	"fmt"
	"strings"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
)

type RegisterUserUseCase struct {
	UserRepo repository.UserRepository
}

func NewRegisterUserUseCase(userRepo repository.UserRepository) *RegisterUserUseCase {
	return &RegisterUserUseCase{UserRepo: userRepo}
}

func (uc *RegisterUserUseCase) Execute(ctx context.Context, email, password string) (*entities.User, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return nil, fmt.Errorf("email is required")
	}
	if len(password) < 8 {
		return nil, fmt.Errorf("password must be at least 8 characters")
	}

	exists, err := uc.UserRepo.EmailExists(ctx, email)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("a user with this email already exists")
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}

	user := &entities.User{Email: email, PasswordHash: hash}
	id, err := uc.UserRepo.Create(ctx, user)
	if err != nil {
		return nil, err
	}
	user.ID = id

	return user, nil
}

// NormalizeEmail is exported because the login rate limiter must key on
// exactly the same string the user lookup does — otherwise bob@x.com and
// BOB@x.com get separate attempt budgets.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
