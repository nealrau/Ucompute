package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	heartbeatEvery = time.Minute
	onlineWindow   = 3 * time.Minute  // a worker is online if its heartbeat is newer than this
	jobExpiry      = 15 * time.Minute // jobs nobody picked up for this long are dropped
	refRetention   = 24 * time.Hour   // finished jobs stay on the relay this long
	logCap         = 64 << 20
	artifactBudget = 1 << 30
)

var errLocked = errors.New("locked by another process")

type wjob struct {
	id, sha string
	spec    *jobSpec
	dir     string // log.txt, result.json and artifacts/ live here
	ctx     context.Context
	cancel  context.CancelFunc
	gone    atomic.Bool // the client deleted the job branch: cancel

	mu  sync.Mutex
	res jobResult
	tip string // last result commit pushed
}

func (j *wjob) update(f func(r *jobResult)) {
	j.mu.Lock()
	f(&j.res)
	j.mu.Unlock()
}

func (j *wjob) status() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.res.Status
}

type worker struct {
	cfg   *Config
	r     relay
	home  string
	repo  string // bare repo used to build result and heartbeat commits
	empty string // the empty tree
	poll  time.Duration
	log   *log.Logger

	mu       sync.Mutex
	active   map[string]*wjob
	done     map[string]time.Time
	projects map[string]chan struct{} // one job per project workspace at a time
	lastJob  time.Time
	paused   string

	sem     chan struct{}
	pushMu  sync.Mutex
	started time.Time
	wg      sync.WaitGroup
}

func newWorker(cfg *Config, r relay, home string, lg *log.Logger) *worker {
	return &worker{
		cfg: cfg, r: r, home: home, repo: filepath.Join(home, "relay.git"), poll: 2 * time.Second, log: lg,
		active: map[string]*wjob{}, done: map[string]time.Time{}, projects: map[string]chan struct{}{},
		sem: make(chan struct{}, max(cfg.MaxParallel, 1)), started: time.Now(),
	}
}

func cmdWorker(args []string) int {
	fs := newFlagSet("worker")
	var logPath string
	fs.StringVar(&logPath, "log", "", "")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	lg := log.New(os.Stderr, "", log.LstdFlags)
	if logPath != "" {
		f, err := openLog(logPath)
		if err != nil {
			return fail("%v", err)
		}
		defer f.Close()
		lg.SetOutput(f)
	}
	cfg, err := loadConfig()
	if err != nil {
		lg.Print(err)
		return fail("%v", err)
	}
	home := ucHome()
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fail("%v", err)
	}
	unlock, err := lockFile(filepath.Join(home, "worker.lock"))
	if err != nil {
		lg.Print("another worker is already running on this machine; exiting")
		return fail("another ucompute worker is already running on this machine (see: ucompute service status)")
	}
	defer unlock()
	pidFile := filepath.Join(home, "worker.pid")
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)
	defer os.Remove(pidFile)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Right after login the keychain or network may not be ready yet; keep trying.
	var r relay
	for attempt := 1; ; attempt++ {
		if r, err = openRelay(cfg); err == nil {
			break
		}
		lg.Print(err)
		if attempt == 6 || ctx.Err() != nil {
			return fail("%v", err)
		}
		sleepCtx(ctx, 10*time.Second)
	}
	if err := newWorker(cfg, r, home, lg).run(ctx); err != nil {
		lg.Printf("worker stopped: %v", err)
		return fail("%v", err)
	}
	return 0
}

