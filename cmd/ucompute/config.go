package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config is stored in ~/.ucompute/config.json.
type Config struct {
	// Relay is the private GitHub repo ("owner/name") both machines use to
	// exchange jobs and results. An absolute path to a bare git repo also
	// works (handy for tests and shared drives).
	Relay              string `json:"relay"`
	Name               string `json:"name"`       // this machine's name as a worker
	MachineID          string `json:"machine_id"` // detects two machines claiming one name
	DefaultWorker      string `json:"default_worker,omitempty"`
	MaxParallel        int    `json:"max_parallel,omitempty"` // jobs this worker runs at once
	Shell              string `json:"shell,omitempty"`        // shell used to run jobs on this worker
	AllowCollaborators bool   `json:"allow_collaborators,omitempty"`
}

var errNotSetUp = errors.New("this machine is not set up yet — run: ucompute setup   (use `ucompute setup --worker` on the machine that should run jobs)")

func ucHome() string {
	if h := os.Getenv("UCOMPUTE_HOME"); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		h = "."
	}
	return filepath.Join(h, ".ucompute")
}

func configPath() string { return filepath.Join(ucHome(), "config.json") }

func loadConfig() (*Config, error) {
	b, err := os.ReadFile(configPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNotSetUp
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("reading %s: %w", configPath(), err)
	}
	if c.Relay == "" {
		return nil, errNotSetUp
	}
	if c.MachineID == "" {
		c.fill()
		_ = c.save()
	}
	c.fill()
	return &c, nil
}

func (c *Config) fill() {
	if c.Name == "" {
		c.Name = defaultName()
	}
	if c.MachineID == "" {
		c.MachineID = randomHex(8)
	}
	if c.MaxParallel <= 0 {
		c.MaxParallel = 2
	}
}

func (c *Config) save() error { return writeFileAtomic(configPath(), mustJSON(c), 0o600) }

func defaultName() string {
	h, _ := os.Hostname()
	h = strings.ToLower(h)
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	if n := sanitizeName(h); n != "" {
		return n
	}
	return "machine"
}

// resolveToken finds a GitHub token, preferring an explicit one and falling
// back to the GitHub CLI's login.
func resolveToken() (string, error) {
	if t := strings.TrimSpace(os.Getenv("UCOMPUTE_TOKEN")); t != "" {
		return t, nil
	}
	if b, err := os.ReadFile(filepath.Join(ucHome(), "token")); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if t := strings.TrimSpace(os.Getenv(k)); t != "" {
			return t, nil
		}
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return "", errors.New("no GitHub login found: install the GitHub CLI (https://cli.github.com) and run `gh auth login`, or set GH_TOKEN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if t := strings.TrimSpace(string(out)); err == nil && t != "" {
		return t, nil
	}
	if msg := strings.TrimSpace(stderr.String()); msg != "" || ctx.Err() != nil {
		if ctx.Err() != nil {
			msg = "timed out"
		}
		return "", fmt.Errorf("could not get your GitHub login from `gh auth token` (%s); run `gh auth login`, or set GH_TOKEN", msg)
	}
	return "", errors.New("no GitHub login found: run `gh auth login` (or set GH_TOKEN)")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func mustJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp-" + randomHex(4)
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// within reports whether p is base or inside it.
func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
