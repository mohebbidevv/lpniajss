package application

import (
	"context"
	"fmt"
	"time"

	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/caddy"
	"golaunch/internal/infrastructure/diskspace"
)

const stopTimeoutSeconds = 10

const (
	defaultBuildTimeout = 15 * time.Minute
	defaultReadyTimeout = 90 * time.Second
)

// DeployConfig is the resource and timing policy for a deploy. Zero fields
// fall back to platform defaults, so an empty value is usable.
type DeployConfig struct {
	BuildTimeout time.Duration
	BuildLimits  entities.ResourceLimits
	AppLimits    entities.ResourceLimits
	ReadyTimeout time.Duration

	// DataRoot is the filesystem the runtime writes images to, and
	// MinBuildDiskBytes the headroom required before a build may start.
	// Zero for either disables the pre-flight check.
	DataRoot          string
	MinBuildDiskBytes uint64
}

func (c DeployConfig) withDefaults() DeployConfig {
	if c.BuildTimeout == 0 {
		c.BuildTimeout = defaultBuildTimeout
	}
	if c.ReadyTimeout == 0 {
		c.ReadyTimeout = defaultReadyTimeout
	}
	return c
}

// DeployPipeline owns the whole build-and-flip sequence for a project. It
// talks only to repository.Runtime / repository.ImageBuilder — never to a
// concrete runtime package — so a Docker implementation of those interfaces
// is a drop-in swap.
type DeployPipeline struct {
	ProjectRepo    repository.ProjectRepository
	DeploymentRepo repository.DeploymentRepository
	Runtime        repository.Runtime
	Builder        repository.ImageBuilder
	GitSource      repository.GitSource
	EnvVarRepo     repository.EnvVarRepository
	Caddy          *caddy.CaddyClient
	Registry       *LogRegistry
	Config         DeployConfig

	// ImageGC is optional. When set, a successful deploy kicks off a
	// retention sweep for the project it just deployed.
	ImageGC *ImageGC
}

func NewDeployPipeline(
	projectRepo repository.ProjectRepository,
	deploymentRepo repository.DeploymentRepository,
	runtime repository.Runtime,
	builder repository.ImageBuilder,
	gitSource repository.GitSource,
	envVarRepo repository.EnvVarRepository,
	caddyClient *caddy.CaddyClient,
	registry *LogRegistry,
	cfg DeployConfig,
) *DeployPipeline {
	return &DeployPipeline{
		ProjectRepo:    projectRepo,
		DeploymentRepo: deploymentRepo,
		Runtime:        runtime,
		Builder:        builder,
		GitSource:      gitSource,
		EnvVarRepo:     envVarRepo,
		Caddy:          caddyClient,
		Registry:       registry,
		Config:         cfg.withDefaults(),
	}
}

