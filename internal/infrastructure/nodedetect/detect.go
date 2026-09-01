// Package nodedetect inspects a Node project's source tree and works out how
// to install, build and start it. It is deliberately runtime-agnostic: the
// host-exec runtime runs these commands directly, and the Docker builder
// bakes the same commands into a generated Dockerfile. Keeping one detector
// means both runtimes agree on what a given project is.
package nodedetect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// PackageManager is chosen by which lockfile the project ships.
type PackageManager string

const (
	PackageManagerNPM  PackageManager = "npm"
	PackageManagerYarn PackageManager = "yarn"
	PackageManagerPNPM PackageManager = "pnpm"
)

// DefaultNodeMajor is the Node major version used when a project doesn't pin
// one via engines.node or .nvmrc.
const DefaultNodeMajor = "22"

// ProjectSpecs is everything the two runtimes need to know about a project.
type ProjectSpecs struct {
	IsNext bool

	// PackageManager and Lockfile describe how dependencies are pinned.
	// Lockfile is the bare filename ("package-lock.json") or "" if the
	// project ships none — the Docker builder needs the name to COPY it
	// into the dependency layer, and its absence decides whether a
	// reproducible `ci`-style install is possible at all.
	PackageManager PackageManager
	Lockfile       string

	// NodeMajor is the major version only ("22"), suitable for building a
	// base image tag. Always non-empty; falls back to DefaultNodeMajor.
	NodeMajor string

	InstallCmd []string
	BuildCmd   []string
	StartCmd   []string
}

// GetProjectSpecs inspects the project at path. port is baked into the start
// command for frameworks that take it as a flag rather than reading $PORT.
func GetProjectSpecs(path string, port int) *ProjectSpecs {
	pkg := readPackageJSON(path)

	isNext := fileExists(filepath.Join(path, "next.config.js")) ||
		fileExists(filepath.Join(path, "next.config.ts")) ||
		fileExists(filepath.Join(path, "next.config.mjs")) ||
		hasDependency(pkg, "next")

	pm, lockfile := detectPackageManager(path)

	return &ProjectSpecs{
		IsNext:         isNext,
		PackageManager: pm,
		Lockfile:       lockfile,
		NodeMajor:      detectNodeMajor(path, pkg),
		InstallCmd:     installCmd(pm, lockfile != ""),
		BuildCmd:       buildCmd(pm, pkg, isNext),
		StartCmd:       startCmd(pm, pkg, isNext, port),
	}
}

// ── package manager ──────────────────────────────────────────────────────

// detectPackageManager picks a manager from the lockfile on disk. Order
// matters only for projects that (wrongly) ship more than one lockfile;
// pnpm and yarn are checked first because a project with both pnpm-lock.yaml
// and package-lock.json is almost always a pnpm project with a stale npm
// lockfile left behind.
func detectPackageManager(path string) (PackageManager, string) {
	switch {
	case fileExists(filepath.Join(path, "pnpm-lock.yaml")):
		return PackageManagerPNPM, "pnpm-lock.yaml"
	case fileExists(filepath.Join(path, "yarn.lock")):
		return PackageManagerYarn, "yarn.lock"
	case fileExists(filepath.Join(path, "package-lock.json")):
		return PackageManagerNPM, "package-lock.json"
	default:
		return PackageManagerNPM, ""
	}
}

// installCmd returns a reproducible install when the project ships a
// lockfile, and a plain resolving install when it doesn't. `npm ci` and the
// --frozen-lockfile variants deliberately fail when the lockfile disagrees
// with package.json: for a deploy platform a loud failure beats silently
// installing versions the developer never tested against.
func installCmd(pm PackageManager, hasLockfile bool) []string {
	switch pm {
	case PackageManagerPNPM:
		if hasLockfile {
			return []string{"pnpm", "install", "--frozen-lockfile"}
		}
		return []string{"pnpm", "install"}
	case PackageManagerYarn:
		if hasLockfile {
			return []string{"yarn", "install", "--frozen-lockfile"}
		}
		return []string{"yarn", "install"}
	default:
		if hasLockfile {
			return []string{"npm", "ci", "--no-audit", "--no-fund"}
		}
		return []string{"npm", "install", "--no-audit", "--no-fund"}
	}
}

// ── build ────────────────────────────────────────────────────────────────

// buildCmd prefers the project's own build script — that's what the author
// tested and it covers every framework, not just Next. The bare `next build`
// fallback only applies to a Next project that somehow has no build script.
func buildCmd(pm PackageManager, pkg packageJSON, isNext bool) []string {
	if pkg.Scripts["build"] != "" {
		return runScript(pm, "build")
	}
	if isNext {
		return []string{"npx", "next", "build"}
	}
	return nil
}

// ── start ────────────────────────────────────────────────────────────────

// startCmd resolves how to boot the app. Next gets an explicit -p because
// `next start` does not read $PORT; everything else is expected to honour
// the PORT environment variable the runtime injects.
func startCmd(pm PackageManager, pkg packageJSON, isNext bool, port int) []string {
	if isNext {
		return []string{"npx", "next", "start", "-p", fmt.Sprintf("%d", port)}
	}

	switch {
	case pkg.Scripts["start"] != "":
		return runStart(pm)
	case pkg.Scripts["dev"] != "":
		return runScript(pm, "dev")
	case pkg.Main != "":
		return []string{"node", pkg.Main}
	default:
		return []string{"node", "index.js"}
	}
}

// runScript builds the "run a named script" invocation. npm needs the
// explicit `run`; yarn and pnpm accept the script name directly but `run`
// is valid for all three, so it's used uniformly for anything that isn't a
// lifecycle script.
func runScript(pm PackageManager, script string) []string {
	return []string{string(pm), "run", script}
}

// runStart uses the lifecycle form (`npm start`, not `npm run start`) since
// that's what all three managers document for the start script.
func runStart(pm PackageManager) []string {
	return []string{string(pm), "start"}
}

// ── node version ─────────────────────────────────────────────────────────

var majorVersionPattern = regexp.MustCompile(`(\d+)`)

// detectNodeMajor reads a pinned Node version from engines.node or .nvmrc
// and reduces it to a major version. Range syntax (">=18", "^20 || ^22",
// "20.x") is handled by taking the first number that appears, which for
// every range form in practice is the lowest supported major — the safe end
// of the range to build against.
func detectNodeMajor(path string, pkg packageJSON) string {
	if major := firstMajor(pkg.Engines["node"]); major != "" {
		return major
	}

	if data, err := os.ReadFile(filepath.Join(path, ".nvmrc")); err == nil {
		if major := firstMajor(string(data)); major != "" {
			return major
		}
	}

	return DefaultNodeMajor
}

func firstMajor(constraint string) string {
	match := majorVersionPattern.FindStringSubmatch(strings.TrimSpace(constraint))
	if match == nil {
		return ""
	}
	return match[1]
}

// ── helpers ──────────────────────────────────────────────────────────────

type packageJSON struct {
	Scripts      map[string]string `json:"scripts"`
	Main         string            `json:"main"`
	Dependencies map[string]string `json:"dependencies"`
	Engines      map[string]string `json:"engines"`
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readPackageJSON(path string) packageJSON {
	var pkg packageJSON
	data, err := os.ReadFile(filepath.Join(path, "package.json"))
	if err != nil {
		return pkg
	}
	json.Unmarshal(data, &pkg)
	return pkg
}

func hasDependency(pkg packageJSON, dep string) bool {
	_, ok := pkg.Dependencies[dep]
	return ok
}
