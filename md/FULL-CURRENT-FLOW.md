Everything read. Here's the whole thing, end to end.

---

# STAGE 0 — Boot

### `main()` → [cmd/main.go](cmd/main.go)

```go
os.MkdirAll("./uploads", 0755)
os.MkdirAll("./work", 0755)
ctx := context.Background()
```

`ctx` here is **never cancelled**. It's the lifetime context for `EventConsumer`, `Reconciler`, and the HTTP handlers. Nothing shuts them down; the process just exits.

```go
configuration, err := config.LoadConfig()
```

### `config.LoadConfig()` → [config.go:80](internal/infrastructure/config/config.go#L80)

1. `os.Getwd()` + `filepath.Join(cwd, "cmd/configuration.json")` — **the path is relative to the working directory**, so the binary only works when launched from the repo root. `make run` does that; `./cmd/main` from elsewhere does not.
2. `utils.OpenJSON[AppConfig]` — a generic that reads the file and `json.Unmarshal`s into `AppConfig`. Go's field matching is case-insensitive, which is why lowercase `"server"`/`"runtime"` bind to `Server`/`Runtime` with no tags on those fields.
3. Hard requirement check: `DB.Host`, `DB.User`, `DB.Password` non-empty → otherwise error.
4. `cfg.applyDefaults()` — pointer receiver, mutates in place. Fills server port, Caddy URL/domain, then the runtime block. **`GOLAUNCH_RUNTIME` is read here**, lowercased, and overrides whatever the JSON said. Then every zero field gets a default (512MB app / 2048MB build — a webpack build needs far more than the server it produces).
5. `cfg.Runtime.validate()` — value receiver, pure. Rejects any driver that isn't `docker`/`hostexec` and any endpoint mode that isn't `ip`/`dns`. A typo fails at boot with a readable message instead of a nil-pointer panic three layers down.

```go
dbDSN := postgres.BuildDSN(configuration.DB)
dbPool, err := postgres.ConnectDB(ctx, dbDSN)
postgres.RunMigrations(ctx, dbPool, "file://migrations")
```

pgxpool — a connection pool, so concurrent workers and HTTP handlers don't serialize on one connection. Migrations run before anything else touches a table.

```go
registry := application.NewLogRegistry()
dbRepo := postgres.NewProjectRepository(dbPool)
deploymentRepo := postgres.NewDeploymentRepository(dbPool)
caddyClient := caddy.NewCaddyClient(configuration.Caddy.AdminURL, configuration.Caddy.Domain)
```

`LogRegistry` wraps a `sync.Map` — `projectID → chan LogLine`. It exists because the HTTP handler and the worker goroutine are strangers: the handler creates the channel, the worker finds it by project ID. `sync.Map` rather than `map + RWMutex` because the access pattern is write-once-read-many per key.

### `buildDeployStack(ctx, configuration.Runtime)` → [cmd/wire.go:31](cmd/wire.go#L31)

The **only** function in the codebase that knows a concrete runtime type exists. A `switch` on the driver:

**`hostexec` branch** — two constructors, nothing else. `Closer` stays nil.

**`docker` branch:**
1. `dockerx.NewClient(ctx)` → `client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())`. `FromEnv` reads `DOCKER_HOST` and falls back to `/var/run/docker.sock`. Version negotiation makes a `GET /version` call so a newer SDK doesn't hard-fail against an older daemon. Then a `Ping` with a 5s timeout — **fail fast at boot** rather than discovering the socket is unreachable on someone's first deploy.
2. `dockerrun.EnsureNetwork(ctx, cli, cfg.Network)` — `NetworkInspect`; on 404, `NetworkCreate` with driver `bridge`. A 409 from a concurrent creator is treated as success. Must happen before any container create, because `translate` hardcodes the network into both `NetworkMode` and `EndpointsConfig`.
3. `dockerrun.New(cli, Config{...})` — `Config.withDefaults()` runs **once here**, so the hot path reads resolved values.
4. `dockerbuild.NewDockerImageBuilder(cli)` — same `*client.Client`, one connection serving both.

Returns `deployStack{Runtime, Builder, Closer}` — three interface/io values. Everything downstream sees `repository.Runtime`, never `*DockerRuntime`.

```go
pipeline := application.NewDeployPipeline(dbRepo, deploymentRepo, stack.Runtime, stack.Builder,
    gitSource, caddyClient, registry, deployConfig(configuration.Runtime))
```

`deployConfig` maps the flat JSON block onto `DeployConfig`, converting `int` seconds to `time.Duration` and splitting the two resource budgets (`BuildLimits` vs `AppLimits`).

### The processor closure

```go
var eventConsumer *application.EventConsumer   // declared first, assigned later

processor := func(ctx context.Context, job queue.Job) error {
	logCh, ok := registry.Get(job.ProjectID)
	if ok {
		defer registry.Delete(job.ProjectID)
		defer close(logCh)
	}
	err := pipeline.Deploy(ctx, job.ProjectID)
	if err == nil && eventConsumer != nil {
		eventConsumer.ResetCrashCount(job.ProjectID)
	}
	return err
}
```

The `var eventConsumer` declared *before* the closure and assigned *after* is a deliberate cycle-break: the closure needs the consumer, the consumer needs the pool, the pool needs the closure. The `!= nil` guard covers the window where a job could theoretically run before assignment.