// Deploy runs one build-and-start attempt for projectID and, on success,
// flips the live Caddy route to it before tearing down whatever was running
// before. The route flip happens BEFORE the old deployment is stopped —
// that ordering is what keeps the project's existing site up if this
// deploy fails, and avoids a gap in service if it succeeds. Don't reorder it.
func (p *DeployPipeline) Deploy(ctx context.Context, projectID string) error {
	project, err := p.ProjectRepo.GetByID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project not found: %w", err)
	}

	// git-sourced project: re-sync from origin before every build. This is
	// what makes "redeploy" free — there's no separate redeploy path, hitting
	// run again just pulls latest first.
	if project.RepoURL != nil {
		ref := ""
		if project.RepoRef != nil {
			ref = *project.RepoRef
		}
		p.streamLog(ctx, projectID, LogLine{Stream: "info", Text: "pulling latest from GitHub..."})
		if err := p.GitSource.Sync(ctx, *project.RepoURL, ref, project.SourceLocation); err != nil {
			return fmt.Errorf("git sync failed: %w", err)
		}
	}

	sourceRoot, err := ResolveProjectRoot(project.SourceLocation)
	if err != nil {
		return fmt.Errorf("resolve source root: %w", err)
	}

	// capture the previously-live deployment (nil on a project's first deploy)
	var previous *entities.Deployment
	if project.CurrentDeploymentID != nil {
		previous, err = p.DeploymentRepo.GetByID(ctx, *project.CurrentDeploymentID)
		if err != nil {
			return fmt.Errorf("load previous deployment: %w", err)
		}
	}

	deployment := entities.NewDeployment(project.ID, "")
	deploymentID, err := p.DeploymentRepo.Create(ctx, deployment)
	if err != nil {
		return fmt.Errorf("create deployment row: %w", err)
	}
	deployment.ID = deploymentID

	// first deploy ever: nothing is serving yet, so it's fair to call the
	// project "building". A redeploy leaves the project's status alone —
	// the previous deployment is still live and serving traffic.
	if previous == nil {
		_ = p.ProjectRepo.UpdateStatus(ctx, project.ID, entities.StatusBuilding)
	}

	logSink := func(line entities.LogLine) {
		p.streamLog(ctx, projectID, LogLine{Stream: string(line.Stream), Text: line.Text})
	}

	labels := map[string]string{
		"project_id":    project.ID,
		"deployment_id": deployment.ID,
		"slug":          project.Slug,
	}

	// Pre-flight, so a full disk produces one clear sentence instead of a
	// cryptic "no space left on device" buried in a build log the user can
	// do nothing about.
	if msg, ok := p.checkDiskHeadroom(); !ok {
		p.streamLog(ctx, projectID, LogLine{Stream: "stderr", Text: msg})
		return p.fail(ctx, project.ID, deployment.ID, "", previous, msg)
	}

	imageRef, err := p.Builder.Build(ctx, entities.BuildRequest{
		ProjectID: project.ID,
		SourceDir: sourceRoot,
		ImageTag:  fmt.Sprintf("%s-%s", project.Slug, deployment.ID),
		Timeout:   int(p.Config.BuildTimeout.Seconds()),
		Limits:    p.Config.BuildLimits,
	}, logSink)
	if err != nil {
		// nothing was actually built, so there's no image to clean up
		return p.fail(ctx, project.ID, deployment.ID, "", previous, fmt.Sprintf("build failed: %v", err))
	}

	// bookkeeping only — a failure here doesn't affect whether this
	// deployment is safe to run, so it's a warning rather than a failed
	// deploy. But it does mean this deployment can't be a rollback target
	// later, since nothing else records which image it used.
	if err := p.DeploymentRepo.SetImageRef(ctx, deployment.ID, imageRef); err != nil {
		p.streamLog(ctx, projectID, LogLine{Stream: "stderr", Text: fmt.Sprintf("warning: failed to record image ref: %v", err)})
	}

	envVars, err := p.EnvVarRepo.ListForProject(ctx, project.ID)
	if err != nil {
		return p.fail(ctx, project.ID, deployment.ID, imageRef, previous, fmt.Sprintf("load env vars failed: %v", err))
	}

	spec := entities.RuntimeSpec{
		DeploymentID: deployment.ID,
		ProjectID:    project.ID,
		Slug:         project.Slug,
		ImageRef:     imageRef,
		Env:          appEnv(envVars),
		Limits:       p.Config.AppLimits,
		Labels:       labels,
	}

	handle, err := p.Runtime.Start(ctx, spec)
	if err != nil {
		return p.fail(ctx, project.ID, deployment.ID, imageRef, previous, fmt.Sprintf("start failed: %v", err))
	}

	// success — store the handle and mark this deployment running
	if err := p.DeploymentRepo.SetContainerInfo(ctx, deployment.ID, string(handle)); err != nil {
		return fmt.Errorf("record container info: %w", err)
	}

	// Start only means the process was launched. Flipping traffic before
	// the app is listening would serve errors for the first seconds of
	// every deploy, so the readiness gate comes first.
	p.streamLog(ctx, projectID, LogLine{Stream: "info", Text: "waiting for the app to start serving..."})
	if err := p.Runtime.WaitReady(ctx, handle, p.Config.ReadyTimeout); err != nil {
		p.captureFailureLogs(ctx, projectID, handle)
		p.discard(ctx, handle)
		return p.fail(ctx, project.ID, deployment.ID, imageRef, previous, fmt.Sprintf("app never became ready: %v", err))
	}

	// only the runtime knows how its instances are addressed
	upstream, err := p.Runtime.Endpoint(ctx, handle)
	if err != nil {
		p.captureFailureLogs(ctx, projectID, handle)
		p.discard(ctx, handle)
		return p.fail(ctx, project.ID, deployment.ID, imageRef, previous, fmt.Sprintf("resolve endpoint failed: %v", err))
	}

	// flip the route to the new deployment. RegisterRoute replaces any
	// existing route for this hostname, so this alone is what makes the
	// new version live — no separate "remove old route" step is needed.
	if err := p.Caddy.RegisterRoute(project.ID, project.Slug, upstream); err != nil {
		// the new instance is up but unreachable — treat as a failed deploy
		// and clean it up. The previous deployment's route is untouched.
		p.captureFailureLogs(ctx, projectID, handle)
		p.discard(ctx, handle)
		return p.fail(ctx, project.ID, deployment.ID, imageRef, previous, fmt.Sprintf("register route failed: %v", err))
	}

	if err := p.ProjectRepo.SetCurrentDeployment(ctx, project.ID, deployment.ID); err != nil {
		return fmt.Errorf("update current deployment: %w", err)
	}
	_ = p.ProjectRepo.UpdateStatus(ctx, project.ID, entities.StatusRunning)

	p.streamLog(ctx, projectID, LogLine{Stream: "info", Text: fmt.Sprintf("live at http://%s.localhost", project.Slug)})

	// only now that traffic is flowing to the new deployment do we tear
	// down the old one.
	if previous != nil {
		prevHandle := entities.RuntimeHandle(previous.ContainerID)
		if err := p.Runtime.Stop(ctx, prevHandle, stopTimeoutSeconds); err != nil {
			p.streamLog(ctx, projectID, LogLine{Stream: "stderr", Text: fmt.Sprintf("warning: failed to stop previous deployment: %v", err)})
		}
		if err := p.Runtime.Remove(ctx, prevHandle); err != nil {
			p.streamLog(ctx, projectID, LogLine{Stream: "stderr", Text: fmt.Sprintf("warning: failed to remove previous deployment: %v", err)})
		}
		// previous.ImageRef is deliberately NOT removed here — it's a
		// rollback target the moment it stops being current. Images do
		// accumulate unbounded as a result; a real GC sweep (keep the last
		// N per project) is separate, still-unbuilt work, not this line.
		// only overwrite the previous deployment's status if it was actually
		// still running — if it had already crashed (e.g. EventConsumer beat
		// us to it) or failed, this teardown step shouldn't clobber that
		// history with "stopped".
		if previous.Status == entities.DeploymentRunning {
			if err := p.DeploymentRepo.UpdateStatus(ctx, previous.ID, entities.DeploymentStopped); err != nil {
				p.streamLog(ctx, projectID, LogLine{Stream: "stderr", Text: fmt.Sprintf("warning: failed to mark previous deployment stopped: %v", err)})
			}
		}
	}

	// Reclaim old images for this project, detached: the user's deploy is
	// already done and must not wait on disk housekeeping.
	//
	// context.WithoutCancel keeps the values — the request-scoped logger,
	// so GC output still correlates — while shedding the cancellation,
	// which fires the moment this job returns.
	if p.ImageGC != nil {
		gcCtx := context.WithoutCancel(ctx)
		go p.ImageGC.CollectProject(gcCtx, projectID)
	}

	return nil
}

