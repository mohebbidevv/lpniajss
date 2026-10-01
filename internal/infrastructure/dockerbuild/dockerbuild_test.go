package dockerbuild

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golaunch/internal/domain/entities"
	"golaunch/internal/infrastructure/nodedetect"
)

func writeFiles(t *testing.T, files map[string]string) string {
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

// ── Dockerfile generation ────────────────────────────────────────────────

func TestGenerateDockerfile(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"package.json":      `{"scripts":{"build":"next build"},"dependencies":{"next":"14"},"engines":{"node":"20"}}`,
		"package-lock.json": "{}",
	})
	df := GenerateDockerfile(nodedetect.GetProjectSpecs(dir, ContainerPort))

	mustContain := []string{
		"FROM node:20-slim AS deps",
		"FROM node:20-slim AS build",
		"FROM node:20-slim AS runtime",
		"COPY package.json ./",
		"COPY package-lock.json ./",
		"RUN npm ci --no-audit --no-fund",
		"COPY --from=deps /app/node_modules ./node_modules",
		"RUN npm run build",
		"USER node",
		"EXPOSE 3000",
		"ENV NODE_ENV=production",
	}
	for _, want := range mustContain {
		if !strings.Contains(df, want) {
			t.Errorf("generated Dockerfile missing %q\n---\n%s", want, df)
		}
	}

	// exec form matters: shell form would put /bin/sh between Docker and the
	// app, and sh doesn't forward SIGTERM, so graceful shutdown would break
	if !strings.Contains(df, `CMD ["npx","next","start","-p","3000"]`) {
		t.Errorf("CMD must be exec form\n---\n%s", df)
	}

	// the deps layer is only cacheable if the manifest is copied before the
	// source; if `COPY . .` came first every commit would bust the layer
	if depsCopy, srcCopy := strings.Index(df, "COPY package.json ./"), strings.Index(df, "COPY . ."); depsCopy > srcCopy {
		t.Error("manifest must be copied before the source or the dependency layer never caches")
	}
}

func TestGenerateDockerfileOmitsMissingLockfile(t *testing.T) {
	dir := writeFiles(t, map[string]string{"package.json": "{}"})
	df := GenerateDockerfile(nodedetect.GetProjectSpecs(dir, ContainerPort))

	// COPY of a path that doesn't exist is a hard build failure
	for _, lock := range []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml"} {
		if strings.Contains(df, "COPY "+lock) {
			t.Errorf("Dockerfile copies %s, which this project does not ship\n---\n%s", lock, df)
		}
	}
}

func TestGenerateDockerfileEnablesCorepackOnlyWhenNeeded(t *testing.T) {
	npmDir := writeFiles(t, map[string]string{"package.json": "{}", "package-lock.json": "{}"})
	if df := GenerateDockerfile(nodedetect.GetProjectSpecs(npmDir, ContainerPort)); strings.Contains(df, "corepack") {
		t.Error("npm needs no corepack shim; node images already ship npm")
	}

	pnpmDir := writeFiles(t, map[string]string{"package.json": "{}", "pnpm-lock.yaml": ""})
	df := GenerateDockerfile(nodedetect.GetProjectSpecs(pnpmDir, ContainerPort))
	if !strings.Contains(df, "RUN corepack enable pnpm") {
		t.Errorf("pnpm is not on PATH in node images without corepack\n---\n%s", df)
	}
	if !strings.Contains(df, "RUN pnpm install --frozen-lockfile") {
		t.Errorf("expected a pnpm install\n---\n%s", df)
	}
}

// ── ignore matching ──────────────────────────────────────────────────────

