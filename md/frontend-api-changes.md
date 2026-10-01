# API changes for frontend — full reference

Everything the backend added/changed this round. Nothing here existed in
the frontend before, except where noted. This supersedes the earlier
`frontend-changes.md` — read this one.

All authed endpoints use an `HttpOnly` session cookie, never a bearer
token. Every `fetch`/`axios` call to any endpoint below needs
`credentials: "include"` — there is no token to attach manually. The
dev-server origin must be in the backend's CORS allowlist or every call
below fails silently in the browser before reaching a handler.

**Error shape, everywhere:** a non-2xx response is JSON:
```json
{ "error": "human-readable message" }
```
Not plain text — `res.json()` and read `.error`, not `res.text()`. A `500`
always returns a generic `"something went wrong, please try again"` — never
build UI expecting a specific 500 message; the specific ones only ever ride
on 4xx.

---

## 1. Auth

| Endpoint | Method | Body | Notes |
|---|---|---|---|
| `/auth/register` | POST | `{email, password}` | 201 + user object. 409 if email taken, 400 if password < 8 chars. |
| `/auth/login` | POST | `{email, password}` | 200 + user object, sets the session cookie. 401 on bad creds. |
| `/auth/logout` | POST | — | 204, clears the cookie. |
| `/auth/me` | GET | — | 200 + user object if session valid, else 401. **Call this once on app load** to restore session state after a refresh. |

User object: `{id, email, created_at}` (never a password hash).

---

## 2. Anonymous submit → signup → build

**The core flow this session built.** `/upload` and `/import/github` no
longer require login — get the file/repo in first, gate on signup before
the actual build.

| Endpoint | Method | Auth | Body |
|---|---|---|---|
| `/upload` | POST | optional | multipart, field `file`, `.zip` only, 20MB cap |
| `/import/github` | POST | optional | `{repo_url, ref}` — `ref` optional, `repo_url` must be `https://github.com/<owner>/<repo>` |

Both return `{"ProjectID": "...", "UniqueKey": "..."}` on success (200).
**Not wrapped in the new error envelope on success** — that shape is
unchanged from before this session.

**The flow:**
1. Call `/upload` or `/import/github` — no login required. Hold onto
   `ProjectID`.
2. If the caller has no session, show signup/login. If they already have a
   valid session (check `/auth/me` on boot, as above), skip straight to
   step 4 — the project's already owned, no claim needed.
3. On successful register/login, call `POST /projects/{ProjectID}/claim`
   (no body). Safe to call unconditionally even if the project was already
   owned (no-op, still 200) — simplest to just always call it rather than
   branching.
4. Call `POST /run/{ProjectID}` (unchanged, existing SSE build-log stream)
   to actually build and deploy.

**`POST /projects/{id}/claim`** — 200 + `ProjectSummary` (see §3) on
success. 404 if the project doesn't exist or is owned by someone else
(same not-found-not-forbidden pattern as everything else). 409 if raced by
another claim. **403 if the caller has hit the 2-project cap** — see §6.

Abandoned anonymous submissions (uploaded, never claimed) are deleted
server-side after ~1 hour. Don't build a "resume my upload" feature — once
the tab closes, `ProjectID` is gone and there's nothing to recover.

---

## 3. Projects list + detail

**New dashboard surface — nothing here existed before.**

**`GET /projects`** — every project the caller owns:
```json
[{
  "id": "...", "name": "...", "slug": "...",
  "status": "pending|building|running|stopped|failed",
  "source_type": "zip|git", "live_url": "http://slug.domain",
  "created_at": "...", "updated_at": "..."
}]
```
Empty array (not null) if the user owns nothing. This is the dashboard home
— land here after login/claim.

**`GET /projects/{id}`** — same fields, plus:
```json
{ ..., "deployment": { "status": "...", "failure_reason": "...", "exit_code": 1 } }
```
`deployment` key is absent if never deployed. `failure_reason`/`exit_code`
only present on a failed/crashed deployment. 404 if not owned.

---

## 4. Project actions (mostly pre-existing, listed for completeness)

| Endpoint | Method | Notes |
|---|---|---|
| `POST /run/{id}` | SSE | Build + deploy. Also **is** the redeploy call for git projects — hitting it again re-syncs from origin first, no separate "redeploy" endpoint exists. |
| `POST /stop/{id}` | — | Stops the live deployment. `{"status": "stopped"}` on success. |
| `POST /projects/{id}/slug` or `PATCH` | `{slug}` | Rename. 409 on collision, 400 empty slug. |
| `DELETE /projects/{id}` | — | **New.** Tears down the live deployment and deletes everything. 204. **No undo — confirm dialog strongly recommended.** |

