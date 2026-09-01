package nodedetect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProject lays out a fake project on disk. files maps a relative path
// to its contents; empty contents are fine for marker files like lockfiles.
func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestPackageManagerAndInstallCommand(t *testing.T) {
	tests := []struct {
		name        string
		files       map[string]string
		wantPM      PackageManager
		wantLock    string
		wantInstall []string
	}{
		{
			name:        "npm with lockfile installs reproducibly",
			files:       map[string]string{"package.json": "{}", "package-lock.json": "{}"},
			wantPM:      PackageManagerNPM,
			wantLock:    "package-lock.json",
			wantInstall: []string{"npm", "ci", "--no-audit", "--no-fund"},
		},
		{
			name:        "no lockfile falls back to a resolving install",
			files:       map[string]string{"package.json": "{}"},
			wantPM:      PackageManagerNPM,
			wantLock:    "",
			wantInstall: []string{"npm", "install", "--no-audit", "--no-fund"},
		},
		{
			name:        "pnpm lockfile wins",
			files:       map[string]string{"package.json": "{}", "pnpm-lock.yaml": ""},
			wantPM:      PackageManagerPNPM,
			wantLock:    "pnpm-lock.yaml",
			wantInstall: []string{"pnpm", "install", "--frozen-lockfile"},
		},
		{
			name:        "yarn lockfile wins",
			files:       map[string]string{"package.json": "{}", "yarn.lock": ""},
			wantPM:      PackageManagerYarn,
			wantLock:    "yarn.lock",
			wantInstall: []string{"yarn", "install", "--frozen-lockfile"},
		},
		{
			// a stale package-lock.json left behind in a pnpm repo must not
			// hijack the install, or dependencies resolve differently than
			// the author intended
			name:        "pnpm takes precedence over a leftover npm lockfile",
			files:       map[string]string{"package.json": "{}", "pnpm-lock.yaml": "", "package-lock.json": "{}"},
			wantPM:      PackageManagerPNPM,
			wantLock:    "pnpm-lock.yaml",
			wantInstall: []string{"pnpm", "install", "--frozen-lockfile"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs := GetProjectSpecs(writeProject(t, tt.files), 3000)

			if specs.PackageManager != tt.wantPM {
				t.Errorf("PackageManager = %q, want %q", specs.PackageManager, tt.wantPM)
			}
			if specs.Lockfile != tt.wantLock {
				t.Errorf("Lockfile = %q, want %q", specs.Lockfile, tt.wantLock)
			}
			if got := strings.Join(specs.InstallCmd, " "); got != strings.Join(tt.wantInstall, " ") {
				t.Errorf("InstallCmd = %q, want %q", got, tt.wantInstall)
			}
		})
	}
}

func TestNextDetection(t *testing.T) {
	t.Run("via dependency", func(t *testing.T) {
		dir := writeProject(t, map[string]string{
			"package.json": `{"dependencies":{"next":"14.0.0"}}`,
		})
		specs := GetProjectSpecs(dir, 3000)

		if !specs.IsNext {
			t.Fatal("expected Next.js to be detected from the dependency list")
		}
		// next start ignores $PORT, so the port has to be baked in as a flag
		if got := strings.Join(specs.StartCmd, " "); got != "npx next start -p 3000" {
			t.Errorf("StartCmd = %q, want the port passed explicitly", got)
		}
	})

	t.Run("via config file", func(t *testing.T) {
		dir := writeProject(t, map[string]string{
			"package.json":    "{}",
			"next.config.mjs": "export default {}",
		})
		if !GetProjectSpecs(dir, 3000).IsNext {
			t.Fatal("expected Next.js to be detected from next.config.mjs")
		}
	})

	t.Run("plain node project is not Next", func(t *testing.T) {
		dir := writeProject(t, map[string]string{
			"package.json": `{"scripts":{"start":"node server.js"}}`,
		})
		specs := GetProjectSpecs(dir, 3000)

		if specs.IsNext {
			t.Fatal("plain node project must not be detected as Next.js")
		}
		if got := strings.Join(specs.StartCmd, " "); got != "npm start" {
			t.Errorf("StartCmd = %q, want %q", got, "npm start")
		}
	})
}

func TestStartCommandFallbacks(t *testing.T) {
	tests := []struct {
		name string
		pkg  string
		want string
	}{
		{"start script preferred", `{"scripts":{"start":"node a.js","dev":"node b.js"},"main":"c.js"}`, "npm start"},
		{"dev script when no start", `{"scripts":{"dev":"node b.js"},"main":"c.js"}`, "npm run dev"},
		{"main when no scripts", `{"main":"server.js"}`, "node server.js"},
		{"index.js as last resort", `{}`, "node index.js"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeProject(t, map[string]string{"package.json": tt.pkg})
			if got := strings.Join(GetProjectSpecs(dir, 3000).StartCmd, " "); got != tt.want {
				t.Errorf("StartCmd = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildCommandPrefersProjectScript(t *testing.T) {
	t.Run("uses the project's own build script", func(t *testing.T) {
		dir := writeProject(t, map[string]string{
			"package.json": `{"scripts":{"build":"tsc"},"dependencies":{"next":"14"}}`,
		})
		if got := strings.Join(GetProjectSpecs(dir, 3000).BuildCmd, " "); got != "npm run build" {
			t.Errorf("BuildCmd = %q, want the project's own build script", got)
		}
	})

	t.Run("no build step when there's nothing to build", func(t *testing.T) {
		dir := writeProject(t, map[string]string{"package.json": `{"scripts":{"start":"node a.js"}}`})
		if cmd := GetProjectSpecs(dir, 3000).BuildCmd; cmd != nil {
			t.Errorf("BuildCmd = %v, want nil", cmd)
		}
	})
}

func TestNodeVersionDetection(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"exact engines version", map[string]string{"package.json": `{"engines":{"node":"20.11.0"}}`}, "20"},
		{"range engines version", map[string]string{"package.json": `{"engines":{"node":">=18.17.0"}}`}, "18"},
		{"wildcard engines version", map[string]string{"package.json": `{"engines":{"node":"20.x"}}`}, "20"},
		{"nvmrc fallback", map[string]string{"package.json": "{}", ".nvmrc": "18.19.0\n"}, "18"},
		{"nvmrc with v prefix", map[string]string{"package.json": "{}", ".nvmrc": "v20\n"}, "20"},
		{"engines wins over nvmrc", map[string]string{"package.json": `{"engines":{"node":"22"}}`, ".nvmrc": "18"}, "22"},
		{"default when unpinned", map[string]string{"package.json": "{}"}, DefaultNodeMajor},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetProjectSpecs(writeProject(t, tt.files), 3000).NodeMajor; got != tt.want {
				t.Errorf("NodeMajor = %q, want %q", got, tt.want)
			}
		})
	}
}

// A directory with no package.json at all must still produce a usable spec
// rather than panicking — an uploaded zip can contain anything.
func TestMissingPackageJSONIsSurvivable(t *testing.T) {
	specs := GetProjectSpecs(t.TempDir(), 3000)

	if specs.NodeMajor != DefaultNodeMajor {
		t.Errorf("NodeMajor = %q, want the default", specs.NodeMajor)
	}
	if len(specs.StartCmd) == 0 {
		t.Error("StartCmd must never be empty")
	}
}
