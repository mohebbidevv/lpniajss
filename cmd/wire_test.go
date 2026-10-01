package main

import (
	"testing"
	"time"

	"golaunch/internal/application"
)

func TestJobTimeoutCoversTheFullDeployBudget(t *testing.T) {
	dc := application.DeployConfig{
		BuildTimeout: 15 * time.Minute,
		ReadyTimeout: 90 * time.Second,
	}

	got := jobTimeoutFor(dc)

	// the whole point: a child context can never outlive its parent, so
	// the worker pool's ceiling must be strictly greater than the sum of
	// every stage timeout the pipeline can legitimately spend — otherwise
	// BuildTimeout is configurable but unreachable, which is the exact bug
	// this exists to prevent.
	minimum := dc.BuildTimeout + dc.ReadyTimeout
	if got <= minimum {
		t.Fatalf("job timeout %s does not exceed build+ready budget %s — BuildTimeout would be unreachable", got, minimum)
	}
}

func TestJobTimeoutScalesWithConfig(t *testing.T) {
	small := jobTimeoutFor(application.DeployConfig{BuildTimeout: time.Minute, ReadyTimeout: time.Second})
	large := jobTimeoutFor(application.DeployConfig{BuildTimeout: time.Hour, ReadyTimeout: time.Minute})

	if large <= small {
		t.Fatalf("a larger configured budget must produce a larger job timeout, got small=%s large=%s", small, large)
	}
}
