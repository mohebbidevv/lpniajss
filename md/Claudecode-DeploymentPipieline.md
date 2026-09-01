# Read event_consumer.go
# Read stop_project.go
# Read reconcile.go
# Read runtime.go
# Read builder.go
# Read main.go
# Read run_project.go

Alright, full walkthrough. I'll go high-level architecture first, then trace one request end-to-end at the low level, then cover the three side-flows (crash, manual stop, startup reconcile).

---

## High level

The core idea: **application code only ever talks to `repository.Runtime` and `repository.ImageBuilder`** (interfaces), never to `hostexec` (or, later, Docker) directly. Today `hostexec` is the only implementation plugged in at `main.go:65-66`. When a Docker implementation of those same two interfaces shows up, you change two lines in `main.go` and nothing in `internal/application/` moves.

---

## Five moving pieces in `internal/application/`:

| File | Job |
|------|-----|
| `run_project.go` | HTTP-facing: validate → register log channel → enqueue a job. Returns instantly. |
| `deploy_pipeline.go` | The actual build-and-launch state machine. Runs inside a worker. |
| `event_consumer.go` | Background goroutine: listens for "a process died" and decides crash vs. intentional. |
| `stop_project.go` | User-initiated shutdown of a project. |
| `reconcile.go` | Runs once at boot: reconciles DB state vs. what's actually running. |

Plus one entity, **Deployment**, which is the whole reason this isn't a single-status-field system anymore: a `Project` has many `Deployment`s over its life, and `Project.CurrentDeploymentID` points at whichever one is currently live. That's what gives you rollback/history instead of overwriting one row in place.

---

## The deploy flow, end to end

### 1. Request comes in
`RunHandler.ServeHTTP` (`run_handler.go`) calls `RunProjectUseCase.Execute(ctx, projectID)`.

### 2. Execute (`run_project.go:38-63`) — deliberately dumb:
- Checks the project exists.
- Makes a buffered channel, registers it in `LogRegistry` keyed by `projectID` (`log_registry.go`).
- Submits a `queue.Job{ProjectID: projectID}` to the worker pool.
- Returns the channel immediately. The HTTP handler starts looping over it and writing SSE frames — that loop is what makes the deploy logs show up live in the browser.

> It does **not** touch ports, deployment rows, or project status anymore. That's the whole point of the "fast receptionist" instruction — all of that moved into the pipeline so it runs off the HTTP request path, inside a worker goroutine, under the pool's own 5-minute timeout.