**Defer order is LIFO**: `close(logCh)` runs first, then `registry.Delete`. So the SSE handler's `for range logCh` terminates before the key disappears — no window where a late `streamLog` finds the channel in the registry but already closed. (A `streamLog` racing *between* those two defers would panic on send-to-closed, but both defers run in the same goroutine with no yield between them.)

```go
workerPool := queue.NewWorkerPool(4, processor)
workerPool.Start()
defer workerPool.ShutDown()

eventConsumer = application.NewEventConsumer(dbRepo, deploymentRepo, stack.Runtime, caddyClient, workerPool)
go eventConsumer.Run(ctx)

reconciler := application.NewReconciler(deploymentRepo, stack.Runtime, caddyClient, pipeline)
go reconciler.Run(ctx)

packageHttp.InitializeRoutes(ctx, dbPool, workerPool, caddyClient, registry, stack.Runtime, mux)
server.ListenAndServe()
```

`NewWorkerPool` makes `jobs chan Job` buffered at **200** and its own `context.WithCancel(context.Background())`. `Start()` spawns 4 `Worker(i)` goroutines, each parked in a `select` on `wp.ctx.Done()` and `<-wp.jobs`.

`ListenAndServe` blocks forever, so the `defer workerPool.ShutDown()` and `defer dbPool.Close()` never actually run — the process dies by signal.

---

# STAGE 1 — The request

```
GET /run/{projectID}
```

`mux.Handle("/run/{projectID}", withCORS(...))` — Go 1.22 stdlib path patterns. `withCORS` short-circuits `OPTIONS` with 204, otherwise delegates.

### `RunHandler.ServeHTTP` → [run_handler.go:19](internal/infrastructure/http/handlers/run_handler.go#L19)

```go
projectID := r.PathValue("projectID")
w.Header().Set("Content-Type", "text/event-stream")
w.Header().Set("Cache-Control", "no-cache")
w.Header().Set("X-Accel-Buffering", "no")
flusher, ok := w.(http.Flusher)
```

The type assertion to `http.Flusher` is what makes SSE possible — without an explicit `Flush()` after each write, Go buffers the response and the client sees nothing until the handler returns, which for a 3-minute deploy is useless.

```go
logCh, err := h.RunUseCase.Execute(r.Context(), projectID)
```

**`r.Context()`, not the server ctx** — so a browser closing the tab cancels this context. Note where that context does and doesn't reach: it's used for the `GetByID` check, and it's *not* the context the deploy runs under. The deploy continues even if the client disconnects, which is correct — you don't want a closed tab to abort a build.

### `RunProjectUseCase.Execute` → [run_project.go:38](internal/application/run_project.go#L38)

Four steps, deliberately trivial — this is a receptionist, not a worker:

```go
if _, err := uc.ProjectRepo.GetByID(ctx, projectID); err != nil { ... }   // 1. exists?
logCh := make(chan LogLine, 64)                                          // 2. channel
uc.Registry.Register(projectID, logCh)                                   // 3. publish it
err := uc.WP.Submit(queue.Job{ID: utils.NewID(), ProjectID: projectID})  // 4. enqueue
```

Order matters: **register before submit.** A worker can pick the job up on the next scheduler tick; if the channel weren't in the registry yet, the first build log lines would be dropped.

If `Submit` fails (queue full), it unwinds: `Registry.Delete` + `close(logCh)` + error. Without the unwind, a stale channel would sit in the registry forever and the next deploy for that project would write into a channel nobody reads.

```go
return logCh, nil
```

Back in the handler:

```go
for line := range logCh {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", line.Stream, line.Text)
	flusher.Flush()
}
fmt.Fprintf(w, "event: done\ndata: process finished\n\n")
```

The `range` blocks until the processor's `defer close(logCh)` fires. That's the coupling: **the HTTP response stays open for exactly as long as the deploy runs.** The `\n\n` terminates each SSE frame; `event:` names it so the browser's `EventSource` can dispatch per stream type.

---

# STAGE 2 — Queue handoff

### `WorkerPool.Submit` → [workerpool.go:106](internal/queue/workerpool.go#L106)

```go
select {
case wp.jobs <- job:
	return nil
default:
	return fmt.Errorf("BUFFER FULLLLLLLLLLLLLLLLL")
}
```

Non-blocking send with a `default` arm. The buffer is 200; past that, submission fails immediately rather than blocking the HTTP handler. Backpressure surfaces as a 4xx-ish SSE error instead of a hung request.

### `WorkerPool.Worker(id)` → [workerpool.go:56](internal/queue/workerpool.go#L56)

```go
for {
	select {
	case <-wp.ctx.Done():
		return
	case job, ok := <-wp.jobs:
		if !ok { return }
		wp.Process(id, job)
	}
}
```

Four of these run concurrently. A Go `select` with multiple ready cases picks pseudo-randomly, and multiple goroutines receiving from one channel means exactly one gets each job — that's the work distribution, no dispatcher needed.