func openLog(p string) (*os.File, error) {
	if info, err := os.Stat(p); err == nil && info.Size() > 5<<20 {
		_ = os.Rename(p, p+".1")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

func (w *worker) run(ctx context.Context) error {
	if err := w.init(); err != nil {
		return err
	}
	if err := w.checkNameClash(); err != nil {
		return err
	}
	w.loadState()
	w.log.Printf("ucompute %s: worker %q serving jobs from %s (up to %d at a time)", version, w.cfg.Name, w.r.describe(), cap(w.sem))
	w.refreshSecurity()
	w.sendHeartbeat(false)
	go w.background(ctx)
	var backoff time.Duration
	for {
		if w.pausedReason() == "" {
			refs, err := w.r.listRefs("job/" + w.cfg.Name + "/")
			if err != nil {
				backoff = min(max(2*backoff, 5*time.Second), time.Minute)
				w.log.Printf("polling the relay failed: %v (retrying in %s)", err, backoff)
			} else {
				backoff = 0
				w.reconcile(ctx, refs)
			}
		}
		wait := w.interval()
		if backoff > 0 {
			wait = backoff
		}
		select {
		case <-ctx.Done():
			w.shutdown()
			return nil
		case <-time.After(wait):
		}
	}
}

func (w *worker) init() error {
	for _, d := range []string{"jobs", "work", "tmp", "scratch"} {
		if err := os.MkdirAll(filepath.Join(w.home, d), 0o700); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(w.repo, "HEAD")); err != nil {
		if _, err := gitRun("", nil, "init", "--bare", "--quiet", w.repo); err != nil {
			return err
		}
	}
	empty, err := gitRunIn("", nil, strings.NewReader(""), "--git-dir", w.repo, "mktree")
	if err != nil {
		return err
	}
	w.empty = empty
	return nil
}

// checkNameClash refuses to start if a different machine is online under our
// name: both would run every job.
func (w *worker) checkNameClash() error {
	sha, ok, err := w.r.getRef(workerRef(w.cfg.Name))
	if err != nil || !ok {
		return nil
	}
	msg, err := w.r.commitMessage(sha)
	if err != nil {
		return nil
	}
	hb, err := parseHeartbeat(msg)
	if err == nil && hb.MachineID != "" && hb.MachineID != w.cfg.MachineID && !hb.Stopped && time.Since(hb.Seen) < onlineWindow {
		return fmt.Errorf("another machine is already online as worker %q; give this one its own name: ucompute setup --worker --name <name>", w.cfg.Name)
	}
	return nil
}

func (w *worker) interval() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.active) > 0 || time.Since(w.lastJob) < 10*time.Minute {
		return w.poll
	}
	return w.poll * 3 / 2
}

func (w *worker) pausedReason() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.paused
}

func (w *worker) isActive(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.active[id]
	return ok
}

// background sends heartbeats and does periodic housekeeping.
func (w *worker) background(ctx context.Context) {
	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()
	maint := time.NewTicker(10 * time.Minute)
	defer maint.Stop()
	w.gc()
	lastCheck := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hb.C:
			w.sendHeartbeat(false)
		case <-maint.C:
			if w.pausedReason() != "" || time.Since(lastCheck) > time.Hour {
				w.refreshSecurity()
				lastCheck = time.Now()
			}
			w.gc()
		}
	}
}

func (w *worker) refreshSecurity() {
	reason := ""
	if err := w.checkSecurity(); err != nil {
		reason = err.Error()
	}
	w.mu.Lock()
	prev := w.paused
	w.paused = reason
	w.mu.Unlock()
	switch {
	case reason != "" && reason != prev:
		w.log.Printf("PAUSED, not accepting jobs: %s", reason)
		w.sendHeartbeat(false)
	case reason == "" && prev != "":
		w.log.Printf("resumed")
		w.sendHeartbeat(false)
	}
}

