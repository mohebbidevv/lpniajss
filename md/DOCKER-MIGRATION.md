Alright, real blueprint. Following your existing clean-arch layout so nothing feels bolted on.

---

## The key architectural move

Define a **`Runtime` interface in the domain layer**. `ProjectRunner` (host exec) and `DockerRuntime` become two implementations of it. That gets you:

- the feature flag for free (swap implementations at wiring time in `main.go`)
- application layer never imports Docker
- you can delete the host-exec one later without touching use cases

This is the single decision that makes the migration survivable. Everything below hangs off it.

---

## New file tree

```
internal/
├── domain/
│   ├── entities/
│   │   ├── project.go              (MODIFIED — new fields)
│   │   ├── deployment.go           (NEW)
│   │   └── runtime.go              (NEW — value types for runtime layer)
│   └── ports/                      (NEW package)
│       ├── runtime.go              (NEW — the Runtime interface)
│       └── image_builder.go        (NEW — the Builder interface)
│
├── application/
│   ├── run_project.go              (MODIFIED — heavy)
│   ├── project_runner.go           (KEPT as-is, becomes legacy impl)
│   ├── upload_project.go           (MODIFIED — light)
│   ├── log_registry.go             (unchanged)
│   ├── reconcile.go                (NEW)
│   ├── stop_project.go             (NEW)
│   └── delete_project.go           (NEW)
│
├── infrastructure/
│   ├── docker/                     (NEW package)
│   │   ├── client.go
│   │   ├── builder.go
│   │   ├── runtime.go
│   │   ├── logs.go
│   │   ├── events.go
│   │   ├── limits.go
│   │   ├── labels.go
│   │   └── prune.go
│   ├── dockerfile/                 (NEW package)
│   │   ├── generator.go
│   │   ├── detect.go
│   │   └── templates/
│   │       ├── node.dockerfile.tmpl
│   │       └── next.dockerfile.tmpl
│   ├── hostexec/                   (NEW — move project_runner here eventually)
│   │   └── runtime.go
│   ├── caddy/client.go             (MODIFIED)
│   └── database/postgres/
│       ├── project_repo_impl.go    (MODIFIED)
│       └── deployment_repo_impl.go (NEW)
│
└── queue/                          (mostly unchanged, semantics change)
```

---

## The interfaces (domain/ports)

### `ports.Runtime`

The thing your application layer talks to. Methods, described:

- **`Start(ctx, spec) → (handle, error)`** — takes a spec (image ref, env, limits, labels, name), creates + starts a container, returns immediately with an identifier. **Does not block.** This is the death of `cmd.Wait()`.
- **`Stop(ctx, handle, timeout) → error`** — graceful SIGTERM then kill.
- **`Remove(ctx, handle) → error`** — delete the container.
- **`Status(ctx, handle) → (RuntimeStatus, error)`** — running/exited/oom/notfound.
- **`Logs(ctx, handle, opts) → (<-chan LogLine, error)`** — follow, with tail/since options.
- **`List(ctx, labelFilter) → ([]RuntimeInstance, error)`** — for reconciliation.
- **`Events(ctx) → (<-chan RuntimeEvent, error)`** — the lifecycle stream.

### `ports.ImageBuilder`

- **`Build(ctx, req) → (imageRef string, error)`** where req has: source dir, tag, build args, timeout, resource caps, and a log sink func.
- **`RemoveImage(ctx, ref) → error`**
- **`ImageExists(ctx, ref) → (bool, error)`** — reconciliation asks this before deciding to rebuild.

Both interfaces live in domain, both take/return only domain types. No Docker types leak upward.

---

## New domain types

**`entities.Deployment`** — this is the big conceptual addition. Right now a project *is* its deployment; you overwrite status and port in place, so you have no history. You want:

- `ID`, `ProjectID`
- `ImageRef` — what got built
- `ContainerID` — what's running
- `Status` — building/running/stopped/failed/crashed
- `Port` (only if you keep published ports)
- `CreatedAt`, `StartedAt`, `StoppedAt`
- `ExitCode`, `FailureReason`

Why: **rollback** (checklist item), **crash history**, and reconciliation needs to know "which image was this project last successfully on." A project has many deployments; one is current.

**`entities.Project` gains:** `CurrentDeploymentID`, `Slug` (the subdomain, split from the UUID as discussed), and drops direct `Port`/`Status` reliance over time.

**Value types in `entities/runtime.go`:** `RuntimeSpec`, `RuntimeStatus`, `RuntimeEvent`, `RuntimeInstance`, `ResourceLimits`, `LogOptions`. Plain structs, no behavior.

---

## The docker package, file by file

**`client.go`** — thin wrapper owning the `*client.Client`, connection config, ping-on-startup health check. Everything else takes this.

**`labels.go`** — the label schema, centralized. `launchpad.managed=true`, `launchpad.project_id`, `launchpad.deployment_id`, `launchpad.slug`. Plus helpers to build filter args and parse labels back into a struct. **Put this in one file** — label typos are the kind of bug that silently returns zero results and costs you an hour.

