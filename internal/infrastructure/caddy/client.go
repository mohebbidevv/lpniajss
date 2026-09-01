package caddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type CaddyClient struct {
	AdminURL string
	Domain   string
}

func NewCaddyClient(adminURL, domain string) *CaddyClient {
	return &CaddyClient{AdminURL: adminURL, Domain: domain}
}

type route struct {
	Match    []routeMatch      `json:"match"`
	Handle   []json.RawMessage `json:"handle"`
	Terminal bool              `json:"terminal,omitempty"`
}

type routeMatch struct {
	Host []string `json:"host"`
}

func (c *CaddyClient) routesURL() string {
	return fmt.Sprintf("%s/config/apps/http/servers/srv0/routes", c.AdminURL)
}

func (c *CaddyClient) getRoutes() ([]route, error) {
	resp, err := http.Get(c.routesURL())
	if err != nil {
		return nil, fmt.Errorf("get routes: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("caddy returned %d: %s", resp.StatusCode, body)
	}

	var routes []route
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		return nil, fmt.Errorf("decode routes: %w", err)
	}
	return routes, nil
}

func (c *CaddyClient) putRoutes(routes []route) error {
	body, err := json.Marshal(routes)
	if err != nil {
		return fmt.Errorf("marshal routes: %w", err)
	}

	req, err := http.NewRequest(http.MethodPatch, c.routesURL(), bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("caddy request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("caddy returned %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

func buildReverseProxyRoute(hostname, dialTarget string) (route, error) {
	handler := map[string]interface{}{
		"handler":   "reverse_proxy",
		"upstreams": []map[string]string{{"dial": dialTarget}},
	}
	handlerBytes, err := json.Marshal(handler)
	if err != nil {
		return route{}, fmt.Errorf("marshal handler: %w", err)
	}

	return route{
		Match:    []routeMatch{{Host: []string{hostname}}},
		Handle:   []json.RawMessage{handlerBytes},
		Terminal: true,
	}, nil
}

// RegisterRoute points {slug}.{domain} at dialTarget — "localhost:3001" for
// host-exec, "project-abc123:3000" for a container on a shared Docker
// network. The caller decides the target; this client doesn't know or
// care which runtime produced it.
func (c *CaddyClient) RegisterRoute(slug, dialTarget string) error {
	hostname := fmt.Sprintf("%s.%s", slug, c.Domain)

	routes, err := c.getRoutes()
	if err != nil {
		return err
	}

	newRoute, err := buildReverseProxyRoute(hostname, dialTarget)
	if err != nil {
		return err
	}

	replaced := false
	for i, r := range routes {
		if matchesHost(r, hostname) {
			routes[i] = newRoute
			replaced = true
			break
		}
	}
	if !replaced {
		routes = append([]route{newRoute}, routes...)
	}

	return c.putRoutes(routes)
}

func (c *CaddyClient) RemoveRoute(slug string) error {
	hostname := fmt.Sprintf("%s.%s", slug, c.Domain)

	routes, err := c.getRoutes()
	if err != nil {
		return err
	}

	out := routes[:0]
	found := false
	for _, r := range routes {
		if matchesHost(r, hostname) {
			found = true
			continue
		}
		out = append(out, r)
	}
	if !found {
		return nil
	}

	return c.putRoutes(out)
}

func matchesHost(r route, hostname string) bool {
	for _, m := range r.Match {
		for _, h := range m.Host {
			if h == hostname {
				return true
			}
		}
	}
	return false
}

