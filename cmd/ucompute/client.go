package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

type client struct {
	cfg        *Config
	r          relay
	home       string
	cwd        string
	stdout     io.Writer
	stderr     io.Writer
	quiet      bool
	poll       time.Duration
	ackTimeout time.Duration
}

func newClient(cfg *Config, r relay) *client {
	cwd, _ := os.Getwd()
	return &client{cfg: cfg, r: r, home: ucHome(), cwd: cwd, stdout: os.Stdout, stderr: os.Stderr,
		poll: time.Second, ackTimeout: 90 * time.Second}
}

func (c *client) infof(format string, a ...any) {
	if !c.quiet {
		fmt.Fprintf(c.stderr, "ucompute: "+format+"\n", a...)
	}
}

func (c *client) failf(format string, a ...any) int {
	fmt.Fprintf(c.stderr, "ucompute: "+format+"\n", a...)
	return exitUcompute
}

type runOpts struct {
	worker  string
	timeout time.Duration
	fetch   []string
	include []string
	env     []string
	noSync  bool
	detach  bool
}

func cmdRun(args []string) int {
	fs := newFlagSet("run")
	o := runOpts{timeout: time.Hour}
	var quiet bool
	for _, n := range []string{"worker", "w"} {
		fs.StringVar(&o.worker, n, "", "")
	}
	for _, n := range []string{"timeout", "t"} {
		fs.Var(durationFlag{&o.timeout}, n, "")
	}
	for _, n := range []string{"env", "e"} {
		fs.Var((*stringList)(&o.env), n, "")
	}
	fs.Var((*stringList)(&o.fetch), "fetch", "")
	fs.Var((*stringList)(&o.include), "include", "")
	fs.BoolVar(&o.noSync, "no-sync", false, "")
	for _, n := range []string{"detach", "d"} {
		fs.BoolVar(&o.detach, n, false, "")
	}
	for _, n := range []string{"quiet", "q"} {
		fs.BoolVar(&quiet, n, false, "")
	}
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() == 0 {
		return fail("no command given. Example: ucompute run -- npm test")
	}
	cfg, err := loadConfig()
	if err != nil {
		return fail("%v", err)
	}
	r, err := openRelay(cfg)
	if err != nil {
		return fail("%v", err)
	}
	c := newClient(cfg, r)
	c.quiet = quiet
	c.pruneRecords()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return c.run(ctx, joinCommand(fs.Args()), o)
}

func (c *client) run(ctx context.Context, command string, o runOpts) int {
	worker, err := c.pickWorker(o.worker)
	if err != nil {
		return c.failf("%v", err)
	}
	env, err := parseEnvFlags(o.env)
	if err != nil {
		return c.failf("%v", err)
	}
	fetch := make([]string, len(o.fetch))
	for i, p := range o.fetch {
		fetch[i] = filepath.ToSlash(p)
	}
	spec := &jobSpec{V: protocolVersion, ID: newJobID(), Command: command, Sync: !o.noSync, Env: env,
		TimeoutSec: int(o.timeout / time.Second), Fetch: fetch, Client: c.cfg.Name, Created: time.Now().UTC()}
	start := time.Now()
	if o.noSync {
		if err := c.submitBare(spec, worker); err != nil {
			return c.failf("submitting the job: %v", err)
		}
	} else {
		snap, err := takeSnapshot(c.home, c.cwd, spec, o.include)
		if err != nil {
			return c.failf("%v", err)
		}
		for attempt := 1; ; attempt++ {
			err := snap.push(c.r, jobRef(worker, spec.ID))
			if err == nil {
				break
			}
			if attempt == 3 || ctx.Err() != nil {
				return c.failf("uploading to the relay: %v", err)
			}
			c.infof("upload failed (%v); retrying…", err)
			sleepCtx(ctx, time.Duration(attempt)*3*time.Second)
		}
		c.infof("synced %s (%s) to %s in %s", snap.project, plural(snap.changed, "changed file"), worker, roundDur(time.Since(start)))
	}
	c.saveRecord(&jobRecord{ID: spec.ID, Worker: worker, Command: command, Created: spec.Created})
	if ctx.Err() != nil {
		return c.cancel(worker, spec.ID)
	}
	if o.detach {
		return c.detach(ctx, worker, spec.ID)
	}
	return c.follow(ctx, worker, spec.ID, true)
}