// checkDiskHeadroom reports whether there is room to build. A failure to
// measure is deliberately treated as "yes": users must never be blocked by
// this check's own inability to answer.
func (p *DeployPipeline) checkDiskHeadroom() (string, bool) {
	if p.Config.DataRoot == "" || p.Config.MinBuildDiskBytes == 0 {
		return "", true
	}
	avail, err := diskspace.Available(p.Config.DataRoot)
	if err != nil {
		return "", true
	}
	if avail >= p.Config.MinBuildDiskBytes {
		return "", true
	}
	return fmt.Sprintf(
		"build cannot start: the platform is low on disk (%d MB free, %d MB required) — please retry shortly",
		avail/(1<<20), p.Config.MinBuildDiskBytes/(1<<20),
	), false
}

// discard tears down an instance that was started but never went live. The
// previous deployment and its route are untouched.
func (p *DeployPipeline) discard(ctx context.Context, handle entities.RuntimeHandle) {
	_ = p.Runtime.Stop(ctx, handle, stopTimeoutSeconds)
	_ = p.Runtime.Remove(ctx, handle)
}

// failureLogTail caps how much of a dying instance's own output gets
// pulled into the deploy log — enough to see a stack trace, not so much
// that a crash-looping app floods the stream.
const failureLogTail = 20

// captureFailureLogs streams a short tail of the instance's own stdout/
// stderr before discard() deletes it. Without this, a startup crash's
// real error never reaches anyone — discard() removes the container
// (and Docker's log buffer along with it) before fail() ever runs, so the
// only thing that survives is the exit code WaitReady already reports.
// Best-effort: if the instance is already gone or Logs errors, this is
// silently skipped — the caller has a real error to report regardless.
func (p *DeployPipeline) captureFailureLogs(ctx context.Context, projectID string, handle entities.RuntimeHandle) {
	logCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	lines, err := p.Runtime.Logs(logCtx, handle, entities.LogOptions{Tail: failureLogTail})
	if err != nil {
		return
	}
	for line := range lines {
		p.streamLog(ctx, projectID, LogLine{Stream: string(line.Stream), Text: line.Text})
	}
}

// fail marks the new deployment failed and leaves the previous deployment
// (and its Caddy route) completely untouched — the user's existing site
// must not go down because a new deploy attempt failed. imageRef is the
// image built for THIS failed attempt (empty if the build itself never
// succeeded) — removed here so a failed deploy doesn't leak an image on
// every retry while debugging.
func (p *DeployPipeline) fail(ctx context.Context, projectID, deploymentID, imageRef string, previous *entities.Deployment, reason string) error {
	_ = p.DeploymentRepo.SetFailed(ctx, deploymentID, reason, nil)
	if previous == nil {
		_ = p.ProjectRepo.UpdateStatus(ctx, projectID, entities.StatusFailed)
	}
	if imageRef != "" {
		if err := p.Builder.RemoveImage(ctx, imageRef); err != nil {
			p.streamLog(ctx, projectID, LogLine{Stream: "stderr", Text: fmt.Sprintf("warning: failed to remove failed-build image: %v", err)})
		}
	}
	p.streamLog(ctx, projectID, LogLine{Stream: "stderr", Text: reason})
	return fmt.Errorf("%s", reason)
}

func (p *DeployPipeline) streamLog(ctx context.Context, projectID string, line LogLine) {
	ch, ok := p.Registry.Get(projectID)
	if !ok {
		return
	}
	select {
	case ch <- line:
	case <-ctx.Done():
	}
}
