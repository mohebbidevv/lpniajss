# Per-Project Network Isolation — Implementation Plan

Mechanism-level plan for moving every tenant container off the single shared
`golaunch-edge` bridge. Written against the code on `dockermigration`.

---

## 0. Read this before anything else: the cheap fix is 90% of the win

`internal/infrastructure/dockerrun/network.go:16-21` currently says:

> ICC stays enabled, and that is a constraint rather than a preference.
> Docker's enable_icc=false installs a blanket DROP for traffic forwarded
> between containers on the bridge, which blocks the proxy from reaching an
> app just as surely as it blocks one app reaching another.

**That comment is wrong, and I wrote it.** It is true only for a proxy that
is itself a container on the same bridge. Caddy runs on the **host**
(`endpoint_mode: "ip"` is the default, `config.go` `EndpointIP`), and that
changes the packet path entirely:

- `enable_icc=false` installs, in the `FORWARD` chain, the equivalent of
  `-i br-X -o br-X -j DROP`. It matches only traffic that both enters *and*
  leaves the same bridge — i.e. container-to-container.
- Host-originated traffic never traverses `FORWARD`. A packet the host
  generates goes `OUTPUT` → `POSTROUTING` and is delivered into the bridge.
  The ICC rule cannot match it.
- Container egress is `-i br-X -o eth0` — different out-interface, so it does
  not match either.
- Docker's embedded DNS lives at 127.0.0.11 inside each container's own
  network namespace and is serviced by the daemon, not across the bridge.

So on the current single-network setup, adding
`com.docker.network.bridge.enable_icc=false` to the `NetworkCreate` options
in `EnsureNetwork` **blocks tenant-to-tenant traffic today**, leaves Caddy
working, and is a one-line change with no scaling consequences.

**Do that first.** It is a couple of minutes of work and closes the actual
hole. Everything below is the structural version, which is worth doing — but
mainly for a reason other than the one you asked about (see §7).

### The sysctl that makes ICC silently a no-op

Bridged frames only traverse iptables when `net.bridge.bridge-nf-call-iptables`
is 1. If it is 0 on the host, `enable_icc=false` is accepted, appears in
`docker network inspect`, and **does nothing at all**. Docker normally sets it
via the `br_netfilter` module, but a hardened or minimal host may not have the
module loaded.

Verify with `sysctl net.bridge.bridge-nf-call-iptables` and confirm
`lsmod | grep br_netfilter`. Do not trust the flag without checking; this is a
security control that fails open and fails quietly.

---

## 1. Design decision: per **project**, not per deployment

You asked for "each in one network". The question that decides the shape is
what the isolation boundary actually is, and it is the **tenant**, not the
deployment.

| | Per-deployment | Per-project |
|---|---|---|
| Networks live at once | One per deploy, and **two per project during a route flip** | One per project |
| Churn | Create + destroy on every single deploy | Created once, reused |
| What it isolates | One deployment from another deployment of the same project | One tenant from another |
| Address-pool pressure | Roughly 2× projects | Exactly projects |

Isolating a project's own new deployment from its own old one buys nothing —
they are the same tenant, the same code, the same owner. It costs a network
create and destroy on the deploy critical path, and it doubles the count at
exactly the worst moment.

**Name them `golaunch-proj-<projectID>`.** Project ID is immutable, so unlike
the slug it cannot desynchronise from the network name on rename — the same
reasoning that made the Caddy `@id` keying correct.

---

## 2. The hard wall: 31 networks, and it is closer than you think

This is the constraint that decides whether this design is viable at all.

Docker allocates every user-defined bridge network a subnet from its
predefined pools. In your pinned version those are in
`libnetwork/ipamutils/utils.go` of `docker/docker@v28.3.3`:

| Base | Split into | Networks |
|---|---|---|
| 172.17.0.0/16 | /16 | 1 |
| 172.18.0.0/16 | /16 | 1 |
| 172.19.0.0/16 | /16 | 1 |
| 172.20.0.0/14 | /16 | 4 |
| 172.24.0.0/14 | /16 | 4 |
| 172.28.0.0/14 | /16 | 4 |
| 192.168.0.0/16 | /20 | 16 |
| | **total** | **31** |

`docker0` consumes 172.17.0.0/16, so you get **30 usable user networks**.

At the 31st project, `NetworkCreate` fails with *"could not find an available,
non-overlapping IPv4 address pool among the defaults to assign to the
network"*, and **every subsequent deploy for a new project fails permanently**
until an operator intervenes. There is no retry, no degradation — a hard wall
at a number small enough to hit in the first month.

### The fix, which is host configuration, not code

Set `default-address-pools` in `/etc/docker/daemon.json`. A base of
`10.100.0.0/14` split at `/24` yields 1024 networks of 254 usable addresses
each — far more than one container per project needs.

Three things to get right:

1. **It only affects networks created afterwards.** Existing networks keep
   their subnets, so do this before you have projects, or plan a rebuild.
2. **It requires a daemon restart**, which stops every running container.
   Sequence it with the Caddy route flush from the `@id` migration — both
   want the same maintenance window.
