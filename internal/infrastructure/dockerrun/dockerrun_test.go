package dockerrun

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"

	"golaunch/internal/domain/entities"
)

func newTestRuntime() *DockerRuntime {
	return &DockerRuntime{cfg: Config{}.withDefaults()}
}

func TestTranslateAppliesHardening(t *testing.T) {
	r := newTestRuntime()

	cfg, hostCfg, netCfg := r.translate(entities.RuntimeSpec{
		DeploymentID: "dep-1",
		ImageRef:     "golaunch/app:dep-1",
		Labels:       map[string]string{"slug": "demo"},
	})

	if !hostCfg.ReadonlyRootfs {
		t.Error("rootfs must be read-only")
	}
	if len(hostCfg.CapDrop) != 1 || hostCfg.CapDrop[0] != "ALL" {
		t.Errorf("all capabilities must be dropped, got %v", hostCfg.CapDrop)
	}
	if len(hostCfg.CapAdd) != 0 {
		t.Errorf("no capability may be added back, got %v", hostCfg.CapAdd)
	}
	if hostCfg.Privileged {
		t.Error("container must not be privileged")
	}
	if len(hostCfg.SecurityOpt) != 1 || hostCfg.SecurityOpt[0] != "no-new-privileges:true" {
		t.Errorf("no-new-privileges must be set, got %v", hostCfg.SecurityOpt)
	}
	if len(hostCfg.Binds) != 0 || len(hostCfg.Mounts) != 0 {
		t.Error("no host path may be bound into an app container")
	}
	if len(hostCfg.PortBindings) != 0 || hostCfg.PublishAllPorts {
		t.Error("app containers must not publish host ports")
	}
	if hostCfg.RestartPolicy.Name != container.RestartPolicyDisabled {
		t.Errorf("restart policy must be disabled, got %q", hostCfg.RestartPolicy.Name)
	}
	if string(hostCfg.NetworkMode) != DefaultNetwork {
		t.Errorf("network mode = %q, want %q", hostCfg.NetworkMode, DefaultNetwork)
	}
	if _, ok := netCfg.EndpointsConfig[DefaultNetwork]; !ok {
		t.Error("container must be attached to the edge network")
	}
	if cfg.Labels[labelManaged] != managedValue {
		t.Error("container must carry the managed label")
	}
}

func TestTranslateResourceLimits(t *testing.T) {
	r := newTestRuntime()

	_, hostCfg, _ := r.translate(entities.RuntimeSpec{
		DeploymentID: "dep-2",
		Limits:       entities.ResourceLimits{MemoryMB: 256, CPUCores: 0.5, PidsLimit: 64},
	})

	const wantMemory = 256 * 1024 * 1024
	if hostCfg.Memory != wantMemory {
		t.Errorf("memory = %d, want %d", hostCfg.Memory, wantMemory)
	}
	if hostCfg.MemorySwap != hostCfg.Memory {
		t.Errorf("swap must equal memory to disable it, got %d", hostCfg.MemorySwap)
	}
	if hostCfg.NanoCPUs != 500_000_000 {
		t.Errorf("nanocpus = %d, want 500000000", hostCfg.NanoCPUs)
	}
	if hostCfg.PidsLimit == nil || *hostCfg.PidsLimit != 64 {
		t.Errorf("pids limit = %v, want 64", hostCfg.PidsLimit)
	}
}

func TestTranslateFallsBackToDefaultLimits(t *testing.T) {
	r := newTestRuntime()

	_, hostCfg, _ := r.translate(entities.RuntimeSpec{DeploymentID: "dep-3"})

	if hostCfg.Memory == 0 || hostCfg.NanoCPUs == 0 || hostCfg.PidsLimit == nil {
		t.Error("a spec with no limits must still land inside a cgroup")
	}
}

func TestContainerAddress(t *testing.T) {
	if got := ContainerAddress("dep-4", 3000); got != "golaunch-dep-4:3000" {
		t.Errorf("address = %q", got)
	}
	if got := ContainerAddress("dep-4", 0); got != "golaunch-dep-4:3000" {
		t.Errorf("address with zero port = %q", got)
	}
}

func TestLabelRoundTrip(t *testing.T) {
	in := map[string]string{"project_id": "p1", "deployment_id": "d1"}

	encoded := encodeLabels(in)
	if encoded["golaunch.project_id"] != "p1" {
		t.Errorf("labels must be namespaced, got %v", encoded)
	}

	decoded := decodeLabels(encoded)
	if len(decoded) != len(in) {
		t.Fatalf("round trip = %v, want %v", decoded, in)
	}
	for k, v := range in {
		if decoded[k] != v {
			t.Errorf("%s = %q, want %q", k, decoded[k], v)
		}
	}
}

func TestDecodeLabelsIgnoresForeignKeys(t *testing.T) {
	decoded := decodeLabels(map[string]string{
		"golaunch.slug":         "demo",
		"name":                  "golaunch-dep-1",
		"exitCode":              "1",
		"com.example.unrelated": "x",
	})

	if len(decoded) != 1 || decoded["slug"] != "demo" {
		t.Errorf("decoded = %v, want only the slug label", decoded)
	}
}

