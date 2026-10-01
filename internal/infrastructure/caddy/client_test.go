package caddy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeCaddy models the subset of the admin API this client uses: an ordered
// routes array plus @id addressing over it. Handlers are deliberately
// serialized by one mutex, exactly like the real single-threaded config
// mutation — so any lost route in a test is the client's fault, not the
// fake's.
type fakeCaddy struct {
	mu     sync.Mutex
	routes []map[string]any

	patches int
	inserts int
}

func (f *fakeCaddy) indexOf(id string) int {
	for i, r := range f.routes {
		if r["@id"] == id {
			return i
		}
	}
	return -1
}

func (f *fakeCaddy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	body, _ := io.ReadAll(r.Body)

	switch {
	case strings.HasPrefix(r.URL.Path, "/id/"):
		rest := strings.TrimPrefix(r.URL.Path, "/id/")
		id, sub, _ := strings.Cut(rest, "/")

		i := f.indexOf(id)
		if i < 0 {
			// The real admin API is inconsistent here across versions
			// (404 vs 500); the client must not depend on which.
			http.Error(w, `{"error":"unknown object ID"}`, http.StatusInternalServerError)
			return
		}

		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(f.routes[i])
		case http.MethodPatch:
			if sub == "match" {
				var m []any
				json.Unmarshal(body, &m)
				f.routes[i]["match"] = m
			} else {
				var obj map[string]any
				json.Unmarshal(body, &obj)
				f.routes[i] = obj
				f.patches++
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			f.routes = append(f.routes[:i], f.routes[i+1:]...)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
		}

	case strings.HasSuffix(r.URL.Path, "/routes/0") && r.Method == http.MethodPut:
		var obj map[string]any
		json.Unmarshal(body, &obj)
		f.routes = append([]map[string]any{obj}, f.routes...)
		f.inserts++
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeCaddy) hosts() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := map[string]string{}
	for _, r := range f.routes {
		match, _ := r["match"].([]any)
		if len(match) == 0 {
			continue
		}
		m, _ := match[0].(map[string]any)
		hosts, _ := m["host"].([]any)
		if len(hosts) == 0 {
			continue
		}
		host, _ := hosts[0].(string)

		handle, _ := r["handle"].([]any)
		dial := ""
		if len(handle) > 0 {
			h, _ := handle[0].(map[string]any)
			ups, _ := h["upstreams"].([]any)
			if len(ups) > 0 {
				u, _ := ups[0].(map[string]any)
				dial, _ = u["dial"].(string)
			}
		}
		out[host] = dial
	}
	return out
}

