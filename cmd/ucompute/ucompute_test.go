package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeName(t *testing.T) {
	for in, want := range map[string]string{
		"Neals-MacBook-Pro": "neals-macbook-pro",
		"my box!!":          "my-box",
		"..weird..lock":     "weird",
		"":                  "",
	} {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProjectID(t *testing.T) {
	for origin, want := range map[string]string{
		"https://github.com/nealrau/Veramast.git": "nealrau-veramast",
		"git@github.com:nealrau/Veramast.git":     "nealrau-veramast",
		"ssh://git@host:22/team/app/":             "team-app",
	} {
		if got := projectID("/x", origin); got != want {
			t.Errorf("projectID(%q) = %q, want %q", origin, got, want)
		}
	}
	if a, b := projectID("/a/proj", ""), projectID("/b/proj", ""); a == b || !strings.HasPrefix(a, "proj-") {
		t.Errorf("folders without a remote should get distinct ids, got %q and %q", a, b)
	}
}

func TestJoinCommand(t *testing.T) {
	if got := joinCommand([]string{"npm test && echo ok"}); got != "npm test && echo ok" {
		t.Errorf("single arg should pass through, got %q", got)
	}
	if got := joinCommand([]string{"echo", "it's", "a b", "--x=1"}); got != `echo 'it'\''s' 'a b' --x=1` {
		t.Errorf("got %q", got)
	}
}

func TestJobSpecRoundTrip(t *testing.T) {
	in := &jobSpec{V: 1, ID: newJobID(), Command: "make test", Sync: true, Env: map[string]string{"A": "1"}, Created: time.Now().UTC()}
	out, err := parseJobSpec(in.message())
	if err != nil || out.Command != in.Command || out.Env["A"] != "1" || !out.Sync {
		t.Fatalf("round trip failed: %+v, %v", out, err)
	}
	if ts, ok := jobIDTime(in.ID); !ok || time.Since(ts) > time.Minute {
		t.Errorf("job id %q should carry its creation time", in.ID)
	}
}

func TestCappedWriter(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := &cappedWriter{f: f, max: 10}
	_, _ = w.Write([]byte("12345"))
	_, _ = w.Write([]byte("67890abc"))
	_, _ = w.Write([]byte("more"))
	b, _ := os.ReadFile(f.Name())
	if !strings.HasPrefix(string(b), "1234567890\n[ucompute: output cut off") || !w.wasTruncated() {
		t.Errorf("unexpected log %q", b)
	}
}

func TestGitAtLeast(t *testing.T) {
	if !gitAtLeast("2.50.1", 2, 31) || gitAtLeast("2.30.9", 2, 31) || !gitAtLeast("3.0", 2, 31) {
		t.Error("gitAtLeast is wrong")
	}
}

// TestEndToEnd runs a real worker and client against a local relay.
func TestEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	t.Setenv("UCOMPUTE_HOME", filepath.Join(tmp, "unused-home"))
	relayDir := filepath.Join(tmp, "relay.git")
	mustGit(t, "", "init", "--bare", "--quiet", relayDir)

	// Worker.
	wcfg := &Config{Relay: relayDir, Name: "box", MachineID: "w1", MaxParallel: 2}
	wr, _ := openRelay(wcfg)
	w := newWorker(wcfg, wr, filepath.Join(tmp, "worker"), log.New(io.Discard, "", 0))
	w.poll = 100 * time.Millisecond
	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { _ = w.run(ctx); close(stopped) }()
	defer func() { stop(); <-stopped }()

	// A project with committed, modified, untracked and ignored files.
	proj := filepath.Join(tmp, "proj")
	mustGit(t, "", "init", "--quiet", proj)
	write(t, proj, "a.txt", "committed\n")
	write(t, proj, "sub/c.txt", "in sub\n")
	write(t, proj, ".gitignore", ".env\nout/\n")
	mustGit(t, proj, "add", "-A")
	mustGit(t, proj, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--quiet", "-m", "init")
	write(t, proj, "a.txt", "modified\n")
	write(t, proj, "b.txt", "untracked\n")
	write(t, proj, ".env", "SECRET=1\n")

	ccfg := &Config{Relay: relayDir, Name: "laptop", MachineID: "c1"}
	cr, _ := openRelay(ccfg)
	newC := func(cwd string) (*client, *bytes.Buffer) {
		var out bytes.Buffer
		return &client{cfg: ccfg, r: cr, home: filepath.Join(tmp, "client"), cwd: cwd, stdout: &out,
			stderr: io.Discard, poll: 100 * time.Millisecond, ackTimeout: 30 * time.Second}, &out
	}
	waitFor(t, func() bool { _, ok, _ := cr.getRef(workerRef("box")); return ok })

	t.Run("syncs working tree and returns exit code", func(t *testing.T) {
		c, out := newC(proj)
		code := c.run(context.Background(), "cat a.txt b.txt; test -f .env && echo has-env || echo no-env; mkdir -p out; echo artifact > out/r.txt; exit 3",
			runOpts{worker: "box", timeout: time.Minute, fetch: []string{"out"}})
		if code != 3 {
			t.Fatalf("exit code %d, output %q", code, out)
		}
		for _, want := range []string{"modified", "untracked", "no-env"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output %q lacks %q", out, want)
			}
		}
		if b, err := os.ReadFile(filepath.Join(proj, "out", "r.txt")); err != nil || string(b) != "artifact\n" {
			t.Errorf("fetched file: %q, %v", b, err)
		}
		if _, ok, _ := cr.getRef(jobRef("box", c.latestRecord().ID)); ok {
			t.Error("job branch should be deleted after the job")
		}
	})

	t.Run("include ignored file", func(t *testing.T) {
		c, out := newC(proj)
		if code := c.run(context.Background(), "cat .env", runOpts{worker: "box", timeout: time.Minute, include: []string{".env"}}); code != 0 || !strings.Contains(out.String(), "SECRET=1") {
			t.Fatalf("code %d, output %q", code, out)
		}
	})

	t.Run("runs in the matching subfolder", func(t *testing.T) {
		c, out := newC(filepath.Join(proj, "sub"))
		if code := c.run(context.Background(), "cat c.txt ../a.txt", runOpts{worker: "box", timeout: time.Minute}); code != 0 || out.String() != "in sub\nmodified\n" {
			t.Fatalf("code %d, output %q", code, out)
		}
	})

	// The timing checks only prove the job was killed rather than left to
	// finish its 120s sleep, so they hold up on a slow, loaded machine.
	t.Run("timeout", func(t *testing.T) {
		c, _ := newC(proj)
		start := time.Now()
		if code := c.run(context.Background(), "sleep 120", runOpts{worker: "box", timeout: time.Second}); code != exitTimeout {
			t.Fatalf("code %d", code)
		}
		if d := time.Since(start); d > 90*time.Second {
			t.Errorf("timed-out job wasn't killed (took %s)", d)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		c, _ := newC(proj)
		cctx, cancel := context.WithCancel(context.Background())
		go func() {
			waitFor(t, func() bool { return len(w.runningIDs()) > 0 })
			cancel()
		}()
		start := time.Now()
		if code := c.run(cctx, "sleep 120", runOpts{worker: "box", timeout: 5 * time.Minute}); code != exitCancelled {
			t.Fatalf("code %d", code)
		}
		waitFor(t, func() bool { return len(w.runningIDs()) == 0 })
		if d := time.Since(start); d > 90*time.Second {
			t.Errorf("cancelled job wasn't killed (took %s)", d)
		}
	})

	t.Run("no-sync", func(t *testing.T) {
		c, out := newC(tmp)
		if code := c.run(context.Background(), "echo hi from scratch", runOpts{worker: "box", timeout: time.Minute, noSync: true}); code != 0 || out.String() != "hi from scratch\n" {
			t.Fatalf("code %d, output %q", code, out)
		}
	})

	t.Run("folder without git", func(t *testing.T) {
		dir := filepath.Join(tmp, "plain")
		write(t, dir, "data.txt", "plain folder\n")
		write(t, dir, "node_modules/x.js", "skip me\n")
		c, out := newC(dir)
		if code := c.run(context.Background(), "cat data.txt; ls node_modules 2>/dev/null || echo no-node-modules", runOpts{worker: "box", timeout: time.Minute}); code != 0 ||
			out.String() != "plain folder\nno-node-modules\n" {
			t.Fatalf("code %d, output %q", code, out)
		}
	})

	t.Run("worker listing", func(t *testing.T) {
		c, _ := newC(proj)
		ws, err := c.workers()
		if err != nil || len(ws) != 1 || !ws[0].Online || ws[0].Name != "box" {
			t.Fatalf("workers: %+v, %v", ws, err)
		}
		if _, err := c.pickWorker(""); err != nil {
			t.Fatal(err)
		}
		if _, err := c.pickWorker("nope"); err == nil {
			t.Fatal("expected an error for an unknown worker")
		}
	})
}

func (w *worker) runningIDs() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var ids []string
	for id, j := range w.active {
		if j.status() == statusRunning {
			ids = append(ids, id)
		}
	}
	return ids
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", orDot(dir)}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func orDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Error("timed out waiting")
}
