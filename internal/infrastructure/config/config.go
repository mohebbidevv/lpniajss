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
	Server       ServerConfig
	DB           DBConfig
	Caddy        CaddyConfig
	Runtime      RuntimeConfig
	Housekeeping HousekeepingConfig
}

// HousekeepingConfig governs the anonymous-submission reaper — projects
// staged via /upload or /import/github before signup that never got
// claimed, cleaned up on a timer so an abandoned submission doesn't sit on
// disk/in Postgres forever.
type HousekeepingConfig struct {
	AnonymousProjectTTLMinutes   int `json:"anonymous_project_ttl_minutes"`
	AnonymousReapIntervalMinutes int `json:"anonymous_reap_interval_minutes"`
}

type ServerConfig struct {
	Port string `json:"port"`

	// BindAddress is the interface the control plane listens on. It
	// defaults to loopback because Caddy runs on the same host and is the
	// only thing that should reach this process: exposed directly, a client
	// could set its own X-Forwarded-For and defeat every IP-keyed limiter
	// here. Set "0.0.0.0" only if the proxy is genuinely on another host.
	BindAddress string `json:"bind_address"`

	// AllowedOrigins is the CORS allowlist. Cookie-based auth cannot use a
	// wildcard origin — browsers refuse to send credentials to "*" — so
	// every frontend origin that will call this API needs to be listed here.
	AllowedOrigins []string `json:"allowed_origins"`

	// SecureCookies gates the session cookie's Secure flag. Must be false
	// for local http:// development (a Secure cookie is never sent over
	// plain HTTP) and MUST be true once this is served over HTTPS.
	SecureCookies bool `json:"secure_cookies"`

	// AnonymousSubmitLimit/Window rate-limit /upload and /import/github by
	// client IP — both endpoints accept unauthenticated requests (staged
	// pending signup), so this is the only thing standing between an
	// anonymous caller and unlimited disk-filling submissions.
	AnonymousSubmitLimit         int `json:"anonymous_submit_limit"`
	AnonymousSubmitWindowSeconds int `json:"anonymous_submit_window_seconds"`

	// LoginIPLimit / LoginAccountLimit bound failed sign-in attempts per
	// window. Login is unthrottled without these, and bcrypt at cost 12
	// makes it the cheapest way to exhaust CPU on this process.
	LoginIPLimit       int `json:"login_ip_limit"`
	LoginAccountLimit  int `json:"login_account_limit"`
	LoginWindowSeconds int `json:"login_window_seconds"`

	// InternalToken guards /internal/*, which exposes queue depth and
	// failure counts. Empty means those endpoints 404 for everyone.
	InternalToken string `json:"internal_token"`

	// FrontendBaseURL is where password-reset and verification links point.
	// It is the frontend's origin, not this API's — the user lands on a
	// page that then posts the token back here.
	FrontendBaseURL string `json:"frontend_base_url"`

	// EnvEncryptionKey is a base64-encoded 32-byte AES key protecting
	// project env vars at rest. Empty stores them as plaintext, which is
	// the pre-encryption behaviour and is warned about at boot. Generate
	// one with: openssl rand -base64 32
	EnvEncryptionKey string `json:"env_encryption_key"`

	// LogLevel is debug|info|warn|error; LogFormat is json|text. JSON is
	// queryable and belongs in production; text is readable and belongs in
	// a terminal.
	LogLevel  string `json:"log_level"`
	LogFormat string `json:"log_format"`
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

	// ImageRetentionCount is how many distinct images to keep per project —
	// directly, how many versions deep a rollback can go. Every retained
	// image costs real disk.
	ImageRetentionCount int `json:"image_retention_count"`

	// ImageSweepIntervalMinutes governs the periodic GC pass that catches
	// images orphaned by paths which never reached a successful deploy.
	ImageSweepIntervalMinutes int `json:"image_sweep_interval_minutes"`

	// DataRoot is the filesystem images are written to, and MinBuildDiskGB
	// the headroom a build requires before it will start. Together they turn
	// a full disk into one clear message instead of a cryptic build failure.
	DataRoot       string `json:"data_root"`
	MinBuildDiskGB int    `json:"min_build_disk_gb"`
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

	cfg.applyEnvOverrides()
	cfg.applyDefaults()

	if err := cfg.Runtime.validate(); err != nil {
		return AppConfig{}, err
	}

	return cfg, nil
}

// applyEnvOverrides lets the environment win over the file. The file
// carries structure and defaults; secrets come from the process
// environment, which is not committable, is not exposed by a stray cat of
// the repo, and is the format systemd EnvironmentFile, Docker secrets and
// Kubernetes all already speak.
func (c *AppConfig) applyEnvOverrides() {
	if v := os.Getenv("GOLAUNCH_DB_PASSWORD"); v != "" {
		c.DB.Password = v
	}
	if v := os.Getenv("GOLAUNCH_DB_HOST"); v != "" {
		c.DB.Host = v
	}
	if v := os.Getenv("GOLAUNCH_INTERNAL_TOKEN"); v != "" {
		c.Server.InternalToken = v
	}
	if v := os.Getenv("GOLAUNCH_ENV_ENCRYPTION_KEY"); v != "" {
		c.Server.EnvEncryptionKey = v
	}
}

func (c *AppConfig) applyDefaults() {
	if c.Server.Port == "" {
		c.Server.Port = "8080"
	}
	if c.Server.BindAddress == "" {
		c.Server.BindAddress = "127.0.0.1"
	}
	if len(c.Server.AllowedOrigins) == 0 {
		c.Server.AllowedOrigins = []string{"http://localhost:3000"}
	}
	if c.Caddy.AdminURL == "" {
		c.Caddy.AdminURL = "http://localhost:2019"
	}
	if c.Caddy.Domain == "" {
		c.Caddy.Domain = "localhost"
	}
	if c.Server.AnonymousSubmitLimit == 0 {
		c.Server.AnonymousSubmitLimit = 5
	}
	if c.Server.AnonymousSubmitWindowSeconds == 0 {
		c.Server.AnonymousSubmitWindowSeconds = 600
	}
	if c.Server.FrontendBaseURL == "" && len(c.Server.AllowedOrigins) > 0 {
		c.Server.FrontendBaseURL = c.Server.AllowedOrigins[0]
	}
	if c.Server.LoginIPLimit == 0 {
		c.Server.LoginIPLimit = 20
	}
	if c.Server.LoginAccountLimit == 0 {
		c.Server.LoginAccountLimit = 5
	}
	if c.Server.LoginWindowSeconds == 0 {
		c.Server.LoginWindowSeconds = 900
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = "info"
	}
	if c.Server.LogFormat == "" {
		c.Server.LogFormat = "text"
	}
	if c.Housekeeping.AnonymousProjectTTLMinutes == 0 {
		c.Housekeeping.AnonymousProjectTTLMinutes = 60
	}
	if c.Housekeeping.AnonymousReapIntervalMinutes == 0 {
		c.Housekeeping.AnonymousReapIntervalMinutes = 15
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
	if r.ImageRetentionCount == 0 {
		r.ImageRetentionCount = 5
	}
	if r.ImageSweepIntervalMinutes == 0 {
		r.ImageSweepIntervalMinutes = 60
	}
	if r.DataRoot == "" {
		r.DataRoot = "/var/lib/docker"
	}
	if r.MinBuildDiskGB == 0 {
		r.MinBuildDiskGB = 5
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
