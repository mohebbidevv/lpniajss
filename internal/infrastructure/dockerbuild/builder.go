// Package dockerbuild implements repository.ImageBuilder on top of the
// Docker daemon. Unlike the host-exec builder — where "building" meant
// running npm in place in the source directory — this produces a real,
// immutable image per deployment, which is what makes rollback and
// reproducible restarts possible.
package dockerbuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"

	"golaunch/internal/domain/entities"
	"golaunch/internal/infrastructure/nodedetect"
)

// imageNamespace prefixes every image this platform builds, so a GC sweep
// can tell its own images apart from whatever else lives on the host.
const imageNamespace = "golaunch"

// defaultBuildTimeout caps a single build. A hung `npm install` otherwise
// occupies a build worker forever; four such builds would wedge the whole
// pool.
const defaultBuildTimeout = 15 * time.Minute

// cpuPeriod is the standard CFS scheduling window, in microseconds. A quota
// of N × cpuPeriod grants N cores' worth of CPU time per window.
const cpuPeriod = 100_000

type DockerImageBuilder struct {
	cli *client.Client
}

func NewDockerImageBuilder(cli *client.Client) *DockerImageBuilder {
	return &DockerImageBuilder{cli: cli}
}

// Build produces an image for the project at req.SourceDir and returns its
// reference. Build output is streamed to logSink line by line as it
// happens, so the caller's SSE stream shows the build live rather than
// dumping it at the end.
func (b *DockerImageBuilder) Build(ctx context.Context, req entities.BuildRequest, logSink func(entities.LogLine)) (string, error) {
	timeout := defaultBuildTimeout
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ref := imageRef(req.ImageTag)

	dockerfileName, extraFiles, err := b.resolveDockerfile(req.SourceDir, logSink)
	if err != nil {
		return "", err
	}

	buildContext, err := tarContext(req.SourceDir, extraFiles)
	if err != nil {
		return "", fmt.Errorf("prepare build context: %w", err)
	}
	defer buildContext.Close()

	resp, err := b.cli.ImageBuild(ctx, buildContext, build.ImageBuildOptions{
		Tags:        []string{ref},
		Dockerfile:  dockerfileName,
		Remove:      true,
		ForceRemove: true,
		// The classic builder, not BuildKit: driving BuildKit through this
		// endpoint requires a side-channel session (options.SessionID) and
		// the buildkit client libraries. Layer caching — the reason the
		// Dockerfile is staged the way it is — works on both, and the
		// classic builder still runs every RUN step inside a container, so
		// the isolation story is unchanged.
		Version: build.BuilderV1,
		Labels: map[string]string{
			"golaunch.project_id": req.ProjectID,
		},
		Memory:     req.Limits.MemoryMB * 1024 * 1024,
		MemorySwap: memorySwapFor(req.Limits.MemoryMB),
		CPUQuota:   int64(req.Limits.CPUCores * cpuPeriod),
		CPUPeriod:  cpuPeriod,
	})
	if err != nil {
		return "", fmt.Errorf("start build: %w", err)
	}
	defer resp.Body.Close()

	if err := streamBuildOutput(resp.Body, logSink); err != nil {
		return "", err
	}

	return ref, nil
}