func TestIgnoreMatcher(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		".dockerignore": "*.log\nsecrets/\nbuild/*\n!build/keep.txt\n",
	})
	m, err := loadIgnoreMatcher(dir)
	if err != nil {
		t.Fatalf("loadIgnoreMatcher: %v", err)
	}

	tests := []struct {
		path string
		want bool
	}{
		// always-excluded regardless of .dockerignore
		{"node_modules", true},
		{"node_modules/react/index.js", true},
		{"packages/api/node_modules/x.js", true},
		{".git", true},
		{".git/config", true},
		// from .dockerignore
		{"debug.log", true},
		{"secrets", true},
		{"secrets/key.pem", true},
		{"build/output.js", true},
		// negation re-includes
		{"build/keep.txt", false},
		// ordinary source survives
		{"src/index.js", false},
		{"package.json", false},
		// `*` must not cross a path separator
		{"nested/debug.log", false},
	}

	for _, tt := range tests {
		if got := m.matches(tt.path); got != tt.want {
			t.Errorf("matches(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestIgnoreMatcherPruningIsSafeWithNegations(t *testing.T) {
	// pruning a matched directory is only safe when no negation could
	// re-include something nested inside it
	withNegation := writeFiles(t, map[string]string{".dockerignore": "build/\n!build/keep.txt\n"})
	m, err := loadIgnoreMatcher(withNegation)
	if err != nil {
		t.Fatalf("loadIgnoreMatcher: %v", err)
	}
	if m.canPrune("build") {
		t.Error("must not prune a directory when a negation could re-include its contents")
	}

	plain := writeFiles(t, map[string]string{".dockerignore": "build/\n"})
	m2, err := loadIgnoreMatcher(plain)
	if err != nil {
		t.Fatalf("loadIgnoreMatcher: %v", err)
	}
	if !m2.canPrune("build") {
		t.Error("a plainly excluded directory should be pruned rather than walked")
	}
}

// ── build context ────────────────────────────────────────────────────────

func TestTarContext(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"package.json":            "{}",
		"src/index.js":            "console.log(1)",
		"node_modules/react/i.js": "// vendored",
		".git/config":             "[remote]",
		"secret.log":              "nope",
		".dockerignore":           "*.log\n",
	})

	rc, err := tarContext(dir, map[string]string{generatedDockerfileName: "FROM scratch\n"})
	if err != nil {
		t.Fatalf("tarContext: %v", err)
	}
	defer rc.Close()

	var names []string
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		names = append(names, h.Name)
	}
	sort.Strings(names)
	joined := strings.Join(names, "\n")

	for _, want := range []string{"package.json", "src/index.js", generatedDockerfileName} {
		if !strings.Contains(joined, want) {
			t.Errorf("build context missing %q\ngot:\n%s", want, joined)
		}
	}

	// shipping the host's node_modules would overwrite the correctly
	// installed tree in the build stage
	for _, unwanted := range []string{"node_modules", ".git", "secret.log"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("build context must not include %q\ngot:\n%s", unwanted, joined)
		}
	}
}

// ── image references ─────────────────────────────────────────────────────