3. **Do not overlap anything real.** `10.0.0.0/8` is the tempting choice and
   the wrong one: it collides with most VPC and VPN ranges, and the symptom
   is that the host loses the ability to reach some external service, days
   later, for no visible reason.

### The softer limits past that

- Each network is a Linux bridge interface (`br-<12 hex>`), so `ip link` grows
  linearly. Thousands is fine; it is not free.
- Docker maintains `DOCKER-ISOLATION-STAGE-1` and `-STAGE-2` chains with one
  rule per network each — O(n), not O(n²), but netfilter traverses them
  linearly per packet, so several thousand networks is measurable on the
  packet path.
- `NetworkCreate` costs roughly 50–200ms and partially serialises in the
  daemon. It lands on the first deploy of each project, once.

Practical ceiling with a corrected pool: low thousands of projects on one
host. Well past the point where a second host is the real answer.

---

## 3. Predictable bridge names — the detail that makes §7 possible

By default the kernel-visible interface is `br-` plus the first 12 characters
of the network ID: unpredictable, and different on every recreate. That makes
firewall rules impossible to write statically.

Pass `com.docker.network.bridge.name` in the network's `Options` to choose the
name yourself. Then **one** iptables rule with an interface wildcard (`-i gl+`
matches every interface starting `gl`) governs every tenant bridge, instead of
one rule per project that has to be added and removed in lockstep with network
lifecycle.

Constraint: `IFNAMSIZ` caps interface names at 15 characters including the
terminator, so you get 14 usable. `gl-` plus 11 characters of the project ID
fits. The name must be unique per network; derive it deterministically from
the project ID and it is.

This is the single most valuable line in the whole change, and it is easy to
miss because nothing breaks without it — you only discover the problem when
you try to write the egress rules and find you cannot name the interfaces.

---

## 4. Code changes, file by file

### 4.1 `dockerrun/network.go` — `EnsureNetwork` becomes per-project

Today it is called once at boot from `cmd/wire.go:42` with the single
configured name. It becomes a method the runtime calls on the deploy path,
taking a project ID.

It should keep its current shape: inspect first, create on not-found, and
treat `errdefs.IsConflict` as success — two concurrent deploys of the same
project will race here, and the loser must not fail. That logic already
exists at `network.go:27-40` and is correct; it only needs the name derived
from the project rather than passed in.

Add to the create options: `enable_icc=false`, the predictable bridge name
from §3, and the existing managed label. Keep the label — the GC in §5
depends on it to tell your networks from the host's.

### 4.2 `dockerrun/spec.go` — network per container

`spec.go:59` sets `NetworkMode` from `r.cfg.Network`, and `spec.go:102-105`
puts a single `EndpointsConfig` entry under the same name. Both derive from
`spec.ProjectID` instead; `entities.RuntimeSpec` already carries it, so no
plumbing changes.

### 4.3 `dockerrun/runtime.go` — ensure before create

`Start` must call the per-project ensure before `ContainerCreate`, because a
create against a missing network fails. It already handles a name conflict by
reclaiming a stale container (`runtime.go:44-51`); the network ensure goes
above that, since the reclaim path also needs the network to exist.

### 4.4 `dockerrun/endpoint.go` — the one that will bite you

`networkIP` at `endpoint.go:59` looks up `NetworkSettings.Networks[r.cfg.Network]`
— a single hardcoded name. With per-project networks that key is wrong for
every container, and `Endpoint` fails for all of them.

`Endpoint` only receives a handle, not a spec, so it cannot compute the name
from a project ID it does not have. Two ways out, and the second is better:

- Thread the project ID through the `Runtime.Endpoint` signature. Invasive:
  it changes the port, and three call sites (`deploy_pipeline.go:200`,
  `rollback_deployment.go:~140`, `reconcile.go:~85`) plus every fake.
- **Read it back off the container.** The inspect result already carries the
  container's own labels, and `spec.go` writes `project_id` into them
  (`deploy_pipeline.go:136-140` sets it). Resolve the network name from the
  container's own label — self-describing, no signature change, and correct
  by construction for a container this platform created.

Add a fallback for the migration window: if the computed network is absent
from the map but exactly one non-`bridge` network is attached, use that. It
carries pre-migration containers on `golaunch-edge` through until they are
redeployed, and costs a few lines.

### 4.5 `dockerrun/config.go` — `Network` changes meaning

`Config.Network` stops being *the* network and becomes a name **prefix**, or
is retained only as the legacy/shared name for the fallback above. Update the
`DefaultNetwork` comment at `config.go:8-11`, which currently describes Caddy
as sitting on the shared network — it does not; it is on the host.

### 4.6 `cmd/wire.go` — drop the boot-time ensure

`wire.go:42` creating one network at boot goes away. Nothing needs a network
to exist before the first deploy any more.

---

## 5. Lifecycle: the part that leaks if you skip it

A network is not removable while a container is attached, which dictates the
ordering everywhere.

