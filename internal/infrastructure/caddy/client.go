package caddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// adminTimeout bounds every admin-API call. http.DefaultClient has no
// timeout at all, so a wedged Caddy admin socket would hang whichever deploy
// worker was flipping a route — and a hung worker is 25% of build capacity.
const adminTimeout = 10 * time.Second

type CaddyClient struct {
	AdminURL string
	Domain   string

	http *http.Client

	// mu guards only the create path, which is the one operation that
	// cannot be expressed as a single addressed write (it has to decide
	// between "insert" and "replace"). Every other method here is one
	// atomic server-side call and needs no lock.
	mu sync.Mutex
}

func NewCaddyClient(adminURL, domain string) *CaddyClient {
	return &CaddyClient{
		AdminURL: adminURL,
		Domain:   domain,
		http:     &http.Client{Timeout: adminTimeout},
	}
}

// route carries an @id so each one is individually addressable at
// /id/<routeID>. That is what makes register and remove single atomic
// operations instead of a read-modify-write of the entire routes array,
// where two concurrent deploys silently overwrite each other's route.
type route struct {
	ID       string            `json:"@id,omitempty"`
	Match    []routeMatch      `json:"match"`
	Handle   []json.RawMessage `json:"handle"`
	Terminal bool              `json:"terminal,omitempty"`
}

type routeMatch struct {
	Host []string `json:"host"`
}

// routeID is derived from the immutable project ID, never the slug. Keying
// on a mutable slug would make a rename a remove-then-add — two writes with
// a window where neither hostname resolves. Keyed on the ID, a rename is a
// single in-place update of the host matcher.
func routeID(projectID string) string {
	return "golaunch-project-" + projectID
}

func (c *CaddyClient) routesURL() string {
	return fmt.Sprintf("%s/config/apps/http/servers/srv0/routes", c.AdminURL)
}

func (c *CaddyClient) idURL(id string, subpath ...string) string {
	url := fmt.Sprintf("%s/id/%s", c.AdminURL, id)
	for _, s := range subpath {
		url += "/" + s
	}
	return url
}

// do issues one admin-API call. It reports success separately from the
// error so callers can branch on "this @id does not exist yet" without
// depending on which status code a given Caddy version uses for that — the
// admin API is inconsistent about 404 vs 500 there, so nothing in this file
// parses a status code or an error string to make that decision.
func (c *CaddyClient) do(method, url string, body []byte) (ok bool, err error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return false, fmt.Errorf("build caddy request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("caddy request failed: %w", err)
	}
	defer resp.Body.Close()

	// Always drain: an undrained body prevents the connection from being
	// reused and forces a fresh dial on every call.
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("caddy returned %d for %s %s: %s", resp.StatusCode, method, url, respBody)
	}
	return true, nil
}

func buildRoute(id, hostname, dialTarget string) (route, error) {
	handler := map[string]any{
		"handler":   "reverse_proxy",
		"upstreams": []map[string]string{{"dial": dialTarget}},
	}
	handlerBytes, err := json.Marshal(handler)
	if err != nil {
		return route{}, fmt.Errorf("marshal handler: %w", err)
	}
	return route{
		ID:       id,
		Match:    []routeMatch{{Host: []string{hostname}}},
		Handle:   []json.RawMessage{handlerBytes},
		Terminal: true,
	}, nil
}

// RegisterRoute points {slug}.{domain} at dialTarget — "localhost:3001" for
// host-exec, "172.18.0.4:3000" for a container. The caller decides the
// target; this client doesn't know or care which runtime produced it.
//
// The common case (a redeploy or rollback of a project that already has a
// route) is a single PATCH against that project's @id: one atomic
// server-side replace, no read, and a payload the size of one route rather
// than the whole array.
func (c *CaddyClient) RegisterRoute(projectID, slug, dialTarget string) error {
	id := routeID(projectID)
	hostname := fmt.Sprintf("%s.%s", slug, c.Domain)

	r, err := buildRoute(id, hostname, dialTarget)
	if err != nil {
		return err
	}
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal route: %w", err)
	}

	// PATCH replaces an existing value and fails when the path is unknown,
	// which is exactly the signal we want: success means the route was
	// already there and has been swapped atomically.
	if ok, _ := c.do(http.MethodPatch, c.idURL(id), body); ok {
		return nil
	}
	return c.createRoute(id, body)
}

// createRoute handles a project's first ever route. It is the only path that
// has to decide between inserting and replacing, so it is the only path that
// takes the lock — and it re-checks under that lock, because two deploys of
// the same project can race here and a lost check would append a duplicate
// route for one hostname.
func (c *CaddyClient) createRoute(id string, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Another goroutine may have created it between the PATCH above and
	// this lock. If so, replace rather than insert.
	if exists, _ := c.do(http.MethodGet, c.idURL(id), nil); exists {
		_, err := c.do(http.MethodPatch, c.idURL(id), body)
		return err
	}

	// PUT at an array index inserts at that position rather than replacing
	// the array, so this cannot clobber a concurrent writer the way the old
	// whole-array PATCH did. Index 0 keeps project routes ahead of any
	// static catch-all.
	if _, err := c.do(http.MethodPut, c.routesURL()+"/0", body); err != nil {
		return fmt.Errorf("insert route: %w", err)
	}
	return nil
}

// RetargetRoute moves a project's existing route to a new hostname, leaving
// its upstream untouched — the container behind it hasn't changed, only
// which hostname reaches it.
//
// It addresses the matcher directly (/id/<routeID>/match), so it neither
// reads nor rewrites the route's handler. One atomic write of one field.
// A missing route is not an error: only a live project has a route to move,
// and the reconciler re-derives it from the DB row if it is absent.
func (c *CaddyClient) RetargetRoute(projectID, newSlug string) error {
	hostname := fmt.Sprintf("%s.%s", newSlug, c.Domain)

	body, err := json.Marshal([]routeMatch{{Host: []string{hostname}}})
	if err != nil {
		return fmt.Errorf("marshal match: %w", err)
	}

	id := routeID(projectID)
	if ok, patchErr := c.do(http.MethodPatch, c.idURL(id, "match"), body); !ok {
		if exists, _ := c.do(http.MethodGet, c.idURL(id), nil); !exists {
			return nil
		}
		return patchErr
	}
	return nil
}

// RemoveRoute deletes exactly this project's route. A route that is already
// absent is success, not an error: stop, delete and crash-cleanup all call
// this on paths where the route may legitimately be gone already.
func (c *CaddyClient) RemoveRoute(projectID string) error {
	id := routeID(projectID)

	ok, delErr := c.do(http.MethodDelete, c.idURL(id), nil)
	if ok {
		return nil
	}
	if exists, _ := c.do(http.MethodGet, c.idURL(id), nil); !exists {
		return nil
	}
	return delErr
}

// RouteExists reports whether Caddy currently holds a route for this
// project, so the reconciler can skip a rewrite that would change nothing.
func (c *CaddyClient) RouteExists(projectID string) bool {
	ok, _ := c.do(http.MethodGet, c.idURL(routeID(projectID)), nil)
	return ok
}