// resolveDockerfile decides whether to honour a Dockerfile the project ships
// or synthesise one. A project that brings its own is always trusted over
// detection — an author who wrote a Dockerfile knows things about their app
// that package.json can't express.
func (b *DockerImageBuilder) resolveDockerfile(sourceDir string, logSink func(entities.LogLine)) (string, map[string]string, error) {
	// package manager/lockfile detection runs unconditionally — a broken
	// lockfile is orthogonal to whether the project brought its own
	// Dockerfile, so this must not live inside the generate-only branch.
	specs := nodedetect.GetProjectSpecs(sourceDir, ContainerPort)

	extraFiles := map[string]string{}
	if err := b.overridePrivateRegistry(sourceDir, specs, extraFiles, logSink); err != nil {
		return "", nil, err
	}

	if _, err := os.Stat(filepath.Join(sourceDir, "Dockerfile")); err == nil {
		logSink(entities.LogLine{Stream: entities.LogInfo, Text: "using the Dockerfile shipped with this project"})
		return "Dockerfile", extraFiles, nil
	} else if !os.IsNotExist(err) {
		return "", nil, fmt.Errorf("stat Dockerfile: %w", err)
	}

	dockerfile := GenerateDockerfile(specs)

	logSink(entities.LogLine{
		Stream: entities.LogInfo,
		Text: fmt.Sprintf("detected a %s project on Node %s (%s); generating a Dockerfile",
			frameworkName(specs), specs.NodeMajor, specs.PackageManager),
	})

	extraFiles[generatedDockerfileName] = dockerfile
	return generatedDockerfileName, extraFiles, nil
}

// overridePrivateRegistry reads the project's lockfile, if it has one, and
// rewrites any "resolved" entry pointing at a private/third-party registry
// mirror back to the public npm registry — see forcePublicRegistry for why
// this has to touch the lockfile itself rather than just setting a
// registry env var. The rewritten content is added to extraFiles under the
// lockfile's own name, which context.go's tar walk treats as authoritative
// over the real file on disk.
func (b *DockerImageBuilder) overridePrivateRegistry(sourceDir string, specs *nodedetect.ProjectSpecs, extraFiles map[string]string, logSink func(entities.LogLine)) error {
	if specs.Lockfile == "" {
		return nil
	}

	content, err := os.ReadFile(filepath.Join(sourceDir, specs.Lockfile))
	if err != nil {
		return fmt.Errorf("read lockfile: %w", err)
	}

	rewritten, changed := forcePublicRegistry(specs.PackageManager, content)
	if !changed {
		return nil
	}

	extraFiles[specs.Lockfile] = string(rewritten)
	logSink(entities.LogLine{
		Stream: entities.LogInfo,
		Text:   fmt.Sprintf("%s referenced a private registry mirror — rewriting to the public npm registry", specs.Lockfile),
	})
	return nil
}

func frameworkName(specs *nodedetect.ProjectSpecs) string {
	if specs.IsNext {
		return "Next.js"
	}
	return "Node"
}

// memorySwapFor pins swap to the memory limit, which disables swap for the
// build entirely. Without it a memory-hungry build silently thrashes the
// host's swap instead of failing; with it, it gets OOM-killed promptly and
// the developer sees a real error.
func memorySwapFor(memoryMB int64) int64 {
	if memoryMB <= 0 {
		return 0
	}
	return memoryMB * 1024 * 1024
}

// ── build output ─────────────────────────────────────────────────────────

// buildMessage is one frame of the daemon's newline-delimited JSON build
// stream. During an image pull the daemon sends one status frame per chunk
// downloaded — tens or hundreds per layer, all carrying the same Status
// text ("Downloading") with only ProgressDetail changing — so ID is what
// lets streamBuildOutput tell "still the same phase" from "phase changed".
type buildMessage struct {
	Stream string `json:"stream"`
	Status string `json:"status"`
	ID     string `json:"id"`
	Error  string `json:"error"`

	ErrorDetail *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// streamBuildOutput decodes the build stream, forwarding progress to
// logSink and surfacing a failed build as an error.
//
// A build that fails reports it *inside* the stream rather than through the
// HTTP status, so this has to be parsed — returning nil here on a failed
// build would let the pipeline start a container from a stale or missing
// image.
func streamBuildOutput(body io.Reader, logSink func(entities.LogLine)) error {
	decoder := json.NewDecoder(body)

	// last phase seen per layer ID, so a pull's per-chunk progress frames
	// (same Status, same ID, repeated hundreds of times) collapse into one
	// line per real phase change instead of flooding the stream.
	lastStatus := make(map[string]string)

	for {
		var msg buildMessage
		if err := decoder.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode build output: %w", err)
		}

		if msg.Error != "" {
			detail := msg.Error
			if msg.ErrorDetail != nil && msg.ErrorDetail.Message != "" {
				detail = msg.ErrorDetail.Message
			}
			emitLines(logSink, entities.LogStderr, detail)
			return fmt.Errorf("build failed: %s", strings.TrimSpace(detail))
		}

		if msg.Stream != "" {
			emitLines(logSink, entities.LogStdout, msg.Stream)
		}
		if msg.Status != "" && lastStatus[msg.ID] != msg.Status {
			lastStatus[msg.ID] = msg.Status
			emitLines(logSink, entities.LogInfo, statusLine(msg))
		}
	}
}

