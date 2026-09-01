package hostexec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type ProjectSpecs struct {
	IsNext     bool
	InstallCmd []string
	BuildCmd   []string
	StartCmd   []string
}

func GetProjectSpecs(path string, port int) *ProjectSpecs {
	pkg := readPackageJSON(path)

	isNext := fileExists(filepath.Join(path, "next.config.js")) ||
		fileExists(filepath.Join(path, "next.config.ts")) ||
		fileExists(filepath.Join(path, "next.config.mjs")) ||
		hasDependency(path, "next")

	var startCmd []string
	var buildCmd []string

	if isNext {
		startCmd = []string{"npx", "next", "start", "-p", itoa(port)}
		buildCmd = []string{"npx", "next", "build"}
	} else {
		switch {
		case pkg.Scripts["start"] != "":
			startCmd = []string{"npm", "start"}
		case pkg.Scripts["dev"] != "":
			startCmd = []string{"npm", "run", "dev"}
		case pkg.Main != "":
			startCmd = []string{"node", pkg.Main}
		default:
			startCmd = []string{"node", "index.js"}
		}
	}

	return &ProjectSpecs{
		IsNext:     isNext,
		InstallCmd: []string{"npm", "i", "--no-audit", "--no-fund"},
		BuildCmd:   buildCmd,
		StartCmd:   startCmd,
	}
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// ── helpers ──────────────────────────────────────────────────────────────

type packageJSON struct {
	Scripts      map[string]string `json:"scripts"`
	Main         string            `json:"main"`
	Dependencies map[string]string `json:"dependencies"`
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readPackageJSON(path string) packageJSON {
	var pkg packageJSON
	fullPath := filepath.Join(path, "package.json")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return pkg
	}
	json.Unmarshal(data, &pkg)
	return pkg
}

func hasDependency(path, dep string) bool {
	pkg := readPackageJSON(path)
	_, ok := pkg.Dependencies[dep]
	return ok
}