// submitBare submits a job that doesn't carry a project snapshot.
func (c *client) submitBare(spec *jobSpec, worker string) error {
	repo, err := c.cacheRepo()
	if err != nil {
		return err
	}
	env := gitEnv(c.r.gitConfig()...)
	tree, err := gitRunIn("", env, strings.NewReader(""), "--git-dir", repo, "mktree")
	if err != nil {
		return err
	}
	commit, err := gitRunIn("", env, strings.NewReader(spec.message()), "--git-dir", repo, "commit-tree", tree, "-F", "-")
	if err != nil {
		return err
	}
	_, err = gitRun("", env, "--git-dir", repo, "push", "--quiet", "--no-verify", c.r.url(), commit+":refs/heads/"+jobRef(worker, spec.ID))
	return err
}

// cacheRepo is a local bare repo results are fetched into.
func (c *client) cacheRepo() (string, error) {
	dir := filepath.Join(c.home, "cache.git")
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(c.home, 0o700); err != nil {
		return "", err
	}
	_, err := gitRun("", nil, "init", "--bare", "--quiet", dir)
	return dir, err
}

// follow streams a job's output until it finishes and returns its exit code.
// With cancelOnStop, an interrupt cancels the remote job; otherwise it only
// detaches.
func (c *client) follow(ctx context.Context, worker, id string, cancelOnStop bool) int {
	repo, err := c.cacheRepo()
	if err != nil {
		return c.failf("%v", err)
	}
	env := gitEnv(c.r.gitConfig()...)
	local := "refs/ucompute/" + id
	var (
		seen, status, waiting string
		offset                int64
		deadline              = time.Now().Add(c.ackTimeout)
		errSince              time.Time
	)
	for {
		if ctx.Err() != nil {
			if cancelOnStop {
				return c.cancel(worker, id)
			}
			c.infof("detached; job %s keeps running on %s. Re-attach with: ucompute logs %s", id, worker, id)
			return exitCancelled
		}
		sha, ok, err := c.r.getRef(resultRef(worker, id))
		if err == nil && ok && sha != seen {
			_, err = gitRun("", env, "--git-dir", repo, "fetch", "--no-tags", "--quiet", c.r.url(),
				"+refs/heads/"+resultRef(worker, id)+":"+local)
		}
		if err != nil {
			if errSince.IsZero() {
				errSince = time.Now()
				c.infof("relay unreachable (%v); retrying…", err)
			}
			if time.Since(errSince) > 5*time.Minute {
				return c.failf("giving up on job %s: %v (re-attach later with: ucompute logs %s)", id, err, id)
			}
			sleepCtx(ctx, 3*time.Second)
			continue
		}
		errSince = time.Time{}
		if !ok {
			if seen != "" {
				return c.failf("job %s was removed from the relay before it finished", id)
			}
			if time.Now().After(deadline) {
				_ = c.r.deleteRef(jobRef(worker, id))
				return c.failf("worker %q did not pick up job %s within %s. Is it online? Check: ucompute status", worker, id, c.ackTimeout)
			}
			sleepCtx(ctx, c.poll)
			continue
		}
		if sha != seen {
			seen = sha
			res, output, err := c.readUpdate(repo, local, offset)
			if err != nil {
				return c.failf("reading job output: %v", err)
			}
			if res.Status != status || res.Waiting != waiting {
				status, waiting = res.Status, res.Waiting
				switch {
				case res.Status == statusQueued && res.Waiting != "":
					c.infof("waiting on %s: %s", worker, res.Waiting)
				case res.Status == statusRunning:
					c.infof("running on %s", worker)
				}
			}
			_, _ = c.stdout.Write(output)
			offset += int64(len(output))
			if res.final() {
				return c.finish(repo, local, worker, id, res)
			}
		}
		sleepCtx(ctx, c.poll)
	}
}

// readUpdate returns the job's current result and the output after offset.
func (c *client) readUpdate(repo, local string, offset int64) (*jobResult, []byte, error) {
	raw, err := gitOut("", nil, nil, "--git-dir", repo, "cat-file", "blob", local+":result.json")
	if err != nil {
		return nil, nil, err
	}
	var res jobResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, nil, err
	}
	size, err := gitRun("", nil, "--git-dir", repo, "cat-file", "-s", local+":log.txt")
	if n, _ := strconv.ParseInt(size, 10, 64); err != nil || n <= offset {
		return &res, nil, nil
	}
	data, err := gitOut("", nil, nil, "--git-dir", repo, "cat-file", "blob", local+":log.txt")
	if err != nil || int64(len(data)) <= offset {
		return &res, nil, err
	}
	return &res, data[offset:], nil
}