func newTestClient(t *testing.T) (*CaddyClient, *fakeCaddy) {
	t.Helper()
	fake := &fakeCaddy{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return NewCaddyClient(srv.URL, "example.com"), fake
}

// TestRegisterInsertsThenPatches pins the two-path behaviour: a project's
// first route is inserted, and every subsequent deploy replaces it in place
// via its @id rather than rewriting the whole array.
func TestRegisterInsertsThenPatches(t *testing.T) {
	c, fake := newTestClient(t)

	if err := c.RegisterRoute("proj1", "myapp", "172.18.0.2:3000"); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if fake.inserts != 1 || fake.patches != 0 {
		t.Fatalf("first register: inserts=%d patches=%d, want 1/0", fake.inserts, fake.patches)
	}

	if err := c.RegisterRoute("proj1", "myapp", "172.18.0.9:3000"); err != nil {
		t.Fatalf("redeploy: %v", err)
	}
	if fake.inserts != 1 || fake.patches != 1 {
		t.Fatalf("redeploy: inserts=%d patches=%d, want 1/1", fake.inserts, fake.patches)
	}

	got := fake.hosts()
	if len(got) != 1 {
		t.Fatalf("expected exactly one route, got %d: %v", len(got), got)
	}
	if got["myapp.example.com"] != "172.18.0.9:3000" {
		t.Errorf("upstream = %q, want the redeployed address", got["myapp.example.com"])
	}
}

// TestConcurrentRegistersAllSurvive is the regression test for the
// read-modify-write race. Under the old whole-array GET/mutate/PATCH, two
// deploys finishing together would silently drop one route: the project
// reported "live" and 502'd forever with nothing logged.
func TestConcurrentRegistersAllSurvive(t *testing.T) {
	c, fake := newTestClient(t)

	const n = 25
	var wg sync.WaitGroup
	errs := make(chan error, n)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("proj%02d", i)
			if err := c.RegisterRoute(id, id, fmt.Sprintf("10.0.0.%d:3000", i+1)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent register: %v", err)
	}

	got := fake.hosts()
	if len(got) != n {
		t.Fatalf("only %d of %d routes survived concurrent registration", len(got), n)
	}
	for i := 0; i < n; i++ {
		host := fmt.Sprintf("proj%02d.example.com", i)
		want := fmt.Sprintf("10.0.0.%d:3000", i+1)
		if got[host] != want {
			t.Errorf("%s -> %q, want %q", host, got[host], want)
		}
	}
}

// TestConcurrentFirstRegisterSameProject covers two deploys of the SAME
// project racing on the create path — the one place the client still has to
// choose between insert and replace. Exactly one route must exist after.
func TestConcurrentFirstRegisterSameProject(t *testing.T) {
	c, fake := newTestClient(t)

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			_ = c.RegisterRoute("same", "same", fmt.Sprintf("10.0.0.%d:3000", i+1))
		}(i)
	}
	wg.Wait()

	if got := fake.hosts(); len(got) != 1 {
		t.Fatalf("expected 1 route after racing first-registers, got %d: %v", len(got), got)
	}
}

// TestRetargetKeepsUpstream is the rename path: the hostname moves, the
// container behind it does not.
func TestRetargetKeepsUpstream(t *testing.T) {
	c, fake := newTestClient(t)

	if err := c.RegisterRoute("proj1", "oldname", "172.18.0.2:3000"); err != nil {
		t.Fatal(err)
	}
	if err := c.RetargetRoute("proj1", "newname"); err != nil {
		t.Fatalf("retarget: %v", err)
	}

	got := fake.hosts()
	if len(got) != 1 {
		t.Fatalf("expected 1 route, got %d: %v", len(got), got)
	}
	if dial, ok := got["newname.example.com"]; !ok || dial != "172.18.0.2:3000" {
		t.Errorf("after rename got %v, want newname.example.com -> 172.18.0.2:3000", got)
	}
}

// TestRetargetMissingRouteIsNotAnError: only a live project has a route to
// move, and the reconciler re-derives it from the DB row otherwise.
func TestRetargetMissingRouteIsNotAnError(t *testing.T) {
	c, _ := newTestClient(t)
	if err := c.RetargetRoute("ghost", "whatever"); err != nil {
		t.Errorf("retarget of a missing route = %v, want nil", err)
	}
}

// TestRemoveIsIdempotent: stop, delete and crash-cleanup all call
// RemoveRoute on paths where the route may legitimately be gone already.
func TestRemoveIsIdempotent(t *testing.T) {
	c, fake := newTestClient(t)

	if err := c.RegisterRoute("proj1", "myapp", "172.18.0.2:3000"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveRoute("proj1"); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if err := c.RemoveRoute("proj1"); err != nil {
		t.Errorf("second remove = %v, want nil", err)
	}
	if got := fake.hosts(); len(got) != 0 {
		t.Errorf("routes remain after removal: %v", got)
	}
}

func TestRouteExists(t *testing.T) {
	c, _ := newTestClient(t)

	if c.RouteExists("proj1") {
		t.Error("RouteExists = true before any registration")
	}
	if err := c.RegisterRoute("proj1", "myapp", "172.18.0.2:3000"); err != nil {
		t.Fatal(err)
	}
	if !c.RouteExists("proj1") {
		t.Error("RouteExists = false after registration")
	}
}