### `WorkerPool.Process` → [workerpool.go:74](internal/queue/workerpool.go#L74)

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
defer cancel()
err := wp.processor(ctx, job)
```

**A fresh `context.Background()`, deliberately detached from the request.** This is why closing the browser tab doesn't kill the deploy.

**But note the 5 minutes.** See the findings at the end — this is the ceiling on everything downstream.

```go
if err != nil {
	wp.stats.failed++
	if job.RetryCount < 3 {
		job.RetryCount++
		backoff := time.Duration(job.RetryCount) * 2 * time.Second
		time.Sleep(backoff)
		wp.Submit(job)
	}
}
```

`time.Sleep` here blocks **the worker**, not a timer goroutine — one of four workers is unavailable for up to 6 seconds. And on retry, the log channel is already closed and deleted, so `registry.Get` returns `ok == false` and all retry output goes nowhere. The client saw `event: done` long ago.

---

# STAGE 3 — Deploy: resolving the source

### `DeployPipeline.Deploy(ctx, projectID)` → [deploy_pipeline.go:81](internal/application/deploy_pipeline.go#L81)

```go
project, err := p.ProjectRepo.GetByID(ctx, projectID)
```

Gives `Slug`, `SourceLocation`, `RepoURL`, `RepoRef`, `CurrentDeploymentID`.

```go
if project.RepoURL != nil {
	p.streamLog(ctx, projectID, LogLine{Stream: "info", Text: "pulling latest from GitHub..."})
	p.GitSource.Sync(ctx, *project.RepoURL, ref, project.SourceLocation)
}
```

`RepoURL` is `*string` — nil distinguishes a zip-uploaded project from a git-imported one. Sync runs on **every** deploy, which is why there's no separate "redeploy" endpoint: hitting `/run` again pulls latest first.

### `streamLog` → [deploy_pipeline.go:246](internal/application/deploy_pipeline.go#L246)

```go
ch, ok := p.Registry.Get(projectID)
if !ok { return }
select {
case ch <- line:
case <-ctx.Done():
}
```

Two guards. `!ok` covers deploys with no listener (reconciler-triggered, crash-restart-triggered) — those silently drop logs, which is intended. The `select` on `ctx.Done()` covers a full 64-slot buffer with a dead SSE reader: without it, a disconnected client would deadlock the entire deploy.

```go
sourceRoot, err := ResolveProjectRoot(project.SourceLocation)
```

`os.ReadDir`; if there's exactly one entry and it's a directory, step into it. Handles zips packed with a wrapper folder (`myapp.zip` → `myapp/package.json`) versus zips packed at the root.

```go
var previous *entities.Deployment
if project.CurrentDeploymentID != nil {
	previous, err = p.DeploymentRepo.GetByID(ctx, *project.CurrentDeploymentID)
}
```

Captured **before** anything changes. `previous == nil` is the "first deploy ever" signal and it drives two later decisions: whether to mark the project `building`, and whether a failure marks the project `failed`.

```go
deployment := entities.NewDeployment(project.ID, "")
deploymentID, err := p.DeploymentRepo.Create(ctx, deployment)
deployment.ID = deploymentID
```

`INSERT INTO deployments (project_id, image_ref, status, created_at) ... RETURNING id`. Status starts `building`. The image ref is `""` — the tag needs the deployment ID, which doesn't exist until this INSERT returns. (It's never written back; see findings.)

```go
if previous == nil {
	p.ProjectRepo.UpdateStatus(ctx, project.ID, entities.StatusBuilding)
}
```

A **redeploy deliberately leaves the project status alone** — the old version is still serving traffic, so calling the project "building" would be a lie to anyone reading the dashboard.

```go
logSink := func(line entities.LogLine) {
	p.streamLog(ctx, projectID, LogLine{Stream: string(line.Stream), Text: line.Text})
}