// checkSecurity makes sure only the owner can submit jobs and the synced code
// stays private. Network errors don't pause the worker.
func (w *worker) checkSecurity() error {
	gr, ok := w.r.(*githubRelay)
	if !ok {
		return nil
	}
	info, err := gr.gh.repo(gr.repo)
	if isStatus(err, 404) {
		return fmt.Errorf("relay repo %s is missing or not accessible with this GitHub login", gr.repo)
	}
	if err != nil {
		return nil
	}
	if !info.Private {
		return fmt.Errorf("relay repo github.com/%s is public, which would expose your code. Make it private: gh repo edit %s --visibility private --accept-visibility-change-consequences", gr.repo, gr.repo)
	}
	if err := gr.gh.hardenRepo(gr.repo); err != nil {
		w.log.Printf("warning: could not turn off GitHub Actions on the relay: %v", err)
	}
	if w.cfg.AllowCollaborators {
		return nil
	}
	users, err := gr.gh.collaborators(gr.repo)
	if err != nil {
		return nil
	}
	var others []string
	for _, u := range users {
		if !strings.EqualFold(u, info.Owner.Login) {
			others = append(others, u)
		}
	}
	if len(others) > 0 {
		return fmt.Errorf("the relay repo has collaborators (%s) who could run commands on this machine. Remove them, or set \"allow_collaborators\": true in %s", strings.Join(others, ", "), configPath())
	}
	return nil
}