// statusLine prefixes a status with its layer ID when there is one, so
// "Downloading" / "Pull complete" for five different layers don't read as
// five identical, unattributable lines.
func statusLine(msg buildMessage) string {
	if msg.ID == "" {
		return msg.Status
	}
	return msg.ID + ": " + msg.Status
}

// emitLines splits a frame into individual log lines. The daemon batches
// several newline-separated lines into one "stream" field, and forwarding
// that as a single LogLine would render as one unbroken blob in the SSE
// stream.
func emitLines(logSink func(entities.LogLine), stream entities.LogStream, text string) {
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		logSink(entities.LogLine{Stream: stream, Text: line})
	}
}

// ── image lifecycle ──────────────────────────────────────────────────────

func (b *DockerImageBuilder) ImageExists(ctx context.Context, imageRef string) (bool, error) {
	if _, err := b.cli.ImageInspect(ctx, imageRef); err != nil {
		if client.IsErrNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("inspect image %s: %w", imageRef, err)
	}
	return true, nil
}

// RemoveImage deletes an image. A missing image is treated as success —
// the caller wanted it gone, and it is.
// PruneBuildCache reclaims build cache entries untouched for keepSince.
//
// Deliberately filtered by age rather than All:true — a blanket prune would
// throw away the warm layer cache that makes an incremental redeploy fast,
// trading a one-off disk win for a permanent build slowdown.
func (b *DockerImageBuilder) PruneBuildCache(ctx context.Context, keepSince time.Duration) (uint64, error) {
	report, err := b.cli.BuildCachePrune(ctx, build.CachePruneOptions{
		Filters: filters.NewArgs(filters.Arg("until", keepSince.String())),
	})
	if err != nil {
		return 0, fmt.Errorf("prune build cache: %w", err)
	}
	if report == nil {
		return 0, nil
	}
	return report.SpaceReclaimed, nil
}

func (b *DockerImageBuilder) RemoveImage(ctx context.Context, imageRef string) error {
	_, err := b.cli.ImageRemove(ctx, imageRef, image.RemoveOptions{PruneChildren: true})
	if err != nil && !client.IsErrNotFound(err) {
		return fmt.Errorf("remove image %s: %w", imageRef, err)
	}
	return nil
}

// ── reference naming ─────────────────────────────────────────────────────

var invalidRefChars = regexp.MustCompile(`[^a-z0-9._/-]+`)

// imageRef turns a caller-supplied tag into a valid image reference.
// A tag that already carries an explicit ":version" is respected as a full
// reference; anything else becomes golaunch/<name>:latest.
func imageRef(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))

	name, version, hasVersion := strings.Cut(tag, ":")
	name = sanitizeRefPart(name)
	if name == "" {
		name = "unnamed"
	}

	if !strings.Contains(name, "/") {
		name = imageNamespace + "/" + name
	}

	if !hasVersion {
		return name + ":latest"
	}

	version = sanitizeRefPart(version)
	if version == "" {
		version = "latest"
	}
	return name + ":" + version
}

func sanitizeRefPart(s string) string {
	s = invalidRefChars.ReplaceAllString(s, "-")
	return strings.Trim(s, "-._/")
}