**On project delete** (`application/delete_project.go`): remove the network
*after* `Runtime.Remove` of the container, not before. Today the use case
stops the container, removes it, removes the image, and deletes the source
tree; the network removal belongs at the end of that sequence. A failure here
should warn rather than fail the delete — the sweep below will catch it.

**On deploy failure**: `DeployPipeline.discard` tears down a container that
never went live. Deliberately leave the network alone — the project will
almost certainly be redeployed, and destroying it means recreating it 30
seconds later.

**Periodic sweep**: networks leak from crashes, from a delete that failed
partway, and from projects removed before this existed. Add a pass alongside
the existing image GC (`application/image_gc.go` already has the `Run`
ticker shape) that lists networks with the managed label, and removes any
whose name maps to a project that no longer exists **and** that has no
attached containers. Both conditions, not either: a network with containers
attached will refuse removal anyway, and a network for a live project must
never be touched.

Never use `docker network prune`. It is scoped by "no containers attached",
not by your label, and it will happily delete a project's network during the
window between a container being removed and its replacement starting — which
is exactly what a redeploy is.

---

## 6. What breaks, and the migration

**Existing containers are on `golaunch-edge`.** The `Endpoint` fallback in
§4.4 keeps them routable; they migrate to their own network on next deploy.
The reconciler will not force this — it only redeploys deployments the runtime
has lost.

**`Runtime.List`** is label-filtered (`labels.go`), not network-scoped, so it
is unaffected. That is why the reconciler keeps working mid-migration.

**`hostexec`** has no networks and needs no changes.

**Rollback** goes through `Runtime.Start`, so it inherits everything without
its own changes.

**Order of operations for the maintenance window**, since two other pending
changes want the same restart:

1. Edit `daemon.json` for `default-address-pools` (§2).
2. Restart the Docker daemon — this stops all containers.
3. Flush the Caddy routes array for the `@id` migration (still outstanding).
4. Start the new control-plane binary; the reconciler rebuilds routes and
   redeploys.

Doing these separately means three outages instead of one.

---

## 7. The actual reason to do this: egress control

Tenant-to-tenant isolation is the thing you asked about, and §0 gets you that
for one line. The structural reason to go further is the thing that is not in
any of the plan docs and is the most likely way this platform gets taken down:

**nothing currently limits what a deployed container can send outbound.** A
user can deploy something that sends spam, port-scans the internet, joins a
botnet, or hammers someone else's API. Your CPU and memory limits do not touch
it. The realistic consequence is not a security incident report — it is your
hosting provider null-routing the IP with no warning.

On a single shared bridge you cannot express per-tenant egress policy at all:
every container shares one interface and one subnet, so there is nothing to
match on. With per-project bridges and the predictable names from §3, you get
handles:

- A default-deny egress policy on `-i gl+` with explicit allowances for DNS,
  HTTP/HTTPS to the internet, and nothing else — which alone stops SMTP abuse
  and most scanning.
- **Block the link-local metadata address 169.254.169.254**, which on any
  cloud host hands out instance credentials to whoever asks. This is the
  single highest-value egress rule and it is trivial to forget.
- Block the host's own management ranges, so a tenant cannot reach your
  Postgres or the Docker admin socket's host port.
- Per-bridge traffic accounting and rate limiting via `tc`, when you need it.

Sequence it as: §0 now, then this plan, then egress rules on top. The egress
work is a separate piece and does not belong in this one — but it is why this
one is worth doing.

---

## 8. How to verify it

Isolation is a property that fails silently, so test it directly rather than
inferring it from configuration:

- **Tenant-to-tenant is blocked.** Deploy two projects, get container A's IP
  from `docker inspect`, and from inside container B attempt a TCP connection
  to it on the app port. Expect a timeout. Run this *before* the change too,
  and watch it succeed — otherwise you have not proven the test is meaningful.
- **Caddy still reaches apps.** Both projects serve over their hostnames. This
  is the assertion that catches the ICC mistake from §0 if the reasoning there
  is wrong for your host.
- **The ICC rule actually exists**: `iptables -S FORWARD | grep DROP` should
  show a rule per managed bridge, and the sysctl from §0 must be 1.
- **Address pool arithmetic**: create 31 networks by hand on a scratch host
  with default settings and watch the 31st fail. It is worth seeing the error
  once so you recognise it in production.
- **Metadata endpoint** (after egress work): from inside a container,
  `169.254.169.254` must be unreachable.
- **Network GC**: delete a project, confirm the network is gone; kill the
  process mid-delete, confirm the sweep collects it.

---

## 9. Suggested order

1. **§0 ICC on the shared network** — minutes, closes the hole, no scaling risk.
   Verify the sysctl.
2. **§2 daemon address pools** — before any per-project network exists, or you
   will be rebuilding them.
3. **§4 the code change**, with the `Endpoint` label-based resolution and its
   migration fallback.
4. **§5 lifecycle and sweep** — same change set; leaking networks walks you
   into the §2 wall from the other direction.
5. **Egress rules (§7)** — separate piece, and the real payoff.

Steps 1 and 2 are worth doing this week regardless of when the rest lands.