labels := map[string]string{
	"project_id":    project.ID,
	"deployment_id": deployment.ID,
	"slug":          project.Slug,
}
```

`logSink` is an adapter closure — `entities.LogLine` (domain) → `application.LogLine` (SSE). It closes over `ctx` and `projectID`, so the builder can emit logs without knowing anything about SSE, registries, or HTTP.

---

# STAGE 4 — Deploy: the build

```go
imageRef, err := p.Builder.Build(ctx, entities.BuildRequest{
	ProjectID: project.ID,
	SourceDir: sourceRoot,
	ImageTag:  fmt.Sprintf("%s-%s", project.Slug, deployment.ID),
	Timeout:   int(p.Config.BuildTimeout.Seconds()),
	Limits:    p.Config.BuildLimits,
}, logSink)
```

Tag is `<slug>-<deploymentID>` — unique per deployment, which is what makes images immutable and rollback possible in principle.

### `DockerImageBuilder.Build` → [builder.go:55](internal/infrastructure/dockerbuild/builder.go#L55)

```go
timeout := defaultBuildTimeout
if req.Timeout > 0 { timeout = time.Duration(req.Timeout) * time.Second }
ctx, cancel := context.WithTimeout(ctx, timeout)
defer cancel()
```

Derived from the caller's context, so **whichever deadline is sooner wins** — this matters, see findings.

```go
ref := imageRef(req.ImageTag)
```

Lowercases, splits on `:`, replaces anything outside `[a-z0-9._/-]` with `-`, prefixes `golaunch/` if there's no slash, appends `:latest` if there's no tag. `myapp-abc123` → `golaunch/myapp-abc123:latest`. The namespace is what lets a GC sweep tell our images from everything else on the host.

### `resolveDockerfile` → [builder.go:110](internal/infrastructure/dockerbuild/builder.go#L110)

```go
if _, err := os.Stat(filepath.Join(sourceDir, "Dockerfile")); err == nil {
	logSink(... "using the Dockerfile shipped with this project")
	return "Dockerfile", nil, nil
}
specs := nodedetect.GetProjectSpecs(sourceDir, ContainerPort)
dockerfile := GenerateDockerfile(specs)
return generatedDockerfileName, map[string]string{generatedDockerfileName: dockerfile}, nil
```

A project's own Dockerfile always wins — its author knows things `package.json` can't express. The generated one is written as `.golaunch.Dockerfile` so it can never clobber a real one, and the name makes it obvious in a build log that it was synthesised.

### `nodedetect.GetProjectSpecs(path, 3000)` → [detect.go:53](internal/infrastructure/nodedetect/detect.go#L53)

Pure filesystem inspection, no network, no execution:

- **`readPackageJSON`** — `os.ReadFile` + `json.Unmarshal` into a struct with `Scripts`, `Main`, `Dependencies`, `Engines`. **Errors are swallowed**: a missing or malformed `package.json` yields the zero value, and detection falls through to `node index.js`.
- **`isNext`** — any of `next.config.{js,ts,mjs}` on disk, or `next` in dependencies.
- **`detectPackageManager`** — a `switch` on lockfile existence, checked **pnpm → yarn → npm**. That order is the whole logic: a repo with both `pnpm-lock.yaml` and `package-lock.json` is almost always a pnpm project with a stale npm lockfile someone forgot to delete.
- **`detectNodeMajor`** — `engines.node`, then `.nvmrc`, then `"22"`. `firstMajor` regexes out the first integer, so `">=18"`, `"^20 || ^22"`, `"20.x"` all reduce to the *lowest* supported major — the safe end of a range to build against.
- **`installCmd`** — with a lockfile: `npm ci --no-audit --no-fund` / `pnpm install --frozen-lockfile` / `yarn install --frozen-lockfile`. These **fail loudly** when the lockfile disagrees with `package.json`, which for a deploy platform beats silently installing versions the developer never tested.
- **`buildCmd`** — the project's own `build` script first (covers every framework, not just Next), `npx next build` only as a fallback.
- **`startCmd`** — Next gets `npx next start -p 3000` because **`next start` does not read `$PORT`**. Everything else falls through `start` script → `dev` script → `main` field → `node index.js`, all expected to honour `PORT`.

### `GenerateDockerfile(specs)` → [dockerfile.go:31](internal/infrastructure/dockerbuild/dockerfile.go#L31)

Three stages, and the staging is entirely about cache reuse:

**deps** — `COPY package.json ./`, then the lockfile *only if one exists* (a `COPY` of a missing path is a hard build failure), then `RUN npm ci`. Because only the manifest was copied, **this layer's cache key doesn't include your source** — the expensive install is reused across every deploy where dependencies didn't change. Copying source first would invalidate it on every commit.

**build** — `COPY --from=deps /app/node_modules ./node_modules`, `COPY . .`, `RUN npm run build`.

**runtime** —
```
ENV NODE_ENV=production      ← only here; setting it in deps would skip devDependencies the build needs
ENV PORT=3000
ENV HOSTNAME=0.0.0.0         ← Next's standalone server binds localhost without this and is unreachable
COPY --from=build --chown=node:node /app ./
USER node                    ← non-root PID 1
EXPOSE 3000
HEALTHCHECK --interval=1s --timeout=3s --start-period=2s --retries=60 CMD ["node","-e","..."]
CMD ["npm","start"]          ← exec form
```

`writeCorepack` emits `RUN corepack enable pnpm` for non-npm projects — the node images ship npm only, so a pnpm project's install command isn't on `PATH` at all without it.

`execForm` uses `json.Marshal` to produce `["npm","start"]`. **Shell form would put `/bin/sh -c` at PID 1, and sh does not forward SIGTERM to its child** — every `Stop` would be a hard kill with no graceful drain. `Stop`'s entire graceful behaviour depends on this one function.

The `HEALTHCHECK` probe is `require('http').get('http://127.0.0.1:3000/',()=>process.exit(0)).on('error',()=>process.exit(1))`. `node -e` rather than curl because slim images ship neither curl nor wget. **Any HTTP response counts** — a 404 still proves the server is listening, which is the question being asked.

### `tarContext(sourceDir, extraFiles)` → [context.go:167](internal/infrastructure/dockerbuild/context.go#L167)

1. **`loadIgnoreMatcher`** — starts from `alwaysExclude` (`node_modules`, `**/node_modules`, `.git`, `**/.git`) and appends `.dockerignore` if present. `node_modules` is the critical one: the build stage does `COPY . .` **on top of** the node_modules the deps stage installed, so shipping the uploader's copy would overwrite a correctly-installed tree with one possibly built for a different platform. `.git` is dead weight and routinely holds credentials in its remote config.
2. Each line: strip `!` (negation), `path.Clean`, split on `/` into segments.
3. **`io.Pipe`** + a writer goroutine. Streaming, not buffering — a large project never has to fit in memory twice on its way to the daemon.
4. `filepath.WalkDir` with `matchSegments` for Docker's glob semantics: `*` matches within one segment, `**` recurses via `for i := 0; i <= len(name); i++`. `canPrune` returns `fs.SkipDir` for excluded directories, **but only when there are no negations anywhere** — a `!keep-me` line could re-include something nested inside, so pruning would be wrong.
5. `writeTarEntry` — `tar.FileInfoHeader`, `header.Name = rel`. Non-regular files (sockets, devices, fifos) get a header but no payload: the daemon has no use for them and reading one can block forever.
6. `writeExtraFiles` appends the generated `.golaunch.Dockerfile` as a synthetic entry — it never touches disk.

### `cli.ImageBuild` → the daemon

`POST /build?t=golaunch/myapp-abc:latest&dockerfile=.golaunch.Dockerfile&...` with the tar as the request body.

```go
Version:    build.BuilderV1,
Memory:     req.Limits.MemoryMB * 1024 * 1024,
MemorySwap: memorySwapFor(req.Limits.MemoryMB),
CPUQuota:   int64(req.Limits.CPUCores * cpuPeriod),
CPUPeriod:  cpuPeriod,   // 100_000 µs
```

Classic builder, not BuildKit — driving BuildKit through this endpoint needs a side-channel session and the buildkit client libraries. Layer caching works on both, and **the classic builder still runs every `RUN` step inside a container**, so `npm install`'s arbitrary `postinstall` scripts are sandboxed either way. That's already a massive improvement over host-exec, where they run as your user on the host.

### `streamBuildOutput(resp.Body, logSink)` → [builder.go:180](internal/infrastructure/dockerbuild/builder.go#L180)

```go
decoder := json.NewDecoder(body)
for {
	var msg buildMessage
	if err := decoder.Decode(&msg); err != nil {
		if errors.Is(err, io.EOF) { return nil }
		return fmt.Errorf("decode build output: %w", err)
	}
	if msg.Error != "" { ...; return fmt.Errorf("build failed: %s", detail) }
	if msg.Stream != "" { emitLines(logSink, entities.LogStdout, msg.Stream) }
	if msg.Status != "" { emitLines(logSink, entities.LogInfo, msg.Status) }
}
```

**Critical detail: a failed build reports it *inside* the stream, not through the HTTP status.** The response is 200 with an `{"error": "..."}` frame near the end. Returning nil here on a failed build would let the pipeline start a container from a stale or missing image.

`emitLines` splits on `\n` because the daemon batches several lines into one `stream` field — forwarding that as one `LogLine` would render as an unbroken blob in the SSE stream.

Build failure → `p.fail(...)` → deployment marked failed, **previous deployment and its route untouched.**

---

# STAGE 5 — Deploy: start

```go
spec := entities.RuntimeSpec{
	DeploymentID: deployment.ID,
	ProjectID:    project.ID,
	Slug:         project.Slug,
	ImageRef:     imageRef,
	Env:          appEnv(),          // ← []string{"NODE_ENV=production"} and nothing else
	Limits:       p.Config.AppLimits,
	Labels:       labels,
}
handle, err := p.Runtime.Start(ctx, spec)
```

**`spec.Port` is deliberately zero.** Which port an app listens on is a runtime concern — Docker fixes it at 3000, host-exec allocates a free one — and each injects `PORT` itself.

### `DockerRuntime.Start` → [runtime.go:38](internal/infrastructure/dockerrun/runtime.go#L38)

```go
name := containerName(spec)                        // "golaunch-<deploymentID>"
cfg, hostCfg, netCfg := r.translate(spec)
created, err := r.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, name)
if errdefs.IsConflict(err) {
	r.remove(ctx, name, true)                      // reclaim a stale name
	created, err = r.cli.ContainerCreate(...)
}
if err != nil { return "", ... }