func TestLabelFiltersAlwaysScopeToManaged(t *testing.T) {
	args := labelFilters(nil)
	if !args.ExactMatch("label", labelManaged+"="+managedValue) {
		t.Fatal("a nil filter must still be scoped to managed containers")
	}

	args = labelFilters(map[string]string{"project_id": "p1"})
	if !args.ExactMatch("label", "golaunch.project_id=p1") {
		t.Error("application filters must be namespaced")
	}
	if !args.ExactMatch("label", labelManaged+"="+managedValue) {
		t.Error("managed scope must survive an explicit filter")
	}
}

func TestStatusFromState(t *testing.T) {
	code := 137

	tests := []struct {
		name  string
		state *container.State
		want  entities.RuntimeState
	}{
		{"running", &container.State{Running: true}, entities.RuntimeStateRunning},
		{"exited", &container.State{ExitCode: 1}, entities.RuntimeStateExited},
		{"oom outranks exit", &container.State{OOMKilled: true, ExitCode: code}, entities.RuntimeStateOOMKilled},
		{"missing", nil, entities.RuntimeStateNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusFromState(tc.state).State; got != tc.want {
				t.Errorf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTranslateEvent(t *testing.T) {
	evt, ok := translateEvent(events.Message{
		Action: events.ActionOOM,
		Actor: events.Actor{
			ID:         "abc123",
			Attributes: map[string]string{"golaunch.deployment_id": "d1", "name": "golaunch-d1"},
		},
	})
	if !ok {
		t.Fatal("oom must translate")
	}
	if evt.Type != entities.RuntimeEventOOM {
		t.Errorf("type = %q", evt.Type)
	}
	if evt.Handle != "abc123" {
		t.Errorf("handle = %q", evt.Handle)
	}
	if evt.Labels["deployment_id"] != "d1" {
		t.Errorf("labels = %v", evt.Labels)
	}

	if _, ok := translateEvent(events.Message{Action: events.ActionPause}); ok {
		t.Error("unsubscribed actions must be dropped")
	}
}

func TestEventBookmarkIsSecondsDotNanos(t *testing.T) {
	// 1_700_000_000.123456789s → the daemon splits on the dot and reads the
	// left half as seconds, so a bare nanosecond count would be misread.
	got := eventBookmark(1_700_000_000_123_456_789)
	if got != "1700000000.123456790" {
		t.Errorf("bookmark = %q, want 1700000000.123456790", got)
	}

	if got := eventBookmark(1_999_999_999); got != "2.000000000" {
		t.Errorf("nanosecond carry = %q, want 2.000000000", got)
	}
}

func TestTranslateInjectsPortWithoutAliasingCallerEnv(t *testing.T) {
	r := newTestRuntime()
	callerEnv := []string{"NODE_ENV=production"}

	cfg, _, _ := r.translate(entities.RuntimeSpec{DeploymentID: "dep-5", Env: callerEnv})

	if len(callerEnv) != 1 {
		t.Errorf("caller env was mutated: %v", callerEnv)
	}
	var found bool
	for _, e := range cfg.Env {
		if e == "PORT=3000" {
			found = true
		}
	}
	if !found {
		t.Errorf("env = %v, want a PORT entry", cfg.Env)
	}
}

func TestTranslateDoesNotLeakHostEnvironment(t *testing.T) {
	r := newTestRuntime()

	cfg, _, _ := r.translate(entities.RuntimeSpec{DeploymentID: "dep-6"})

	// only what the caller passed plus the runtime's own PORT
	if len(cfg.Env) != 1 || cfg.Env[0] != "PORT=3000" {
		t.Errorf("env = %v, want exactly [PORT=3000]", cfg.Env)
	}
}

func TestTranslateSetsNoNprocUlimit(t *testing.T) {
	r := newTestRuntime()

	_, hostCfg, _ := r.translate(entities.RuntimeSpec{
		DeploymentID: "dep-nproc",
		Limits:       entities.ResourceLimits{PidsLimit: 64},
	})

	// RLIMIT_NPROC is enforced per-UID host-wide, not per-container. Every
	// container here runs as UID 1000, which on a normal single-user Linux
	// box is also the host user's own login UID — so an nproc ulimit counts
	// their desktop's processes and the container can't exec even its first
	// process ("resource temporarily unavailable" on /sbin/docker-init).
	// PidsLimit below is the correctly-scoped control; do not add nproc back.
	for _, u := range hostCfg.Ulimits {
		if u.Name == "nproc" {
			t.Fatalf("nproc ulimit must not be set — it breaks container startup entirely, got %+v", u)
		}
	}

	if hostCfg.PidsLimit == nil || *hostCfg.PidsLimit != 64 {
		t.Errorf("the pids cgroup is what actually caps process count, got %v", hostCfg.PidsLimit)
	}

	var hasNofile bool
	for _, u := range hostCfg.Ulimits {
		if u.Name == "nofile" {
			hasNofile = true
		}
	}
	if !hasNofile {
		t.Error("nofile is per-process and correct — it should still be set")
	}
}