**`limits.go`** — translates `entities.ResourceLimits` into Docker's `HostConfig` (memory, nanoCPUs, pidsLimit, cap drops, read-only rootfs, tmpfs mounts, restart policy). Isolated because you'll tune these constantly and you want one place to look.

**`builder.go`** — implements `ports.ImageBuilder`. Tars the build context, calls ImageBuild, decodes the JSON build-event stream into log lines, enforces timeout via context.

**`runtime.go`** — implements `ports.Runtime`. Container create/start/stop/remove/list/status.

**`logs.go`** — the `stdcopy.StdCopy` demux, container-logs-to-`LogLine`-channel adapter. Separate file because the multiplexing is fiddly and you don't want it tangled into runtime.go.

**`events.go`** — subscribes to the Docker event stream, filters by your label, emits `RuntimeEvent`. Handles reconnect on stream drop.

**`prune.go`** — image/container cleanup routines.

---

## The dockerfile package

**`detect.go`** — this is your existing `isNext` / `readPackageJSON` / `resolveStartCommand` logic, **moved out of `project_runner.go`** and made pure. Input: a directory path. Output: a `ProjectSpec` struct — framework, node version, package manager (npm/yarn/pnpm — detect by lockfile), install command, build command, start command, needs-build bool.

Making this pure and standalone is worth it on its own: it's testable, it's reusable by both runtimes, and it's the thing you'll extend most often as you support more frameworks.

**`generator.go`** — takes `ProjectSpec`, picks a template, renders a Dockerfile to the source dir.

**`templates/`** — Go text/template files. Multi-stage, non-root user, cache-friendly layer order.

---

## Application layer changes

**`run_project.go`** — `RunProjectUseCase` gains `Builder ports.ImageBuilder` and `Runtime ports.Runtime`. `Execute` stays a fast receptionist (validate → create Deployment row → submit job). The heavy lifting moves into a new **`DeployPipeline`** — probably its own file, `deploy_pipeline.go` — with explicit stages:

1. resolve source root
2. detect spec
3. generate Dockerfile
4. build image (stream logs)
5. stop+remove previous container if any
6. start new container
7. register Caddy route
8. mark deployment running

Each stage updates the Deployment row and emits log lines. Failure at any stage → mark failed, keep the *previous* container alive if it exists (that's your zero-downtime-ish behavior from the original checklist).

**`reconcile.go`** — the rewritten reconciler. Lists containers by label, lists deployments from DB, diffs, acts. Three cases as discussed.

**`stop_project.go` / `delete_project.go`** — you don't have these yet and containers make them necessary: stop = container stop + Caddy route removal; delete = that plus image removal, source cleanup, DB cascade.

---

## Queue semantics change

`workerpool.go` code barely changes, but its *meaning* does: it becomes a **build queue**, not a run queue. Jobs are short-lived (build+start, then done). Worker count now bounds concurrent *builds*, not concurrent running apps.

Concretely: drop `maxWorkers` from 15 to something like 2–4. Fifteen simultaneous `npm install`s will thrash a small VPS. And bump the per-job timeout — 5 minutes is tight for a cold Docker build.

Also: your current retry logic (`RetryCount < 3`, blocking `time.Sleep` inside `Process`) will happily retry a build that fails because the user's code is broken. Retrying a deterministic failure three times is just wasting three build slots. Worth adding a distinction between retryable (daemon hiccup) and terminal (build failed) errors.

---

## Migrations needed

1. `deployments` table + FK to projects, index on `project_id` and on `status`
2. `projects` add `slug` (unique), `current_deployment_id`
3. eventually drop `port`/`status` from projects once deployments own them — **do this last**, after everything's reading from the new table

---

## Wiring in main.go

Roughly: build docker client → construct builder + runtime → pick runtime by config flag (`RUNTIME=docker|hostexec`) → pass into use cases → start event-watcher goroutine → run reconcile in background → start HTTP server.

The event watcher is a new long-lived goroutine you don't have today. It needs graceful shutdown handling alongside your worker pool.

---

## What I'd build first, concretely

`dockerfile/detect.go` + `dockerfile/generator.go` + templates, with a tiny CLI or test that points at your `launchpad-test` project and prints the Dockerfile. No Docker SDK, no interfaces, no wiring. You'll immediately learn whether your detection logic is good enough, and it's the input to everything else.

Then `ports.Runtime` + `docker/builder.go`, and get an image to actually build.

---

Two things I'd push back on slightly before you start:

**`ports` as a new package** — your repo interfaces currently live in `domain/repository`. You could just put `Runtime` and `ImageBuilder` there instead of adding a package. Fewer directories, and "repository" is already a slightly loose name in your codebase. Your call, but don't add the package just because it looks more architectural.

**Deployment entity is optional for v1.** You could get containerization working while keeping status/port on Project, and add deployments later. It'd be less work now, more rework later. Given rollback was on your original reliability checklist and it's basically free once deployments exist, I'd do it now — but it is genuinely a scope decision, not a correctness one.

Where do you want to start writing?