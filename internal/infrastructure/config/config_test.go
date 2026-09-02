package config

import (
	"testing"
)

func TestApplyDefaults(t *testing.T) {
	t.Setenv(runtimeDriverEnv, "")

	cfg := AppConfig{}
	cfg.applyDefaults()

	if cfg.Server.Port != "8080" {
		t.Errorf("server port = %q", cfg.Server.Port)
	}
	if cfg.Caddy.AdminURL == "" || cfg.Caddy.Domain == "" {
		t.Errorf("caddy defaults missing: %+v", cfg.Caddy)
	}
	if cfg.Runtime.Driver != DriverDocker {
		t.Errorf("driver = %q, want docker", cfg.Runtime.Driver)
	}
	if cfg.Runtime.EndpointMode != "ip" {
		t.Errorf("endpoint mode = %q, want ip", cfg.Runtime.EndpointMode)
	}
	if cfg.Runtime.BuildMemoryMB <= cfg.Runtime.MemoryMB {
		t.Error("a build should get a larger memory budget than the app it produces")
	}
	if err := cfg.Runtime.validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestDriverEnvOverride(t *testing.T) {
	t.Setenv(runtimeDriverEnv, "HostExec")

	cfg := AppConfig{Runtime: RuntimeConfig{Driver: DriverDocker}}
	cfg.applyDefaults()

	if cfg.Runtime.Driver != DriverHostExec {
		t.Errorf("driver = %q, want hostexec", cfg.Runtime.Driver)
	}
}

func TestValidateRejectsUnknownValues(t *testing.T) {
	if err := (RuntimeConfig{Driver: "podman", EndpointMode: "ip"}).validate(); err == nil {
		t.Error("unknown driver must be rejected")
	}
	if err := (RuntimeConfig{Driver: DriverDocker, EndpointMode: "magic"}).validate(); err == nil {
		t.Error("unknown endpoint mode must be rejected")
	}
}