if err := r.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
	_ = r.remove(ctx, created.ID, true)            // don't leak a container holding the name
	return "", ...
}
return entities.RuntimeHandle(created.ID), nil
```

The name is derived from the **deployment ID, not the slug** — during a route flip both old and new containers are alive on the same network, and a shared name (or a shared DNS alias) would make Docker's resolver round-robin between old and new. Deployment-scoped names keep the Caddy swap the single atomic switch.

`errdefs.IsConflict` works because `client/request.go` wraps every non-2xx through `errhttp.ToNative(statusCode)`; 409 → `errdefs.ErrConflict`, found by walking the `Unwrap()` chain — no string matching. `IsConflict(nil)` is false, so calling it before the `err != nil` check is safe.

**`translate`** produces the full security policy (verified live against the daemon): `CapDrop: [ALL]`, `no-new-privileges:true`, `ReadonlyRootfs`, tmpfs at `/tmp` and `/app/.next/cache` with `noexec,nosuid`, `Memory == MemorySwap` (swap off → prompt OOM kill instead of host thrash), `NanoCPUs`, `PidsLimit`, `nofile`/`nproc` ulimits, `Init: true` (tini reaps orphans; Node doesn't), `RestartPolicy: no` (EventConsumer owns restarts — two authorities would hide crash loops), no binds, no mounts, no port bindings.

`ctx` bounds **only the two HTTP calls**. Unlike host-exec — where `exec.CommandContext` would SIGKILL the app the moment `Deploy()` returned — the container's lifetime belongs to the daemon.

### `HostExecRuntime.Start` (the other branch)

```go
port := spec.Port
if port == 0 { port, _ = allocatePort() }        // net.Listen("tcp","127.0.0.1:0"), read port, close
projSpec := nodedetect.GetProjectSpecs(path, port)
cmd := exec.Command(projSpec.StartCmd[0], projSpec.StartCmd[1:]...)
cmd.Dir = path
cmd.Env = append(mergeEnv(spec.Env), fmt.Sprintf("PORT=%d", port))
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
```

`exec.Command`, **never `exec.CommandContext`** — see the 12-line comment in that file. `Setpgid` makes the process a group leader so `Stop` can `kill(-pgid)` and catch grandchildren. `mergeEnv` is the allowlist (`PATH`, `HOME`, `LANG`, `TMPDIR`, proxies) — the control plane's Postgres credentials no longer reach the app.

Then `cmd.Start()`, register in the map, and `go r.watch(tp, stdout, stderr)` which scans both pipes into a ring buffer, `cmd.Wait()`s, and emits `RuntimeEventDied`.

```go
p.DeploymentRepo.SetContainerInfo(ctx, deployment.ID, string(handle))
```

`UPDATE deployments SET container_id = $2` — this is what lets a future `Stop` or the reconciler find the container again after a restart.

---

# STAGE 6 — Deploy: readiness and the flip

```go
p.streamLog(ctx, projectID, LogLine{Stream: "info", Text: "waiting for the app to start serving..."})
if err := p.Runtime.WaitReady(ctx, handle, p.Config.ReadyTimeout); err != nil {
	p.discard(ctx, handle)
	return p.fail(...)
}
```

### `DockerRuntime.WaitReady` → [endpoint.go:60](internal/infrastructure/dockerrun/endpoint.go#L60)

```go
ctx, cancel := context.WithTimeout(ctx, timeout)
ticker := time.NewTicker(250 * time.Millisecond)
var runningSince time.Time