func TestImageRef(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"my-app-abc123", "golaunch/my-app-abc123:latest"},
		{"My-App-ABC", "golaunch/my-app-abc:latest"},
		{"golaunch/my-app:deploy-1", "golaunch/my-app:deploy-1"},
		{"my app!", "golaunch/my-app:latest"},
		{"", "golaunch/unnamed:latest"},
	}

	for _, tt := range tests {
		if got := imageRef(tt.in); got != tt.want {
			t.Errorf("imageRef(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ── build output stream ──────────────────────────────────────────────────

func TestStreamBuildOutputSurfacesFailure(t *testing.T) {
	// the daemon reports a failed build inside the stream, not via HTTP
	// status — missing this would let the pipeline start a container from a
	// stale image
	body := strings.NewReader(
		`{"stream":"Step 1/3 : FROM node:22-slim\n"}` + "\n" +
			`{"errorDetail":{"message":"npm ci failed"},"error":"build error"}` + "\n",
	)

	var lines []entities.LogLine
	err := streamBuildOutput(body, func(l entities.LogLine) { lines = append(lines, l) })

	if err == nil {
		t.Fatal("expected a failed build to return an error")
	}
	if !strings.Contains(err.Error(), "npm ci failed") {
		t.Errorf("error should carry the daemon's detail, got %q", err)
	}
	if len(lines) == 0 {
		t.Error("build output should still reach the log sink")
	}
}

func TestStreamBuildOutputSplitsBatchedLines(t *testing.T) {
	body := strings.NewReader(`{"stream":"line one\nline two\nline three\n"}` + "\n")

	var lines []entities.LogLine
	if err := streamBuildOutput(body, func(l entities.LogLine) { lines = append(lines, l) }); err != nil {
		t.Fatalf("streamBuildOutput: %v", err)
	}

	// the daemon batches several lines into one frame; forwarding it whole
	// would render as an unbroken blob in the SSE stream
	if len(lines) != 3 {
		t.Fatalf("got %d log lines, want 3: %+v", len(lines), lines)
	}
	if lines[0].Text != "line one" || lines[2].Text != "line three" {
		t.Errorf("unexpected line split: %+v", lines)
	}
}

func TestStreamBuildOutputCollapsesPullProgress(t *testing.T) {
	// a cold pull of a multi-layer image: each layer sends the same Status
	// repeatedly as bytes arrive (only ProgressDetail changes, which this
	// backend doesn't even parse), then moves to the next phase once.
	var b strings.Builder
	layers := []string{"aaa111", "bbb222", "ccc333"}
	for _, id := range layers {
		for i := 0; i < 50; i++ {
			fmt.Fprintf(&b, `{"status":"Downloading","id":%q,"progressDetail":{"current":%d}}`+"\n", id, i)
		}
		fmt.Fprintf(&b, `{"status":"Pull complete","id":%q}`+"\n", id)
	}

	var lines []entities.LogLine
	if err := streamBuildOutput(strings.NewReader(b.String()), func(l entities.LogLine) { lines = append(lines, l) }); err != nil {
		t.Fatalf("streamBuildOutput: %v", err)
	}

	// 150 "Downloading" frames + 3 "Pull complete" frames per layer must
	// collapse to exactly one line per phase per layer — 2 phases x 3
	// layers = 6. Anything close to 153 means the flood is back.
	if len(lines) != 6 {
		t.Fatalf("got %d log lines from a collapsible pull, want 6: %+v", len(lines), lines)
	}
	if lines[0].Text != "aaa111: Downloading" || lines[1].Text != "aaa111: Pull complete" {
		t.Errorf("unexpected collapsed output: %+v", lines)
	}
}

// ── private registry rewriting ───────────────────────────────────────────

func TestForcePublicRegistryRewritesPrivateNpmMirror(t *testing.T) {
	lockfile := `{
  "packages": {
    "node_modules/zod-validation-error": {
      "version": "4.0.2",
      "resolved": "https://mirror-npm.runflare.com/zod-validation-error/-/zod-validation-error-4.0.2.tgz",
      "integrity": "sha512-abc123"
    }
  }
}`

	out, changed := forcePublicRegistry(nodedetect.PackageManagerNPM, []byte(lockfile))
	if !changed {
		t.Fatal("a private mirror host must be reported as changed")
	}
	if strings.Contains(string(out), "mirror-npm.runflare.com") {
		t.Error("the private mirror host must not survive the rewrite")
	}
	want := `"resolved": "https://registry.npmjs.org/zod-validation-error/-/zod-validation-error-4.0.2.tgz"`
	if !strings.Contains(string(out), want) {
		t.Errorf("rewritten lockfile missing %q, got:\n%s", want, out)
	}
	if !strings.Contains(string(out), `"integrity": "sha512-abc123"`) {
		t.Error("the integrity hash must be left untouched — that's what makes the host swap safe")
	}
}

func TestForcePublicRegistryLeavesLegitimateHostsAlone(t *testing.T) {
	lockfile := `{"resolved": "https://registry.npmjs.org/foo/-/foo-1.0.0.tgz"}`

	out, changed := forcePublicRegistry(nodedetect.PackageManagerNPM, []byte(lockfile))
	if changed {
		t.Error("an already-public registry URL must not be reported as changed")
	}
	if string(out) != lockfile {
		t.Errorf("content must be byte-identical when nothing needs rewriting, got:\n%s", out)
	}
}

func TestForcePublicRegistryRewritesYarnLockfile(t *testing.T) {
	lockfile := "zod-validation-error@^4.0.0:\n" +
		"  version \"4.0.2\"\n" +
		"  resolved \"https://mirror-npm.runflare.com/zod-validation-error/-/zod-validation-error-4.0.2.tgz#abc123def456\"\n" +
		"  integrity sha512-abc123\n"

	out, changed := forcePublicRegistry(nodedetect.PackageManagerYarn, []byte(lockfile))
	if !changed {
		t.Fatal("a private mirror host in yarn.lock must be reported as changed")
	}
	want := `resolved "https://registry.npmjs.org/zod-validation-error/-/zod-validation-error-4.0.2.tgz#abc123def456"`
	if !strings.Contains(string(out), want) {
		t.Errorf("rewritten yarn.lock missing %q, got:\n%s", want, out)
	}
}

func TestForcePublicRegistryLeavesYarnpkgMirrorAlone(t *testing.T) {
	// registry.yarnpkg.com is Yarn's own free public mirror, not a private
	// one — rewriting it would be pointless churn, not a fix.
	lockfile := `resolved "https://registry.yarnpkg.com/foo/-/foo-1.0.0.tgz#abc123"`

	out, changed := forcePublicRegistry(nodedetect.PackageManagerYarn, []byte(lockfile))
	if changed {
		t.Error("registry.yarnpkg.com must not be rewritten")
	}
	if string(out) != lockfile {
		t.Errorf("content must be unchanged, got:\n%s", out)
	}
}

func TestTarContextExtraFilesOverrideRealFile(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"package.json":      "{}",
		"package-lock.json": `{"resolved": "https://mirror-npm.runflare.com/x.tgz"}`,
	})

	rc, err := tarContext(dir, map[string]string{
		"package-lock.json": `{"resolved": "https://registry.npmjs.org/x.tgz"}`,
	})
	if err != nil {
		t.Fatalf("tarContext: %v", err)
	}
	defer rc.Close()

	var matches int
	var content string
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if h.Name != "package-lock.json" {
			continue
		}
		matches++
		buf := make([]byte, h.Size)
		if _, err := io.ReadFull(tr, buf); err != nil {
			t.Fatalf("read entry content: %v", err)
		}
		content = string(buf)
	}

	if matches != 1 {
		t.Fatalf("expected exactly one package-lock.json entry in the tar, got %d", matches)
	}
	if strings.Contains(content, "mirror-npm.runflare.com") {
		t.Errorf("the real on-disk file must not win over the extraFiles override, got:\n%s", content)
	}
	if !strings.Contains(content, "registry.npmjs.org") {
		t.Errorf("the tar must contain the rewritten content, got:\n%s", content)
	}
}
