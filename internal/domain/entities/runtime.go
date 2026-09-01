package entities

import "context"

// ResourceLimits is runtime-agnostic — the docker implementation translates
// this into Docker's HostConfig; a future implementation could translate it
// into whatever else.
type ResourceLimits struct {
	MemoryMB   int64
	CPUCores   float64 // e.g. 0.5 = half a core
	PidsLimit  int64
	ReadOnlyFS bool
}

// RuntimeSpec is everything needed to start a project's process/container.
type RuntimeSpec struct {
	DeploymentID string
	ProjectID    string
	Slug         string
	ImageRef     string
	Port         int
	// Env          map[string]string
	Env    []string
	Limits ResourceLimits
	Labels map[string]string
}

type RuntimeState string

const (
	RuntimeStateRunning   RuntimeState = "running"
	RuntimeStateExited    RuntimeState = "exited"
	RuntimeStateOOMKilled RuntimeState = "oom_killed"
	RuntimeStateNotFound  RuntimeState = "not_found"
)

// RuntimeHandle is an opaque reference the runtime implementation returns
// from Start and expects back for Stop/Remove/Status/Logs. Application code
// should treat this as opaque — don't parse it, just store and pass it back.
type RuntimeHandle string

type RuntimeStatus struct {
	State    RuntimeState
	ExitCode *int
}

// RuntimeInstance is what List returns — enough to reconcile DB state
// against actual running instances without a separate Status call per item.
type RuntimeInstance struct {
	Handle RuntimeHandle
	Labels map[string]string
	Status RuntimeStatus
}

type RuntimeEventType string

const (
	RuntimeEventStarted RuntimeEventType = "started"
	RuntimeEventDied    RuntimeEventType = "died"
	RuntimeEventOOM     RuntimeEventType = "oom"
)

type RuntimeEvent struct {
	Type   RuntimeEventType
	Handle RuntimeHandle
	Labels map[string]string
}

type LogStream string

const (
	LogStdout LogStream = "stdout"
	LogStderr LogStream = "stderr"
	LogInfo   LogStream = "info" // for pipeline-generated lines, not process output
)

type LogLine struct {
	Stream LogStream
	Text   string
}

type LogOptions struct {
	Follow bool
	Tail   int // 0 = all
}

// BuildRequest is what ImageBuilder.Build consumes.
type BuildRequest struct {
	ProjectID string
	SourceDir string
	ImageTag  string
	Timeout   int // seconds
	Limits    ResourceLimits
}

var _ = context.Context(nil) // placeholder import anchor if this file ends up needing ctx-typed fields later
