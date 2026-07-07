package caddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// CaddyClient talks to Caddy's admin API (localhost:2019 by default).
// Caddy's API is a JSON tree — you POST to a path in the config tree
// to add/update/delete routes. No restart needed, changes are instant.
type CaddyClient struct {
	AdminURL string // e.g. "http://localhost:2019"
	Domain   string // e.g. "golaunch.dev" — subdomains go under this
}

func NewCaddyClient(adminURL, domain string) *CaddyClient {
	return &CaddyClient{
		AdminURL: adminURL,
		Domain:   domain,
	}
}

// route is the JSON structure Caddy expects for one route entry.
// Match: which hostname to match.
// Handle: what to do — in our case, reverse proxy to the Node process.
type route struct {
	Match  []routeMatch  `json:"match"`
	Handle []routeHandle `json:"handle"`
}

type routeMatch struct {
	Host []string `json:"host"`
}

type routeHandle struct {
	Handler   string      `json:"handler"`
	Upstreams []upstream  `json:"upstreams"`
}

type upstream struct {
	Dial string `json:"dial"` // "localhost:3001"
}

// RegisterRoute tells Caddy to route {uniqueKey}.{domain} → localhost:{port}.
// This is called right after a project successfully starts.
func (c *CaddyClient) RegisterRoute(uniqueKey string, port int) error {
	hostname := fmt.Sprintf("%s.%s", uniqueKey, c.Domain)

	r := route{
		Match: []routeMatch{
			{Host: []string{hostname}},
		},
		Handle: []routeHandle{
			{
				Handler: "reverse_proxy",
				Upstreams: []upstream{
					{Dial: fmt.Sprintf("localhost:%d", port)},
				},
			},
		},
	}

	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("failed to marshal route: %w", err)
	}

	// POST to Caddy's config API.
	// This appends a new route to the routes array.
	// The path "..." means append to the array — Caddy's API syntax.
	url := fmt.Sprintf("%s/config/apps/http/servers/srv0/routes/...", c.AdminURL)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("caddy API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("caddy returned status %d", resp.StatusCode)
	}

	return nil
}

// RemoveRoute removes the route for a project.
// Called when a project is stopped or deleted.
// Caddy routes are identified by index in the array — we find it by hostname first.
func (c *CaddyClient) RemoveRoute(uniqueKey string) error {
	hostname := fmt.Sprintf("%s.%s", uniqueKey, c.Domain)

	// GET current routes
	url := fmt.Sprintf("%s/config/apps/http/servers/srv0/routes", c.AdminURL)
	
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to get routes: %w", err)
	}
	defer resp.Body.Close()

	var routes []route
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		return fmt.Errorf("failed to decode routes: %w", err)
	}

	// find the index of our route
	idx := -1
	for i, r := range routes {
		for _, m := range r.Match {
			for _, h := range m.Host {
				if h == hostname {
					idx = i
				}
			}
		}
	}

	if idx == -1 {
		return nil // route doesn't exist — nothing to do
	}

	// DELETE by index
	delURL := fmt.Sprintf("%s/config/apps/http/servers/srv0/routes/%d", c.AdminURL, idx)
	req, err := http.NewRequest(http.MethodDelete, delURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create delete request: %w", err)
	}

	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("caddy delete request failed: %w", err)
	}
	defer delResp.Body.Close()

	return nil
}