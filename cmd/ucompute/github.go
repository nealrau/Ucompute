package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ghClient is a minimal GitHub REST client. Polling goes through conditional
// requests: a 304 Not Modified does not count against the rate limit, which
// is what lets a worker watch the relay around the clock for free.
type ghClient struct {
	token string
	base  string
	hc    *http.Client

	mu    sync.Mutex
	etags map[string]cachedResponse
}

type cachedResponse struct {
	etag string
	body []byte
}

type apiError struct {
	Status  int
	Message string
	Path    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("GitHub API %s: %d %s", e.Path, e.Status, e.Message)
}

func isStatus(err error, code int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == code
}

func newGH(token string) *ghClient {
	base := "https://api.github.com"
	if v := os.Getenv("UCOMPUTE_GITHUB_API"); v != "" {
		base = strings.TrimRight(v, "/")
	}
	return &ghClient{token: token, base: base, hc: &http.Client{Timeout: 30 * time.Second}, etags: map[string]cachedResponse{}}
}

// call performs a request. With conditional set (GET only), it remembers the
// ETag and returns the cached body with changed=false on a 304.
func (g *ghClient) call(method, path string, in any, conditional bool) (body []byte, changed bool, err error) {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, false, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, g.base+path, rd)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ucompute/"+version)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	conditional = conditional && method == http.MethodGet
	var prev cachedResponse
	if conditional {
		g.mu.Lock()
		prev = g.etags[path]
		g.mu.Unlock()
		if prev.etag != "" {
			req.Header.Set("If-None-Match", prev.etag)
		}
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode == http.StatusNotModified && prev.etag != "" {
		return prev.body, false, nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if conditional {
			if et := resp.Header.Get("ETag"); et != "" {
				g.mu.Lock()
				g.etags[path] = cachedResponse{etag: et, body: data}
				g.mu.Unlock()
			}
		}
		return data, true, nil
	}
	if conditional {
		g.mu.Lock()
		delete(g.etags, path)
		g.mu.Unlock()
	}
	e := &apiError{Status: resp.StatusCode, Path: method + " " + path}
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &m) == nil {
		e.Message = m.Message
	}
	if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
		resp.Header.Get("X-RateLimit-Remaining") == "0" {
		e.Message = "API rate limit exceeded"
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			e.Message += "; it resets at " + time.Unix(reset, 0).Format("15:04")
		}
	}
	return nil, false, e
}

func (g *ghClient) getJSON(path string, out any) error {
	b, _, err := g.call(http.MethodGet, path, nil, false)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (g *ghClient) user() (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	if err := g.getJSON("/user", &u); err != nil {
		return "", err
	}
	return u.Login, nil
}

type repoInfo struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func (g *ghClient) repo(full string) (*repoInfo, error) {
	var r repoInfo
	if err := g.getJSON("/repos/"+full, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (g *ghClient) createRepo(name string) (*repoInfo, error) {
	b, _, err := g.call(http.MethodPost, "/user/repos", map[string]any{
		"name":        name,
		"private":     true,
		"auto_init":   true,
		"has_issues":  false,
		"has_wiki":    false,
		"description": "Relay for Ucompute (https://github.com/" + repoSlug + "). Keep it private: anyone who can push here can run commands on your workers.",
	}, false)
	if err != nil {
		return nil, err
	}
	var r repoInfo
	return &r, json.Unmarshal(b, &r)
}

// hardenRepo turns off features that would react to the project snapshots
// pushed into the relay: GitHub Actions would otherwise run the synced
// project's workflows (burning Actions minutes), and Dependabot would file
// alerts against it. Errors are returned for Actions only; the rest is
// best effort.
func (g *ghClient) hardenRepo(full string) error {
	var perms struct {
		Enabled bool `json:"enabled"`
	}
	if err := g.getJSON("/repos/"+full+"/actions/permissions", &perms); err == nil && !perms.Enabled {
		return nil
	}
	_, _, err := g.call(http.MethodPut, "/repos/"+full+"/actions/permissions", map[string]any{"enabled": false}, false)
	_, _, _ = g.call(http.MethodDelete, "/repos/"+full+"/vulnerability-alerts", nil, false)
	_, _, _ = g.call(http.MethodDelete, "/repos/"+full+"/automated-security-fixes", nil, false)
	return err
}

func (g *ghClient) collaborators(full string) ([]string, error) {
	var users []struct {
		Login string `json:"login"`
	}
	if err := g.getJSON("/repos/"+full+"/collaborators?per_page=100", &users); err != nil {
		return nil, err
	}
	out := make([]string, len(users))
	for i, u := range users {
		out[i] = u.Login
	}
	return out, nil
}
