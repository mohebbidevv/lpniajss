package repository

import "context"

// Mailer delivers the two transactional emails this platform sends. It is a
// port so the use cases never depend on a provider SDK, and so local
// development can run against a logging implementation with no signup.
type Mailer interface {
	SendPasswordReset(ctx context.Context, to, rawToken string) error
	SendEmailVerification(ctx context.Context, to, rawToken string) error
}