func (c *client) finish(repo, local, worker, id string, res *jobResult) int {
	if len(res.Artifacts) > 0 {
		files, err := c.extractArtifacts(repo, local)
		if err != nil {
			c.infof("could not copy fetched files: %v", err)
		} else {
			c.infof("fetched %s into %s", plural(len(files), "file"), c.cwd)
		}
	}
	for _, note := range res.Notes {
		c.infof("note: %s", note)
	}
	if data, err := gitOut("", nil, nil, "--git-dir", repo, "cat-file", "blob", local+":log.txt"); err == nil {
		_ = os.WriteFile(filepath.Join(c.home, "jobs", id+".log"), data, 0o600)
	}
	rec := c.loadRecord(id)
	if rec == nil {
		rec = &jobRecord{ID: id, Worker: worker}
	}
	rec.Result = res
	c.saveRecord(rec)
	_ = c.r.deleteRef(jobRef(worker, id))
	_ = c.r.deleteRef(resultRef(worker, id))
	_, _ = gitRun("", nil, "--git-dir", repo, "update-ref", "-d", local)
	return c.report(res)
}

// report prints the outcome line and maps it to an exit code.
func (c *client) report(res *jobResult) int {
	took := ""
	if !res.StartedAt.IsZero() && !res.FinishedAt.IsZero() {
		took = " after " + roundDur(res.FinishedAt.Sub(res.StartedAt))
	}
	switch res.Status {
	case statusExited:
		c.infof("exit %d%s on %s (job %s)", res.ExitCode, took, res.Worker, res.ID)
		if res.ExitCode != 0 && res.ExitCode%256 == 0 {
			return 1 // don't let a large exit code wrap around to "success"
		}
		return res.ExitCode
	case statusTimeout:
		fmt.Fprintf(c.stderr, "ucompute: timed out%s on %s (job %s)\n", took, res.Worker, res.ID)
		return exitTimeout
	case statusCancelled:
		fmt.Fprintf(c.stderr, "ucompute: job %s was cancelled\n", res.ID)
		return exitCancelled
	}
	return c.failf("job %s failed on %s: %s", res.ID, res.Worker, res.Error)
}

func (c *client) extractArtifacts(repo, local string) ([]string, error) {
	out, err := gitOut("", nil, nil, "--git-dir", repo, "ls-tree", "-r", "-z", local, "--", "artifacts")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, rec := range strings.Split(string(out), "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta) // mode type sha
		if !ok || len(f) != 3 || f[1] != "blob" || f[0] == "120000" {
			continue
		}
		rel := strings.TrimPrefix(path, "artifacts/")
		dst := filepath.Join(c.cwd, filepath.FromSlash(rel))
		if !within(c.cwd, dst) {
			continue
		}
		data, err := gitOut("", nil, nil, "--git-dir", repo, "cat-file", "blob", f[2])
		if err != nil {
			return files, err
		}
		mode := os.FileMode(0o644)
		if f[0] == "100755" {
			mode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return files, err
		}
		if err := os.WriteFile(dst, data, mode); err != nil {
			return files, err
		}
		files = append(files, rel)
	}
	return files, nil
}

// detach waits until the worker has accepted the job, then returns.
func (c *client) detach(ctx context.Context, worker, id string) int {
	deadline := time.Now().Add(c.ackTimeout)
	for {
		if _, ok, err := c.r.getRef(resultRef(worker, id)); err == nil && ok {
			break
		}
		if ctx.Err() != nil {
			return c.cancel(worker, id)
		}
		if time.Now().After(deadline) {
			_ = c.r.deleteRef(jobRef(worker, id))
			return c.failf("worker %q did not pick up job %s within %s. Is it online? Check: ucompute status", worker, id, c.ackTimeout)
		}
		sleepCtx(ctx, c.poll)
	}
	fmt.Fprintln(c.stdout, id)
	c.infof("job %s accepted by %s. Follow it with: ucompute logs %s", id, worker, id)
	return 0
}

func (c *client) cancel(worker, id string) int {
	fmt.Fprintf(c.stderr, "\nucompute: cancelling job %s on %s\n", id, worker)
	if err := c.r.deleteRef(jobRef(worker, id)); err != nil {
		c.infof("could not cancel: %v", err)
	}
	return exitCancelled
}

// ---- workers ----

type workerInfo struct {
	heartbeat
	Online  bool          `json:"online"`
	AgeSecs int           `json:"last_seen_seconds_ago"`
	age     time.Duration `json:"-"`
}

func (w *workerInfo) usable() error {
	switch {
	case w.Stopped:
		return fmt.Errorf("worker %q is stopped (since %s)", w.Name, humanAge(w.age))
	case !w.Online:
		return fmt.Errorf("worker %q is offline (last seen %s). Make sure that machine is awake and online", w.Name, humanAge(w.age))
	case w.Paused != "":
		return fmt.Errorf("worker %q is paused: %s", w.Name, w.Paused)
	}
	return nil
}

