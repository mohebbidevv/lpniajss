# Frontend changes doc — anonymous submit → signup → build

Backend change this covers: `/upload` and `/import/github` no longer require
a session. They stage a project anonymously; a new `POST
/projects/{projectID}/claim` (authed) attaches it to a real account. Nothing
about `/run`, `/stop`, the slug endpoint, `/projects`, or `/projects/{id}`
changed — they're still authed exactly as before, and still 404 (not 403) on
a project that isn't yours.

## The new flow, end to end

1. User drops a zip or pastes a GitHub URL. Call `POST /upload` or `POST
   /import/github` — **no auth needed, don't gate this screen on login at
   all.** Response is unchanged: `{ "ProjectID": "...", "UniqueKey": "..." }`.
2. Hang onto `ProjectID` (query param, component state, wherever) and show
   the signup/login screen.
3. On successful register or login, call `POST /projects/{ProjectID}/claim`
   with no body. This is new. Response is a `ProjectSummary` (same shape as
   list/detail — see below).
4. Call `POST /run/{ProjectID}` as already implemented — this kicks off the
   actual build.

If the user was **already logged in** when they hit step 1 (has a valid
session cookie), the backend recognizes them automatically and the project
is created already-owned — skip straight from step 1 to step 4, no signup
screen, no claim call needed. But it's safe to call `claim` unconditionally
after auth resolves regardless of which case you're in — claiming a project
you already own is a no-op that returns 200, not an error. If you don't want
to special-case this in the frontend, just always call `claim` after
step 2/3 and always proceed to `/run` after that — simplest to build.

## `POST /projects/{projectID}/claim`

- Requires the session cookie (same as every other authed call).
- No request body.
- 200 + `ProjectSummary` on success (including a no-op re-claim by the
  owner).
- 404 if the project doesn't exist, **or if it's owned by someone else** —
  same "don't confirm existence" behavior as every other project endpoint.
- 409 if it was already claimed by a different account (a genuine race, not
  the common case).

```json
{
  "id": "...", "name": "...", "slug": "...", "status": "pending",
  "source_type": "zip", "live_url": "http://slug.domain",
  "created_at": "...", "updated_at": "..."
}
```

## What this means for screens you're building

- **Upload/import screen**: remove any auth-gate you had planned before this
  screen. It's the *first* thing a new visitor can do, before any account
  exists.
- **Signup/login screen**: needs to accept and carry forward a pending
  `ProjectID` (from step 1) through to a post-auth redirect that fires the
  claim call. If the existing auth screens were built assuming they're
  always the entry point, they need a "resume this project after auth" path
  now.
- **Already-logged-in case**: if a session cookie is already valid when the
  upload/import screen loads (check via `GET /auth/me` on app boot, as
  covered before), skip the signup screen for that submission entirely —
  don't force a logged-in user through a signup prompt for their own new
  project.
- **Claim failures**: a 404 on claim means the project ID is stale/invalid
  or already someone else's — treat it as "something went wrong, try
  uploading again," not a state worth deep error handling. A 409 is rare
  enough to show a generic error too.

## New this round: logs, delete, the 2-project cap, and error shape

Three more endpoints, plus one change that touches every existing call.

**`GET /projects/{id}/logs`** — SSE, same event format as the existing
build-log stream (`event: stdout|stderr|info`, `data: <line>`). This is
*live* logs from whatever's currently running, not a build log — open it
any time after a project has a successful deploy, not just during one.
404s if the project's never been deployed or nothing's currently running.
Unlike the build-log stream, there's no `event: done` — it stays open and
follows new output until the client disconnects, so close it yourself when
the user navigates away.

**`DELETE /projects/{id}`** — tears down the live deployment (if any) and
removes the project entirely. 204 on success, 404 if it's not yours.
Irreversible — no soft-delete, no undo. Worth a confirm dialog.

**The 2-project cap** — `/upload`, `/import/github` (when already logged
in), and `/projects/{id}/claim` can now all return **403** with an error
message like `"you've reached the 2-project limit — delete a project to
deploy a new one"`. This is the one place `DELETE` isn't optional polish —
without it, a user who hits the cap has no way to ever deploy a third
project. Surface this 403 specifically (not as a generic error) so the
message reaches the user, and point them at deleting an existing project.

**Every error response is now `{"error": "message"}` JSON**, not plain
text. If any existing fetch wrapper was doing `res.text()` on a non-2xx
response, switch it to `res.json()` and read `.error`. A `500` is now
always a generic "something went wrong" — don't build UI that tries to
show raw backend error text for those; the specific, useful messages
(validation, ownership, conflict, the cap above) are still exact strings
on 4xx responses.

## One thing to flag back, not build around

Anonymous submissions that never get claimed (someone uploads, then abandons
the signup screen) sit in the database unowned indefinitely — there's no
cleanup job yet. Not a frontend concern, just don't build any UI that assumes
an unclaimed project reappears somewhere later (a "resume your upload"
feature, a returning-anonymous-user flow) — it isn't retrievable once the
tab closes, since there's nowhere anonymous state is looked up from except
the `ProjectID` the frontend was holding in memory.