for {
	state, err := r.inspectState(ctx, handle)
	if err != nil { return err }
	if !state.Running {
		return fmt.Errorf("container %s exited during startup with code %d", handle, state.ExitCode)
	}
	switch {
	case state.Health != nil:
		if state.Health.Status == container.Healthy { return nil }
	default:
		if runningSince.IsZero() { runningSince = time.Now() }
		if time.Since(runningSince) >= settlePeriod { return nil }
	}
	select {
	case <-ticker.C:
	case <-ctx.Done():
		return fmt.Errorf("container %s did not become ready within %s", handle, timeout)
	}
}
```

**The `!state.Running` check comes first**, which is what makes a dead app fail *fast* instead of burning the full 90-second timeout. Measured on the real daemon: **90ms** for a container that exits immediately.

Two readiness modes. A generated image has a `HEALTHCHECK`, so `State.Health.Status` transitions `starting → healthy` and this is a genuine probe. A BYO image without one gets the weaker guarantee: it stayed alive through a 2-second settle. That's honest about what it can prove, and it still catches the dominant failure — an app that exits on boot.

`HostExecRuntime.WaitReady` can do better because the port is on this host: it actually `net.Dial`s `localhost:<port>` every 200ms, and checks `tp.IsExited()` between attempts.

```go
upstream, err := p.Runtime.Endpoint(ctx, handle)
```

### `DockerRuntime.Endpoint` → [endpoint.go:30](internal/infrastructure/dockerrun/endpoint.go#L30)

One `ContainerInspect`, then a mode switch:

- **`EndpointIP`** (default) → `NetworkSettings.Networks["golaunch-edge"].IPAddress` + `:3000` → `172.18.0.2:3000`. **The host routes to the bridge subnet directly**, so Caddy running on the host dials this with nothing published. Verified live.
- **`EndpointDNS`** → `strings.TrimPrefix(inspected.Name, "/")` + `:3000` (the daemon reports names with a leading slash). For a Caddy that is itself a container on the network.

The mode exists because container names only resolve through Docker's embedded DNS at `127.0.0.11`, which is reachable **from inside a container** — a host process can't use it.

### `CaddyClient.RegisterRoute(slug, upstream)` → [caddy/client.go:97](internal/infrastructure/caddy/client.go#L97)

```go
hostname := fmt.Sprintf("%s.%s", slug, c.Domain)   // "myapp.localhost"
routes, err := c.getRoutes()                       // GET /config/apps/http/servers/srv0/routes
newRoute, err := buildReverseProxyRoute(hostname, dialTarget)