func (c *client) workers() ([]workerInfo, error) {
	refs, err := c.r.listRefs("worker/")
	if err != nil {
		return nil, err
	}
	var out []workerInfo
	for ref, sha := range refs {
		msg, err := c.r.commitMessage(sha)
		if err != nil {
			return nil, err
		}
		hb, err := parseHeartbeat(msg)
		if err != nil {
			continue
		}
		if hb.Name == "" {
			hb.Name = strings.TrimPrefix(ref, "worker/")
		}
		age := max(time.Since(hb.Seen), 0)
		out = append(out, workerInfo{heartbeat: *hb, age: age, AgeSecs: int(age.Seconds()), Online: !hb.Stopped && age < onlineWindow})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

const noWorkersHelp = "no workers yet. On the machine that should run jobs, install ucompute and run: ucompute setup --worker"

func (c *client) pickWorker(want string) (string, error) {
	ws, err := c.workers()
	if err != nil {
		return "", fmt.Errorf("reading workers from the relay: %w", err)
	}
	if want == "" {
		want = c.cfg.DefaultWorker
	}
	if want != "" {
		var names []string
		for i := range ws {
			if ws[i].Name == want {
				return want, ws[i].usable()
			}
			names = append(names, ws[i].Name)
		}
		if len(names) == 0 {
			return "", errors.New(noWorkersHelp)
		}
		return "", fmt.Errorf("there is no worker named %q (known: %s)", want, strings.Join(names, ", "))
	}
	var best *workerInfo
	var reasons []string
	for i := range ws {
		w := &ws[i]
		if err := w.usable(); err != nil {
			reasons = append(reasons, err.Error())
			continue
		}
		if best == nil || betterWorker(w, best, c.cfg.Name) {
			best = w
		}
	}
	if best == nil {
		if len(ws) == 0 {
			return "", errors.New(noWorkersHelp)
		}
		return "", fmt.Errorf("no worker is available: %s", strings.Join(reasons, "; "))
	}
	if best.Name == c.cfg.Name {
		c.infof("no other worker is online, so the job runs on this machine")
	}
	return best.Name, nil
}

func betterWorker(a, b *workerInfo, self string) bool {
	if (a.Name == self) != (b.Name == self) {
		return b.Name == self // prefer another machine over this one
	}
	if len(a.Running) != len(b.Running) {
		return len(a.Running) < len(b.Running)
	}
	return a.CPUs > b.CPUs
}

// ---- job records (local history, for `ucompute logs`) ----

type jobRecord struct {
	ID      string     `json:"id"`
	Worker  string     `json:"worker"`
	Command string     `json:"command,omitempty"`
	Created time.Time  `json:"created"`
	Result  *jobResult `json:"result,omitempty"`
}

func (c *client) recordPath(id string) string { return filepath.Join(c.home, "jobs", id+".json") }

func (c *client) saveRecord(r *jobRecord) {
	_ = writeFileAtomic(c.recordPath(r.ID), mustJSON(r), 0o600)
}

func (c *client) loadRecord(id string) *jobRecord {
	b, err := os.ReadFile(c.recordPath(id))
	if err != nil {
		return nil
	}
	var r jobRecord
	if json.Unmarshal(b, &r) != nil {
		return nil
	}
	return &r
}

func (c *client) latestRecord() *jobRecord {
	files, _ := filepath.Glob(filepath.Join(c.home, "jobs", "*.json"))
	sort.Strings(files) // ids sort by time
	if len(files) == 0 {
		return nil
	}
	return c.loadRecord(strings.TrimSuffix(filepath.Base(files[len(files)-1]), ".json"))
}

func (c *client) pruneRecords() {
	files, _ := filepath.Glob(filepath.Join(c.home, "jobs", "*.*"))
	for _, f := range files {
		if info, err := os.Stat(f); err == nil && !info.IsDir() && time.Since(info.ModTime()) > 14*24*time.Hour {
			os.Remove(f)
		}
	}
}

func cmdLogs(args []string) int {
	fs := newFlagSet("logs")
	var quiet bool
	for _, n := range []string{"quiet", "q"} {
		fs.BoolVar(&quiet, n, false, "")
	}
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	cfg, err := loadConfig()
	if err != nil {
		return fail("%v", err)
	}
	r, err := openRelay(cfg)
	if err != nil {
		return fail("%v", err)
	}
	c := newClient(cfg, r)
	c.quiet = quiet
	var rec *jobRecord
	if id := fs.Arg(0); id != "" {
		if rec = c.loadRecord(id); rec == nil {
			rec = &jobRecord{ID: id}
		}
	} else if rec = c.latestRecord(); rec == nil {
		return fail("no jobs yet")
	}
	if rec.Result != nil && rec.Result.final() {
		if data, err := os.ReadFile(filepath.Join(c.home, "jobs", rec.ID+".log")); err == nil {
			_, _ = c.stdout.Write(data)
		}
		return c.report(rec.Result)
	}
	if rec.Worker == "" {
		for _, prefix := range []string{"result/", "job/"} {
			refs, err := r.listRefs(prefix)
			if err != nil {
				return fail("%v", err)
			}
			for ref := range refs {
				if strings.HasSuffix(ref, "/"+rec.ID) {
					rec.Worker = strings.TrimSuffix(strings.TrimPrefix(ref, prefix), "/"+rec.ID)
				}
			}
			if rec.Worker != "" {
				break
			}
		}
		if rec.Worker == "" {
			return fail("job %s not found (results are kept for 24 hours)", rec.ID)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return c.follow(ctx, rec.Worker, rec.ID, false)
}

func cmdStatus(args []string) int {
	fs := newFlagSet("status")
	var asJSON bool
	fs.BoolVar(&asJSON, "json", false, "")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	cfg, err := loadConfig()
	if err != nil {
		return fail("%v", err)
	}
	r, err := openRelay(cfg)
	if err != nil {
		return fail("%v", err)
	}
	c := newClient(cfg, r)
	ws, werr := c.workers()
	svc := serviceStatus()
	if asJSON {
		out := map[string]any{"version": version, "machine": cfg.Name, "relay": r.describe(), "service": svc, "workers": ws}
		if werr != nil {
			out["error"] = werr.Error()
		}
		_, _ = os.Stdout.Write(mustJSON(out))
		if werr != nil {
			return exitUcompute
		}
		return 0
	}
	fmt.Printf("ucompute %s\n", version)
	fmt.Printf("  this machine    %s\n", cfg.Name)
	relayLine := r.describe()
	if gr, ok := r.(*githubRelay); ok {
		if info, err := gr.gh.repo(gr.repo); err == nil {
			if info.Private {
				relayLine += " (private)"
			} else {
				relayLine += " (PUBLIC — make it private!)"
			}
		}
	}
	fmt.Printf("  relay           %s\n", relayLine)
	fmt.Printf("  worker service  %s\n", svc)
	if werr != nil {
		return fail("could not read workers: %v", werr)
	}
	fmt.Println()
	if len(ws) == 0 {
		fmt.Println("No workers yet. On the machine that should run jobs: ucompute setup --worker")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKER\tSTATE\tLAST SEEN\tSYSTEM\tCPUS\tMEMORY\tJOBS\tVERSION")
	for _, w := range ws {
		state := "online"
		switch {
		case w.Stopped:
			state = "stopped"
		case !w.Online:
			state = "offline"
		case w.Paused != "":
			state = "paused"
		}
		jobs := "idle"
		if n := len(w.Running); n > 0 {
			jobs = fmt.Sprintf("%d running", n)
		}
		if w.Queued > 0 {
			jobs += fmt.Sprintf(", %d queued", w.Queued)
		}
		name := w.Name
		if w.Name == cfg.Name {
			name += " (this machine)"
		}
		mem := "?"
		if w.MemBytes > 0 {
			mem = fmt.Sprintf("%d GB", (w.MemBytes+(1<<29))>>30)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s/%s\t%d\t%s\t%s\t%s\n", name, state, humanAge(w.age), w.OS, w.Arch, w.CPUs, mem, jobs, w.Version)
	}
	_ = tw.Flush()
	for _, w := range ws {
		if w.Paused != "" {
			fmt.Printf("\n%s is paused: %s\n", w.Name, w.Paused)
		}
	}
	return 0
}

// ---- helpers ----

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// joinCommand turns `ucompute run -- npm test` into one shell command line.
// A single argument is used as-is, so `ucompute run "a && b"` works too.
func joinCommand(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func parseEnvFlags(list []string) (map[string]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	env := map[string]string{}
	for _, e := range list {
		k, v, ok := strings.Cut(e, "=")
		if k == "" {
			return nil, fmt.Errorf("bad --env %q (want KEY=VALUE)", e)
		}
		if !ok {
			if v, ok = os.LookupEnv(k); !ok {
				return nil, fmt.Errorf("--env %s: not set on this machine (use KEY=VALUE)", k)
			}
		}
		env[k] = v
	}
	return env, nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func roundDur(d time.Duration) string {
	if d < 10*time.Second {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