### 3. A worker picks up the job
In `main.go:70-77`, the processor closure passed to `queue.NewWorkerPool` does two things:
- Grabs the same log channel back out of the registry (so it can `defer close()` it when the job ends — that's what makes the `SSE` for `line := range logCh` loop terminate and the browser stream close),
- Then calls `pipeline.Deploy(ctx, job.ProjectID)`.

### 4. `DeployPipeline.Deploy` (`deploy_pipeline.go:64-177`) — this is the real logic, in exact order:

- Load project + resolve source dir
- Capture "previous" deployment (`project.CurrentDeploymentID`, nil on first deploy)
- Create new `Deployment` row, status=`building`
- Allocate a port (atomic counter, starts at 3000)
- `Builder.Build(...)` [streams install/build logs]
  - Fails → mark new deployment failed, return. Old deployment/route untouched.
- `Runtime.Start(...)` [fast, just spawns the process]
  - Fails → same as above
- `SetContainerInfo` (store the handle, mark deployment `running`)
- `Caddy.RegisterRoute(slug, "localhost:<port>")` ← **THE TRAFFIC FLIP**
  - Fails → stop/remove the new instance we just started, mark deployment failed, old route untouched
- `project.CurrentDeploymentID = new deployment`
- **ONLY NOW:** `Runtime.Stop` + `Runtime.Remove` + mark old deployment `stopped`

> The reason `RegisterRoute` happens before the old deployment is torn down: `RegisterRoute` doesn't add a route, it replaces whatever route already exists for that hostname (`client.go:117-127` — the `matchesHost`/replace logic already existed). So the moment that call succeeds, **100% of new traffic goes to the new process and 0% to the old one** — there's no window where both or neither are receiving traffic. Only after that flip do we kill the old process. If anything before the flip fails, the old process is still running and still has the route — the flip never happened, so nothing changed for users.

### 5. Build vs Start split
Notice `Builder.Build` and `Runtime.Start` are two separate calls. This is the design decision:

- `repository.ImageBuilder.Build(ctx, req, logSink)` already existed in the codebase, unused, and already had a `logSink func(entities.LogLine)` param.
- I implemented `HostExecImageBuilder` (`builder.go`) to be that build step: it runs `npm install` then the framework's build command via `runStreamed` (`builder.go:65-98`), which pipes stdout/stderr line-by-line into `logSink` as they're produced — that's what makes install/build output show up live in the SSE stream instead of only-on-failure.
- Since there's no real "image" for host-exec, `Build` just returns the source directory path as the `imageRef` string. That string is opaque to the pipeline — it just gets handed back into `RuntimeSpec.ImageRef` for `Start`.
- `HostExecRuntime.Start` (`runtime.go:67-98`) got trimmed down: it used to do install+build+start all in one blocking call with a mismatched signature (`Start(ctx, spec, path string)` — that extra `path` param meant it never actually satisfied `repository.Runtime` before this). Now it just reads `spec.ImageRef` as the working directory, resolves the start command, and spawns the process. No `logSink` needed there because it doesn't produce build output — only reads from the already-built directory.

**Net effect:** zero changes to either domain interface. Docker's implementation will map onto exactly the same split naturally — `Build` = docker image build, `Start` = docker container start.

### 6. Labels
`RuntimeSpec.Labels` gets `project_id`, `deployment_id`, `slug`, and `port` (`deploy_pipeline.go:104-112`). The last two weren't explicitly asked for, but I needed them: `Deployment.Port` isn't actually persisted by the current Postgres `Create` query (a pre-existing gap in a file outside my scope — it only inserts `project_id`, `image_ref`, `status`), so reconcile can't read the port back from the DB row. Reading it off the running instance's labels instead sidesteps that gap.

---

## Low-level: how a process's death gets traced back

`HostExecRuntime` stores labels on every `TrackedProcess` (`process.go`). When the process exits (whether it crashed or was `SIGTERM`'d), `watch()` (`runtime.go:102-134`) calls `tp.Cmd.Wait()`, then emits a `RuntimeEvent{Type: Died, Handle, Labels}` onto the runtime's internal events channel.

`EventConsumer.Run` (`event_consumer.go:34-52`) is a goroutine started in `main.go` at boot that just range-loops that channel forever. For each death event, `handle()` (`event_consumer.go:54-106`):

- Pulls `deployment_id` out of the event's labels.
- Loads that `Deployment` row.
- The key check: **if `deployment.Status == DeploymentStopped` already, this death was expected** — `stop_project.go` marks the row `stopped` before it ever touches the runtime (see below), so by the time the process actually exits and this event fires, the row already says "stopped" and the consumer just ignores it.
- Otherwise, this was unsolicited — mark it `SetFailed` (reason + exit code) then `UpdateStatus(..., DeploymentCrashed)`, and remove its Caddy route so users stop getting proxied to a dead process.

> This ordering — **DB write before the actual stop/kill** — is the same trick used in `stop_project.go` (`stop_project.go:54` happens before `Runtime.Stop` at line 63). It's a one-bit flag that lets the async event consumer distinguish "I killed this on purpose" from "this fell over on its own," without any locking between the two goroutines — by the time the SIGTERM lands and the process actually dies, the row already reflects intent.

---

## Low-level: reconcile at boot

`Reconciler.Run` (`reconcile.go:41-97`), started as a goroutine in `main.go`, runs once:
- Pulls `DeploymentRepo.ListByStatus(Running)` (what the DB thinks is live)
- And `Runtime.List(ctx, nil)` (what's actually running right now — for host-exec this is just whatever's still in the in-memory process map, which is empty after a restart; the "orphan" case matters much more once this is Docker, where containers survive an app restart).
- Indexes runtime instances by their `deployment_id` label, then walks both sets:

  - **DB says running, runtime doesn't have it** → the process is gone (this is the normal case after an app restart with host-exec) → call `Pipeline.Deploy` again to bring it back up.
  - **Both agree** → don't touch the deployment, just re-`RegisterRoute` using the slug/port labels, in case Caddy itself got restarted and lost its config.
  - **Runtime has it, DB doesn't know it as running** → orphan from a previous run → `Stop` + `Remove` it.

---

## What else changed and why

- `caddy/client.go` used to import `application` (for `ReconcileRunningProjects`, which called back into `RunProjectUseCase`), while `application` imports `caddy` (for `*caddy.CaddyClient`) — a real import cycle that already made `go build ./...` fail before I touched anything. I deleted `ReconcileRunningProjects` from caddy entirely since `reconcile.go` is its full replacement with a fundamentally different mechanism (label-diffing instead of just re-running every "running" project).

- `project_runner.go` (the old host-exec runner) was also already broken — it called `hasDependency`/`resolveStartCommand` that had already been moved into `hostexec/detect.go` mid-migration — and fully superseded by the new pipeline, so I deleted it rather than patch dead code.

- `upload_project.go` had a call to `entities.NewProject` with one argument silently missing (passing `"zip"` into the `uniqueKey` slot) — fixed to pass the generated storage ID as the unique key, which is what it already was for the zip path.