replaced := false
for i, r := range routes {
	if matchesHost(r, hostname) { routes[i] = newRoute; replaced = true; break }
}
if !replaced {
	routes = append([]route{newRoute}, routes...)   // PREPEND
}
return c.putRoutes(routes)                          // PATCH
```

Read-modify-write of the whole route array. Two details carry weight:

**Replacement in place is the atomic flip.** Overwriting the existing route for `myapp.localhost` is what makes the new version live — there is no separate "remove old route" step, and no instant where the hostname has no route.

**Prepend, not append.** The Caddyfile has a `*.localhost` catch-all that responds 404. Caddy evaluates routes in order and these are `Terminal: true`, so appending would put every new route *behind* the catch-all and every app would 404.

The handler JSON is built with `map[string]interface{}{"handler":"reverse_proxy","upstreams":[{"dial": "172.18.0.2:3000"}]}`, marshalled into `json.RawMessage` — Caddy's admin API is schemaless at this level, so raw JSON is the honest representation.

```go
p.ProjectRepo.SetCurrentDeployment(ctx, project.ID, deployment.ID)
p.ProjectRepo.UpdateStatus(ctx, project.ID, entities.StatusRunning)
p.streamLog(... "live at http://myapp.localhost")
```

---

# STAGE 7 — Deploy: tearing down the old

```go
if previous != nil {
	prevHandle := entities.RuntimeHandle(previous.ContainerID)
	p.Runtime.Stop(ctx, prevHandle, stopTimeoutSeconds)   // errors → warning log only
	p.Runtime.Remove(ctx, prevHandle)
	if previous.Status == entities.DeploymentRunning {
		p.DeploymentRepo.UpdateStatus(ctx, previous.ID, entities.DeploymentStopped)
	}
}
```

**This happens last, after traffic already moved.** That ordering is the whole zero-downtime story, and the function's doc comment says "Don't reorder it."

Failures here are warnings, not errors — the new version is already live and serving. A leaked old container is an operational annoyance; failing the deploy would be a lie.

The `previous.Status == DeploymentRunning` guard prevents clobbering history: if the old deployment had already crashed and `EventConsumer` marked it `crashed`, overwriting that with `stopped` would erase the evidence.

`Stop` → `POST /containers/{id}/stop?t=10` → SIGTERM, wait 10s, SIGKILL. The app receives SIGTERM because of the exec-form `CMD`. `Remove` → `DELETE /containers/{id}?v=1`, **not forced** — a 409 becomes `cannot remove running container, stop it first`, matching host-exec's wording exactly so both runtimes behave identically.

`Deploy` returns nil → the processor's defers fire → `close(logCh)` → the SSE handler's `range` ends → `event: done` → HTTP response completes.

---

# STAGE 8 — Serving traffic

```
browser → myapp.localhost:80
  → Caddy (host process)
      matches host "myapp.localhost", Terminal:true
      reverse_proxy dial 172.18.0.2:3000
  → host routes to the docker bridge (it IS the gateway)
  → container eth0:3000
  → node
```

No host port is published. The bridge subnet isn't routable from outside the machine, so the app is reachable **only** through Caddy. Caddy's admin API stays on `127.0.0.1:2019`, which no container can reach.

---

# STAGE 9 — The app crashes

The daemon emits `oom` (if applicable) then `die`.

### `DockerRuntime.Events` → `pumpEvents` → `streamEvents` → [events.go](internal/infrastructure/dockerrun/events.go)

```go
messages, errs := r.cli.Events(ctx, events.ListOptions{Since: since, Filters: eventFilters()})
```

Filters are daemon-side: `type=container`, `label=golaunch.managed=true`, `event=start|die|oom`. Without the managed label we'd receive `die` events from Postgres and log "death event with no deployment_id label" for every unrelated container on the box.

```go
case msg := <-messages:
	if msg.TimeNano > 0 { since = eventBookmark(msg.TimeNano) }
	evt, ok := translateEvent(msg)
	if !ok { continue }
	select {
	case out <- evt:
	case <-ctx.Done(): return since
	}
```

`eventBookmark` renders `"<sec>.<9-digit nsec>"` with `+1ns`. The format is not cosmetic — the daemon splits on `.` and reads the left half as **seconds**; a bare nanosecond count would put the cutoff ~55 billion years out and the stream would deliver nothing. The `+1` stops the message being replayed, which would double-count the crash.

`translateEvent` maps `Actor.ID` → handle (the same string `Start` returned) and `decodeLabels(Actor.Attributes)` → labels. `Attributes` mixes container labels with daemon keys (`name`, `image`, `exitCode`); the `golaunch.` prefix check strips them for free. So the consumer resolves an event to a deployment with **zero database queries**.

### `EventConsumer.handle` → [event_consumer.go:87](internal/application/event_consumer.go#L87)

```go
if evt.Type != Died && evt.Type != OOM { return }
deploymentID := evt.Labels["deployment_id"]
deployment, err := c.DeploymentRepo.GetByID(ctx, deploymentID)

if deployment.Status == entities.DeploymentStopped { return }   // ← intentional stop
```

**That one line is the entire distinction between a crash and a deliberate stop.** `StopProjectUseCase` marks the row `stopped` *before* touching the runtime, so when the death event arrives the consumer sees the marker and stays out of it. Ordering in two different files, coupled through the DB.

```go
status, err := c.Runtime.Status(ctx, evt.Handle)
if err == nil { exitCode = status.ExitCode }

reason := "process exited unexpectedly"
if evt.Type == entities.RuntimeEventOOM { reason = "out of memory" }

