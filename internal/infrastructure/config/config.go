package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golaunch/internal/infrastructure/utils"
)

// RuntimeDriver selects which repository.Runtime / repository.ImageBuilder
// implementation the control plane wires up at boot.
type RuntimeDriver string

const (
	DriverDocker   RuntimeDriver = "docker"
	DriverHostExec RuntimeDriver = "hostexec"
)

// runtimeDriverEnv overrides the configured driver, so a developer without a
// daemon can run the control plane without editing committed config.
const runtimeDriverEnv = "GOLAUNCH_RUNTIME"

type AppConfig struct {
	Server  ServerConfig
	DB      DBConfig
	Caddy   CaddyConfig
	Runtime RuntimeConfig
}

type ServerConfig struct {
	Port string `json:"port"`
}

type DBConfig struct {
	Host     string
	Port     int    `json:"port"`
	Name     string `json:"name"`
	User     string `json:"user"`
	Password string `json:"password"`
	SSLMode  string `json:"ssl_mode"` // disable/require
}

type CaddyConfig struct {
	AdminURL string `json:"admin_url"`
	Domain   string `json:"domain"`
}

// RuntimeConfig covers both what a deployed app is allowed to consume and
// what a build is allowed to consume. Builds get a bigger budget than apps:
// a webpack build legitimately needs far more memory than the server it
// produces.
type RuntimeConfig struct {
	Driver        RuntimeDriver `json:"driver"`
	Network       string        `json:"network"`
	ContainerPort int           `json:"container_port"`

	// EndpointMode is "ip" when Caddy runs on the host and "dns" when it
	// runs as a container on the same Docker network.
	EndpointMode string `json:"endpoint_mode"`

	MemoryMB  int64   `json:"memory_mb"`
	CPUCores  float64 `json:"cpu_cores"`
	PidsLimit int64   `json:"pids_limit"`

	BuildTimeoutSeconds int     `json:"build_timeout_seconds"`
	BuildMemoryMB       int64   `json:"build_memory_mb"`
	BuildCPUCores       float64 `json:"build_cpu_cores"`

	ReadyTimeoutSeconds int `json:"ready_timeout_seconds"`
}

func LoadConfig() (AppConfig, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return AppConfig{}, fmt.Errorf("failed to get current working directory: %w", err)
	}

	cfg, err := utils.OpenJSON[AppConfig](filepath.Join(cwd, "cmd/configuration.json"))
	if err != nil {
		return AppConfig{}, err
	}

	if cfg.DB.Host == "" || cfg.DB.User == "" || cfg.DB.Password == "" {
		return AppConfig{}, fmt.Errorf("db host, user and password are required")
	}

	cfg.applyDefaults()

	if err := cfg.Runtime.validate(); err != nil {
		return AppConfig{}, err
	}

	return cfg, nil
}

func (c *AppConfig) applyDefaults() {
	if c.Server.Port == "" {
		c.Server.Port = "8080"
	}
	if c.Caddy.AdminURL == "" {
		c.Caddy.AdminURL = "http://localhost:2019"
	}
	if c.Caddy.Domain == "" {
		c.Caddy.Domain = "localhost"
	}

	r := &c.Runtime
	if override := strings.TrimSpace(os.Getenv(runtimeDriverEnv)); override != "" {
		r.Driver = RuntimeDriver(strings.ToLower(override))
	}
	if r.Driver == "" {
		r.Driver = DriverDocker
	}
	if r.Network == "" {
		r.Network = "golaunch-edge"
	}
	if r.ContainerPort == 0 {
		r.ContainerPort = 3000
	}
	if r.EndpointMode == "" {
		r.EndpointMode = "ip"
	}
	if r.MemoryMB == 0 {
		r.MemoryMB = 512
	}
	if r.CPUCores == 0 {
		r.CPUCores = 1
	}
	if r.PidsLimit == 0 {
		r.PidsLimit = 256
	}
	if r.BuildTimeoutSeconds == 0 {
		r.BuildTimeoutSeconds = 900
	}
	if r.BuildMemoryMB == 0 {
		r.BuildMemoryMB = 2048
	}
	if r.BuildCPUCores == 0 {
		r.BuildCPUCores = 2
	}
	if r.ReadyTimeoutSeconds == 0 {
		r.ReadyTimeoutSeconds = 90
	}
}

func (r RuntimeConfig) validate() error {
	switch r.Driver {
	case DriverDocker, DriverHostExec:
	default:
		return fmt.Errorf("unknown runtime driver %q (want %q or %q)", r.Driver, DriverDocker, DriverHostExec)
	}

	switch r.EndpointMode {
	case "ip", "dns":
	default:
		return fmt.Errorf("unknown endpoint_mode %q (want \"ip\" or \"dns\")", r.EndpointMode)
	}

	return nil
}