---

## 5. Live logs — `GET /projects/{id}/logs` (new)

SSE, same event format as the `/run/{id}` build-log stream:
```
event: stdout|stderr|info
data: <line>
```
Difference from the build stream: this is *live* logs from whatever's
currently running, open-able any time (not just mid-deploy), and it never
sends a `done` event — it stays open and follows new output until the
client disconnects. Close the `EventSource` yourself on navigation away.

404s if the project's never been deployed or nothing's currently running —
show as "nothing to show yet," not an error state.

---

## 6. The 2-project cap (new)

`/upload`, `/import/github` (when already logged in), and
`/projects/{id}/claim` can now return **403**:
```json
{ "error": "you've reached the 2-project limit — delete a project to deploy a new one" }
```
This is the one place `DELETE /projects/{id}` isn't optional — surface
this specific message (not a generic error toast) so the user knows to go
delete something. It is **not** enforced on the anonymous `/upload`/
`/import/github` call itself (no identity to check yet at that point) —
only once a real account is attached, so it can surface as late as the
claim step after signup.

---

## 7. Env vars (new)

| Endpoint | Method | Body |
|---|---|---|
| `/projects/{id}/env` | GET | — |
| `/projects/{id}/env` | PUT or POST | `{"env": {"KEY": "value", ...}}` |

Both return `{"env": {"KEY": "value", ...}}`. **PUT replaces the entire
set** — it's not a per-key patch, so send the full map every time, not just
the keys that changed (read current values with GET first if you're
building an "edit one row" UI).

Key rules: letters/digits/underscore, can't start with a digit, max 50
vars, 4096-char value cap. `PORT` is rejected — it's platform-controlled.
Invalid input is 400 with a specific message; ownership failures are 404,
same as everywhere else.

**Changes don't take effect until the next deploy.** Setting env vars just
persists them — call `POST /run/{id}` afterward to actually redeploy with
the new values. There's no restart-without-rebuild yet, so right now that
means a full rebuild; worth telling the user "saved — redeploy to apply."

Values come back in plaintext on GET — there's no masking/secret handling
yet, by deliberate MVP choice (ownership is the only access control). Don't
build a "hidden until revealed" UI expecting the API to support it; if you
want that treatment client-side, you'd be hiding a value the API is happy
to hand back on every GET.

---

## 8. Rate limiting (new, no frontend work needed — just don't be surprised)

`/upload` and `/import/github` are IP-rate-limited (a handful of
submissions per 10 minutes) since they're unauthenticated now. A `429`
response there means slow down, not a bug — worth a distinct "too many
attempts, try again shortly" message if you want to handle it specially,
otherwise it'll fall through as a generic error.

---

## 9. Rollback (new)

**`GET /projects/{id}/deployments`** — full deployment history for a
project, newest first:
```json
[{
  "id": "...", "status": "running|stopped|failed|crashed|building",
  "is_current": true, "can_roll_back_to": true,
  "failure_reason": "...", "exit_code": 1,
  "created_at": "...", "started_at": "...", "stopped_at": "..."
}]
```
`failure_reason`/`exit_code` only present on a failed/crashed entry.
`can_roll_back_to` is `false` for a deployment that never finished
building — no image exists for it, don't show a rollback action on that
row at all (disabled, not just error-on-click). `is_current` marks the one
that's live right now — don't show a rollback action on that row either.

**`POST /projects/{id}/rollback`** — body `{"deployment_id": "..."}`. 200
+ `{"status": "rolled back"}` on success. This is a **plain synchronous
POST, not SSE** — no log stream like `/run`, just wait for the response
the same way you already do for `/stop` and the rename endpoint. It
usually finishes fast (no build step), but it does start a real container
and wait for it to become ready, so don't treat it as instant — show a
loading state.

Errors, same envelope as everywhere else:
| Status | Meaning |
|---|---|
| 404 | not owned / doesn't exist |
| 409 | that deployment is already the live one |
| 400 | that deployment never finished building, or its image has since been removed |
| 500 | rollback started but failed partway (new container never went ready, etc.) — the previously-live deployment is guaranteed untouched in this case, safe to just show the error and let them retry |

**Where this lives in the UI:** a "deployment history" section on the
project detail page — this is genuinely new surface, there's nothing to
extend. Each past deployment is a row with its status/timestamps and a
"Roll back to this" button (hidden/disabled per the two flags above).

**After a successful rollback**, the project's live deployment has
changed — re-fetch `GET /projects/{id}` (or optimistically update from the
deployment you just rolled back to) so the detail page's current-status
section reflects the new reality immediately, same as you'd do after
`/stop`.
