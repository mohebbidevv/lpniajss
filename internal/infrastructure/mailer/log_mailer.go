// Package mailer implements repository.Mailer.
package mailer

import (
	"context"
	"fmt"
	"log/slog"
)

// LogMailer prints the link instead of sending it, so password reset and
// email verification are fully exercisable in development without signing up
// to a delivery provider. It is not a production implementation: anyone
// reading the process logs can complete either flow.
type LogMailer struct {
	// BaseURL is the frontend origin the links point at.
	BaseURL string
}

func NewLogMailer(baseURL string) *LogMailer {
	if baseURL == "" {
		baseURL = "http://localhost:3000"
	}
	return &LogMailer{BaseURL: baseURL}
}

func (m *LogMailer) SendPasswordReset(ctx context.Context, to, rawToken string) error {
	slog.Warn("LogMailer: password reset link (development only, not emailed)",
		"to", to, "url", fmt.Sprintf("%s/reset-password?token=%s", m.BaseURL, rawToken))
	return nil
}

func (m *LogMailer) SendEmailVerification(ctx context.Context, to, rawToken string) error {
	slog.Warn("LogMailer: email verification link (development only, not emailed)",
		"to", to, "url", fmt.Sprintf("%s/verify-email?token=%s", m.BaseURL, rawToken))
	return nil
}