// reconcile starts new jobs and cancels jobs whose branch was deleted.
func (w *worker) reconcile(ctx context.Context, refs map[string]string) {
	prefix := "job/" + w.cfg.Name + "/"
	present := map[string]string{}
	for ref, sha := range refs {
		if id, ok := strings.CutPrefix(ref, prefix); ok && id != "" && !strings.Contains(id, "/") {
			present[id] = sha
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, j := range w.active {
		if _, ok := present[id]; !ok && !j.gone.Load() && !isFinal(j.status()) {
			j.gone.Store(true)
			j.cancel()
			w.log.Printf("job %s: cancelled by the client", id)
		}
	}
	for id, sha := range present {
		if _, ok := w.active[id]; ok {
			continue
		}
		if _, ok := w.done[id]; ok {
			continue
		}
		jctx, cancel := context.WithCancel(ctx)
		j := &wjob{id: id, sha: sha, ctx: jctx, cancel: cancel, dir: filepath.Join(w.home, "jobs", id)}
		w.active[id] = j
		w.lastJob = time.Now()
		w.wg.Add(1)
		go w.handle(j)
	}
}

func (w *worker) handle(j *wjob) {
	defer w.wg.Done()
	defer w.finish(j)
	j.res = jobResult{ID: j.id, Worker: w.cfg.Name, Status: statusQueued, QueuedAt: time.Now().UTC()}
	_ = os.RemoveAll(j.dir)
	if err := os.MkdirAll(j.dir, 0o700); err != nil {
		w.fail(j, "creating the job folder: %v", err)
		return
	}
	_ = os.WriteFile(filepath.Join(j.dir, "log.txt"), nil, 0o600)

	// A result can already exist if this worker restarted in the middle of the job.
	if sha, ok, err := w.r.getRef(resultRef(w.cfg.Name, j.id)); err == nil && ok {
		if msg, err := w.r.commitMessage(sha); err == nil {
			f := strings.Fields(msg)
			if len(f) > 0 && isFinal(f[len(f)-1]) {
				return
			}
		}
		w.fail(j, "the worker restarted while this job was running; run it again")
		return
	}
	msg, err := w.r.commitMessage(j.sha)
	if err != nil {
		w.fail(j, "reading the job: %v", err)
		return
	}
	spec, err := parseJobSpec(msg)
	if err != nil {
		w.fail(j, "bad job: %v", err)
		return
	}
	j.spec = spec
	switch {
	case spec.V > protocolVersion:
		w.fail(j, "this job needs a newer ucompute on worker %q (it has %s): run `ucompute update` there", w.cfg.Name, version)
		return
	case time.Since(spec.Created) > jobExpiry:
		w.fail(j, "job expired: it was submitted %s ago", roundDur(time.Since(spec.Created)))
		return
	}
	w.log.Printf("job %s from %s: %s", j.id, spec.Client, oneLine(spec.Command))
	if err := w.push(j); err != nil {
		w.log.Printf("job %s: %v", j.id, err)
	}
	release, ok := w.acquire(j)
	if !ok {
		w.stopped(j)
		return
	}
	defer release()
	dir, err := w.prepare(j)
	if err != nil {
		if j.ctx.Err() != nil {
			w.stopped(j)
			return
		}
		w.fail(j, "preparing the workspace: %v", err)
		return
	}
	w.execute(j, dir)
}

// acquire waits for this job's project workspace and a free slot.
func (w *worker) acquire(j *wjob) (release func(), ok bool) {
	var lock chan struct{}
	if j.spec.Sync {
		w.mu.Lock()
		if lock = w.projects[j.spec.Project]; lock == nil {
			lock = make(chan struct{}, 1)
			w.projects[j.spec.Project] = lock
		}
		w.mu.Unlock()
		if !w.take(j, lock, "another job for this project is running") {
			return nil, false
		}
	}
	if !w.take(j, w.sem, "the worker is busy with other jobs") {
		if lock != nil {
			<-lock
		}
		return nil, false
	}
	return func() {
		<-w.sem
		if lock != nil {
			<-lock
		}
	}, true
}

// take claims a slot in ch, telling the client why it waits if it has to.
func (w *worker) take(j *wjob, ch chan struct{}, why string) bool {
	select {
	case ch <- struct{}{}:
		return true
	default:
	}
	j.update(func(r *jobResult) { r.Waiting = why })
	_ = w.push(j)
	select {
	case ch <- struct{}{}:
		j.update(func(r *jobResult) { r.Waiting = "" })
		return true
	case <-j.ctx.Done():
		return false
	}
}

// prepare checks out the job's snapshot into the project's workspace. The
// workspace persists between jobs, so git-ignored files such as node_modules
// or build caches survive and the next run is fast.
func (w *worker) prepare(j *wjob) (string, error) {
	if !j.spec.Sync {
		return filepath.Join(w.home, "scratch"), nil
	}
	ws := filepath.Join(w.home, "work", sanitizeName(j.spec.Project))
	if _, err := os.Stat(filepath.Join(ws, ".git")); err != nil {
		if err := os.MkdirAll(ws, 0o755); err != nil {
			return "", err
		}
		if _, err := gitRun(ws, nil, "init", "--quiet"); err != nil {
			return "", err
		}
	}
	env := gitEnv(append(w.r.gitConfig(), kv{"core.autocrlf", "false"}, kv{"core.longpaths", "true"})...)
	for attempt := 1; ; attempt++ { // ride out network hiccups
		_, err := gitRun(ws, env, "fetch", "--no-tags", "--quiet", w.r.url(),
			"+refs/heads/"+jobRef(w.cfg.Name, j.id)+":refs/ucompute/job")
		if err == nil {
			break
		}
		if attempt == 4 || j.ctx.Err() != nil {
			return "", err
		}
		w.log.Printf("job %s: fetch failed (attempt %d): %v", j.id, attempt, err)
		sleepCtx(j.ctx, time.Duration(attempt)*3*time.Second)
	}
	if _, err := gitRun(ws, env, "checkout", "--quiet", "--force", "--detach", "refs/ucompute/job"); err != nil {
		return "", err
	}
	if _, err := gitRun(ws, env, "clean", "--quiet", "-f", "-d"); err != nil {
		return "", err
	}
	dir := filepath.Join(ws, filepath.FromSlash(j.spec.Dir))
	if !within(ws, dir) {
		return "", fmt.Errorf("bad working directory %q", j.spec.Dir)
	}
	return dir, os.MkdirAll(dir, 0o755)
}

func (w *worker) execute(j *wjob, dir string) {
	f, err := os.OpenFile(filepath.Join(j.dir, "log.txt"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		w.fail(j, "opening the log: %v", err)
		return
	}
	out := &cappedWriter{f: f, max: logCap}
	timeout := time.Duration(j.spec.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	ctx, cancel := context.WithTimeout(j.ctx, timeout)
	defer cancel()
	cmd := shellCommand(ctx, w.cfg.Shell, j.spec.Command)
	cmd.Dir = dir
	cmd.Env = w.jobEnv(j.spec)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 10 * time.Second
	j.update(func(r *jobResult) {
		r.Status, r.Waiting, r.StartedAt = statusRunning, "", time.Now().UTC()
	})
	if err := cmd.Start(); err != nil {
		f.Close()
		w.fail(j, "starting the command: %v", err)
		return
	}
	preventSleep(cmd.Process.Pid)
	if err := w.push(j); err != nil {
		w.log.Printf("job %s: %v", j.id, err)
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go w.streamLog(j, out, stop, stopped)
	_ = cmd.Wait()
	killTree(cmd)
	close(stop)
	<-stopped
	f.Close()

	if j.ctx.Err() != nil {
		w.stopped(j)
		return
	}
	j.update(func(r *jobResult) {
		r.FinishedAt = time.Now().UTC()
		if out.wasTruncated() {
			r.Notes = append(r.Notes, fmt.Sprintf("output was longer than %d MB and was cut off", logCap>>20))
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			r.Status, r.ExitCode = statusTimeout, exitTimeout
			r.Notes = append(r.Notes, fmt.Sprintf("stopped by the %s timeout (raise it with --timeout)", timeout))
			return
		}
		r.Status, r.ExitCode = statusExited, exitCode(cmd.ProcessState)
	})
	w.collectArtifacts(j, dir)
	w.pushFinal(j)
}

// streamLog pushes progress while the job runs: often at first, then less
// often for long jobs.
func (w *worker) streamLog(j *wjob, out *cappedWriter, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	start, lastPush := time.Now(), time.Now()
	var lastSize int64
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		size := out.size()
		if size == lastSize || time.Since(lastPush) < flushInterval(time.Since(start)) {
			continue
		}
		if err := w.push(j); err != nil {
			w.log.Printf("job %s: %v", j.id, err)
		}
		lastSize, lastPush = size, time.Now()
	}
}

func flushInterval(elapsed time.Duration) time.Duration {
	switch {
	case elapsed < 30*time.Second:
		return 2 * time.Second
	case elapsed < 5*time.Minute:
		return 4 * time.Second
	}
	return 10 * time.Second
}

func (w *worker) jobEnv(s *jobSpec) []string {
	env := append(os.Environ(), "UCOMPUTE=1", "UCOMPUTE_JOB="+s.ID, "UCOMPUTE_WORKER="+w.cfg.Name, "UCOMPUTE_CLIENT="+s.Client)
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func (w *worker) collectArtifacts(j *wjob, dir string) {
	budget := int64(artifactBudget)
	var got, notes []string
	for _, p := range j.spec.Fetch {
		clean := filepath.Clean(filepath.FromSlash(p))
		src := filepath.Join(dir, clean)
		if filepath.IsAbs(clean) || !within(dir, src) || src == dir {
			notes = append(notes, fmt.Sprintf("--fetch %s: must be a path inside the working directory", p))
			continue
		}
		if _, err := os.Stat(src); err != nil {
			notes = append(notes, fmt.Sprintf("--fetch %s: not found on the worker", p))
			continue
		}
		if err := copyTree(src, filepath.Join(j.dir, "artifacts", clean), &budget, &notes); err != nil {
			notes = append(notes, fmt.Sprintf("--fetch %s: %v", p, err))
			continue
		}
		got = append(got, p)
	}
	j.update(func(r *jobResult) {
		r.Artifacts = got
		r.Notes = append(r.Notes, notes...)
	})
}

// copyTree copies a file or folder, skipping symlinks, .git folders and files
// too big for GitHub.
func copyTree(src, dst string, budget *int64, notes *[]string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir() && d.Name() == ".git":
			return fs.SkipDir
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case !d.Type().IsRegular():
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxBlob {
			*notes = append(*notes, fmt.Sprintf("skipped %s: larger than 95 MB", filepath.ToSlash(rel)))
			return nil
		}
		if *budget -= info.Size(); *budget < 0 {
			return errors.New("fetched files exceed 1 GB")
		}
		return copyFile(p, target, info.Mode().Perm()|0o600)
	})
}

// push records the job's current state as a new commit on its result branch.
func (w *worker) push(j *wjob) error {
	j.mu.Lock()
	res, parent := j.res, j.tip
	j.mu.Unlock()
	if info, err := os.Stat(filepath.Join(j.dir, "log.txt")); err == nil {
		res.LogBytes = info.Size()
	}
	if err := os.WriteFile(filepath.Join(j.dir, "result.json"), mustJSON(res), 0o600); err != nil {
		return err
	}
	env := append(gitEnv(append(w.r.gitConfig(), kv{"core.autocrlf", "false"}, kv{"core.safecrlf", "false"})...),
		"GIT_DIR="+w.repo, "GIT_WORK_TREE="+j.dir, "GIT_INDEX_FILE="+filepath.Join(w.home, "tmp", j.id+".idx"))
	if _, err := gitRun(j.dir, env, "add", "-f", "-A", "--", "."); err != nil {
		return err
	}
	tree, err := gitRun(j.dir, env, "write-tree")
	if err != nil {
		return err
	}
	args := []string{"commit-tree", tree, "-F", "-"}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit, err := gitRunIn(j.dir, env, strings.NewReader(fmt.Sprintf("ucompute result %s %s\n", j.id, res.Status)), args...)
	if err != nil {
		return err
	}
	w.pushMu.Lock()
	_, err = gitRun(j.dir, env, "push", "--quiet", "--force", "--no-verify", w.r.url(), commit+":refs/heads/"+resultRef(w.cfg.Name, j.id))
	w.pushMu.Unlock()
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.tip = commit
	j.mu.Unlock()
	return nil
}

func (w *worker) pushFinal(j *wjob) {
	for i := 0; ; i++ {
		err := w.push(j)
		if err == nil {
			return
		}
		w.log.Printf("job %s: sending the result failed: %v", j.id, err)
		if i == 3 {
			return
		}
		time.Sleep(time.Duration(i+1) * 3 * time.Second)
	}
}

func (w *worker) fail(j *wjob, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	j.update(func(r *jobResult) {
		r.Status, r.Error, r.ExitCode, r.FinishedAt = statusError, msg, exitUcompute, time.Now().UTC()
	})
	w.pushFinal(j)
}

// stopped handles a job whose context ended: the client cancelled it, or
// this worker is shutting down.
func (w *worker) stopped(j *wjob) {
	if j.gone.Load() {
		j.update(func(r *jobResult) { r.Status, r.ExitCode = statusCancelled, exitCancelled })
		return // nobody is listening; finish() removes the result branch
	}
	w.fail(j, "the worker was stopped while the job was running")
}

func (w *worker) finish(j *wjob) {
	j.cancel()
	w.mu.Lock()
	delete(w.active, j.id)
	w.done[j.id] = time.Now()
	w.mu.Unlock()
	w.saveState()
	_ = os.Remove(filepath.Join(w.home, "tmp", j.id+".idx"))
	if j.gone.Load() {
		_ = w.r.deleteRef(resultRef(w.cfg.Name, j.id))
	}
	j.mu.Lock()
	res := j.res
	j.mu.Unlock()
	switch res.Status {
	case statusExited:
		w.log.Printf("job %s: exit %d after %s", j.id, res.ExitCode, roundDur(res.FinishedAt.Sub(res.StartedAt)))
	case statusError:
		w.log.Printf("job %s failed: %s", j.id, res.Error)
	default:
		w.log.Printf("job %s: %s", j.id, res.Status)
	}
}

func (w *worker) shutdown() {
	w.mu.Lock()
	n := len(w.active)
	w.mu.Unlock()
	if n > 0 {
		w.log.Printf("stopping: cancelling %d job(s)", n)
	}
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
	w.sendHeartbeat(true)
	w.saveState()
	w.log.Printf("worker stopped")
}

func (w *worker) sendHeartbeat(stopped bool) {
	w.mu.Lock()
	hb := heartbeat{
		Name: w.cfg.Name, MachineID: w.cfg.MachineID, Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		CPUs: runtime.NumCPU(), MemBytes: totalMemory(), MaxParallel: cap(w.sem), Paused: w.paused, Stopped: stopped,
		Started: w.started.UTC(), Seen: time.Now().UTC(),
	}
	for id, j := range w.active {
		if j.status() == statusRunning {
			hb.Running = append(hb.Running, id)
		} else {
			hb.Queued++
		}
	}
	w.mu.Unlock()
	b, _ := json.Marshal(hb)
	env := append(gitEnv(w.r.gitConfig()...), "GIT_DIR="+w.repo)
	commit, err := gitRunIn("", env, strings.NewReader("ucompute worker "+w.cfg.Name+"\n\n"+string(b)+"\n"), "commit-tree", w.empty, "-F", "-")
	if err == nil {
		w.pushMu.Lock()
		_, err = gitRun("", env, "push", "--quiet", "--force", "--no-verify", w.r.url(), commit+":refs/heads/"+workerRef(w.cfg.Name))
		w.pushMu.Unlock()
	}
	if err != nil {
		w.log.Printf("heartbeat failed: %v", err)
	}
}

// gc deletes this worker's job and result branches older than refRetention,
// and old local job folders.
func (w *worker) gc() {
	cutoff := time.Now().Add(-refRetention)
	for _, prefix := range []string{"job/", "result/"} {
		refs, err := w.r.listRefs(prefix + w.cfg.Name + "/")
		if err != nil {
			continue
		}
		for ref := range refs {
			id := path.Base(ref)
			if t, ok := jobIDTime(id); ok && t.Before(cutoff) && !w.isActive(id) {
				_ = w.r.deleteRef(ref)
			}
		}
	}
	w.mu.Lock()
	for id, t := range w.done {
		if time.Since(t) > 7*24*time.Hour {
			delete(w.done, id)
		}
	}
	w.mu.Unlock()
	w.saveState()
	entries, _ := os.ReadDir(filepath.Join(w.home, "jobs"))
	for _, e := range entries {
		if info, err := e.Info(); err == nil && e.IsDir() && time.Since(info.ModTime()) > 3*24*time.Hour && !w.isActive(e.Name()) {
			_ = os.RemoveAll(filepath.Join(w.home, "jobs", e.Name()))
		}
	}
}

type workerState struct {
	Done map[string]time.Time `json:"done"`
}

func (w *worker) loadState() {
	b, err := os.ReadFile(filepath.Join(w.home, "worker-state.json"))
	if err != nil {
		return
	}
	var s workerState
	if json.Unmarshal(b, &s) == nil && s.Done != nil {
		w.mu.Lock()
		w.done = s.Done
		w.mu.Unlock()
	}
}

func (w *worker) saveState() {
	w.mu.Lock()
	b := mustJSON(workerState{Done: w.done})
	w.mu.Unlock()
	_ = writeFileAtomic(filepath.Join(w.home, "worker-state.json"), b, 0o600)
}

// cappedWriter writes job output to the log file, up to max bytes.
type cappedWriter struct {
	mu        sync.Mutex
	f         *os.File
	n, max    int64
	truncated bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return len(p), nil
	}
	if room := c.max - c.n; int64(len(p)) > room {
		n, _ := c.f.Write(p[:room])
		m, _ := fmt.Fprintf(c.f, "\n[ucompute: output cut off at %d MB]\n", c.max>>20)
		c.n += int64(n + m)
		c.truncated = true
		return len(p), nil
	}
	n, _ := c.f.Write(p)
	c.n += int64(n)
	return len(p), nil // never fail the job because the log couldn't be written
}

func (c *cappedWriter) size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *cappedWriter) wasTruncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}