c.DeploymentRepo.SetFailed(ctx, deploymentID, reason, exitCode)
c.DeploymentRepo.UpdateStatus(ctx, deploymentID, entities.DeploymentCrashed)
c.Caddy.RemoveRoute(project.Slug)
c.ProjectRepo.UpdateStatus(ctx, project.ID, entities.StatusFailed)
c.maybeRestart(project.ID)
```

`Status` follows up with an inspect because the event carries no exit code. `statusFromState` checks `OOMKilled` **before** the exit code, because an OOM kill also sets `ExitCode: 137` — and the two demand opposite responses: a crash might be fixed by restarting, an OOM guarantees an identical death in a loop.

`RemoveRoute` filters the array in place with `out := routes[:0]` (reusing the backing array) and PATCHes. Traffic now gets the `*.localhost` catch-all 404 instead of a connection refused.

```go
func (c *EventConsumer) maybeRestart(projectID string) {
	c.mu.Lock(); c.crashCounts[projectID]++; count := c.crashCounts[projectID]; c.mu.Unlock()
	if count > c.MaxRestarts { log.Printf("... giving up"); return }
	time.AfterFunc(c.RestartDelay, func() {
		c.WorkerPool.Submit(queue.Job{ID: utils.NewID(), ProjectID: projectID})
	})
}
```

`time.AfterFunc` runs on its own timer goroutine so the 5-second delay never blocks the event loop. Cap is 3 consecutive crashes; `ResetCrashCount` in the processor clears the streak after any successful deploy, so a crash months later doesn't inherit an old count.

That resubmitted job re-enters at **Stage 2** with no registered log channel, so the restart is silent from the client's perspective.

---

# STAGE 10 — Control plane restarts

This is the case that only works because of the migration. Containers outlived the process.

### `Reconciler.Run(ctx)` → [reconcile.go:41](internal/application/reconcile.go#L41)

```go
dbRunning, _ := r.DeploymentRepo.ListByStatus(ctx, entities.DeploymentRunning)
instances, _ := r.Runtime.List(ctx, nil)       // ← nil filter
```

`List(nil)` → `ContainerList{All: true, Filters: labelFilters(nil)}`, and `labelFilters` seeds `golaunch.managed=true` in `NewArgs` **before** any loop, unconditionally. This is load-bearing: the loop below stops and removes everything it doesn't recognize. I verified this live — `postgres-dev` was running on this host and `List(nil)` returned exactly **1** container, ours. Without that filter the reconciler would have removed the user's database on first boot.

```go
byDeploymentID := map[string]entities.RuntimeInstance{}
for _, inst := range instances {
	if id := inst.Labels["deployment_id"]; id != "" { byDeploymentID[id] = inst }
}
```

Three-way settle:

**Both sides agree** → re-register the Caddy route. Caddy may have restarted even though the container didn't, so its config could be empty. `r.Runtime.Endpoint(ctx, inst.Handle)` re-resolves the address — which is why this survives the container getting a **different IP** after a daemon restart. (This is what replaced the old `port` label hack.)

**DB says running, runtime doesn't have it** → `r.Pipeline.Deploy(ctx, d.ProjectID)`, synchronously, in the reconciler goroutine.

**Runtime has it, DB doesn't** → orphan → `Stop` + `Remove`.

---

# STAGE 11 — Explicit stop

`POST /stop/{projectID}` → `StopProjectUseCase.Execute`:

```go
uc.DeploymentRepo.UpdateStatus(ctx, deployment.ID, entities.DeploymentStopped)  // ← FIRST
uc.Caddy.RemoveRoute(project.Slug)
uc.Runtime.Stop(ctx, handle, stopTimeoutSeconds)
uc.Runtime.Remove(ctx, handle)
uc.ProjectRepo.UpdateStatus(ctx, project.ID, entities.StatusStopped)
```

The DB write is first for exactly one reason: `Runtime.Stop` will produce a `die` event, `EventConsumer` will wake up, and it needs to already see `DeploymentStopped` or it will classify this as a crash and auto-redeploy the thing you just stopped.

---

# Findings

**1. The 5-minute worker timeout caps everything.** [workerpool.go:77](internal/queue/workerpool.go#L77) creates `context.WithTimeout(context.Background(), 5*time.Minute)` and passes it into `Deploy`. `dockerbuild.Build` then does `context.WithTimeout(ctx, 900s)` — and a child deadline **cannot outlive its parent**. So the `build_timeout_seconds: 900` I put in your config is unreachable: any build over 5 minutes dies with `context deadline exceeded`, not a build-timeout message. A cold `npm ci` on a large Next project routinely exceeds 5 minutes. The worker timeout should be derived from `BuildTimeout + ReadyTimeout + slack`, or removed in favour of the per-stage timeouts that now exist.

**2. `deployments.image_ref` is always empty.** `NewDeployment(project.ID, "")` is called *before* the build, because the tag needs the row's ID. The real ref comes back from `Build` and is never written — there's no repo method for it, and `SetContainerInfo` only stores the container ID. The column exists and every SELECT reads it. **This blocks Phase 7 rollback**, which needs to know which image a past deployment used.

**3. A failed deploy creates 4 deployment rows.** `Process` retries 3 times on error, and each retry runs `Deploy` from the top, which INSERTs a fresh row. Layered on top of `EventConsumer`'s 3 crash-restarts, one bad project can produce a lot of failure rows and a lot of build load.

**4. `ShutDown()` can panic.** It does `wp.cancel()` then `close(wp.jobs)`. `EventConsumer.maybeRestart` submits from a `time.AfterFunc` goroutine — a send on a closed channel is a panic, not an error. Only reachable on shutdown, and shutdown currently never runs, but it's a live edge.

**5. Retry logs go nowhere.** The processor closes and deletes the log channel on the first attempt, so retries 2–4 stream into a registry miss. The client saw `event: done` before the work actually finished.

Want me to fix the timeout conflict and persist the image ref? Those two are the ones that change behaviour you'd actually hit.