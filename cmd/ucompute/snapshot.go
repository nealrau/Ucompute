package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxBlob is the largest file we try to push; GitHub rejects files over 100 MB.
const maxBlob = 95 << 20

// Folders skipped when snapshotting a folder that isn't a git repository
// (inside a repository, .gitignore decides).
var defaultExcludes = []string{"node_modules/", ".venv/", "venv/", "__pycache__/", ".tox/", ".next/", "target/", ".DS_Store"}

// snapshot is the exact state of a working tree — committed, modified and
// untracked files, minus ignored ones — recorded as a commit on top of HEAD,
// without touching the user's index, branches or stash.
type snapshot struct {
	root    string // project root on this machine
	prefix  string // current directory relative to root, slash-separated
	project string // workspace name on the worker
	gitDir  string // private git dir when root is not a git repository
	changed int    // files that differ from HEAD (or from the last snapshot)
	commit  string
}

func (s *snapshot) env(extra ...kv) []string {
	env := gitEnv(extra...)
	if s.gitDir != "" {
		env = append(env, "GIT_DIR="+s.gitDir, "GIT_WORK_TREE="+s.root)
	}
	return env
}

// takeSnapshot records cwd's project as a commit whose message is spec.
func takeSnapshot(home, cwd string, spec *jobSpec, includes []string) (*snapshot, error) {
	s := &snapshot{}
	if out, err := gitRun(cwd, nil, "rev-parse", "--show-toplevel", "--show-prefix"); err == nil {
		lines := strings.SplitN(out, "\n", 2)
		s.root = filepath.FromSlash(lines[0])
		if len(lines) == 2 {
			s.prefix = strings.TrimSuffix(strings.TrimSpace(lines[1]), "/")
		}
		origin, _ := gitRun(s.root, nil, "config", "--get", "remote.origin.url")
		s.project = projectID(s.root, origin)
	} else if strings.Contains(err.Error(), "not a git repository") {
		abs, err := filepath.Abs(cwd)
		if err != nil {
			return nil, err
		}
		s.root = abs
		s.project = projectID(abs, "")
		s.gitDir = filepath.Join(home, "snapshots", s.project+".git")
		if err := initSnapshotRepo(s.gitDir); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	spec.Project, spec.Dir = s.project, s.prefix

	// Stage everything into a throwaway index. Starting from a copy of the
	// real index lets git skip re-hashing files that haven't changed.
	index := filepath.Join(home, "tmp", spec.ID+".index")
	if err := os.MkdirAll(filepath.Dir(index), 0o700); err != nil {
		return nil, err
	}
	defer os.Remove(index)
	var parent, seed string
	if s.gitDir == "" {
		parent, _ = gitRun(s.root, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
		if p, err := gitRun(s.root, nil, "rev-parse", "--git-path", "index"); err == nil {
			if !filepath.IsAbs(p) {
				p = filepath.Join(s.root, p)
			}
			seed = p
		}
	} else {
		parent, _ = gitRun(s.root, s.env(), "rev-parse", "--verify", "-q", "refs/heads/last^{commit}")
		seed = filepath.Join(s.gitDir, "index")
	}
	env := append(s.env(), "GIT_INDEX_FILE="+index)
	add := func() error {
		_, err := gitRun(s.root, env, "add", "-A", "--", ".")
		return err
	}
	if seed != "" && copyFile(seed, index, 0o600) == nil {
		if err := add(); err == nil {
			seed = "ok"
		}
	}
	if seed != "ok" { // no usable index to start from (or an exotic one): start empty
		os.Remove(index)
		if err := add(); err != nil {
			return nil, err
		}
	}
	if len(includes) > 0 {
		dir := cwd
		if s.gitDir != "" {
			dir = s.root
		}
		if _, err := gitRun(dir, env, append([]string{"add", "-f", "--"}, includes...)...); err != nil {
			return nil, fmt.Errorf("--include: %w", err)
		}
	}
	tree, err := gitRun(s.root, env, "write-tree")
	if err != nil {
		return nil, err
	}
	if err := s.inspect(env, parent, tree); err != nil {
		return nil, err
	}
	args := []string{"commit-tree", tree, "-F", "-"}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	if s.commit, err = gitRunIn(s.root, s.env(), strings.NewReader(spec.message()), args...); err != nil {
		return nil, err
	}
	if s.gitDir != "" {
		_ = copyFile(index, filepath.Join(s.gitDir, "index"), 0o600)
	}
	return s, nil
}

// inspect counts changed files and refuses files GitHub would reject.
func (s *snapshot) inspect(env []string, parent, tree string) error {
	var shas, paths []string
	if parent != "" {
		out, err := gitOut(s.root, env, nil, "diff-tree", "-r", "-z", "--no-renames", parent, tree)
		if err != nil {
			return err
		}
		f := strings.Split(string(out), "\x00")
		for i := 0; i+1 < len(f); i++ {
			if !strings.HasPrefix(f[i], ":") {
				continue
			}
			meta := strings.Fields(f[i]) // ":100644 100644 <old> <new> M"
			path := f[i+1]
			i++
			s.changed++
			if len(meta) == 5 && meta[4] != "D" && meta[1] != "160000" {
				shas, paths = append(shas, meta[3]), append(paths, path)
			}
		}
	} else {
		out, err := gitOut(s.root, env, nil, "ls-tree", "-r", "-z", tree)
		if err != nil {
			return err
		}
		for _, rec := range strings.Split(string(out), "\x00") {
			meta, path, ok := strings.Cut(rec, "\t")
			if f := strings.Fields(meta); ok && len(f) == 3 && f[1] == "blob" {
				s.changed++
				shas, paths = append(shas, f[2]), append(paths, path)
			}
		}
	}
	if len(shas) == 0 {
		return nil
	}
	out, err := gitRunIn(s.root, env, strings.NewReader(strings.Join(shas, "\n")+"\n"), "cat-file", "--batch-check=%(objectsize)")
	if err != nil {
		return err
	}
	var big []string
	for i, line := range strings.Split(out, "\n") {
		if i >= len(paths) {
			break
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64); err == nil && n > maxBlob {
			big = append(big, fmt.Sprintf("%s (%d MB)", paths[i], n>>20))
		}
	}
	if len(big) > 0 {
		return fmt.Errorf("too big to sync (95 MB per file limit): %s — add them to .gitignore", strings.Join(big, ", "))
	}
	return nil
}

// push uploads the snapshot as job branch ref. Only objects the relay doesn't
// have yet travel, thanks to the per-project base branch.
func (s *snapshot) push(r relay, ref string) error {
	_, err := gitRun(s.root, s.env(r.gitConfig()...), "push", "--quiet", "--no-verify", "--no-signed",
		"--no-follow-tags", "--recurse-submodules=no", r.url(),
		s.commit+":refs/heads/"+ref, "+"+s.commit+":refs/heads/"+baseRef(s.project))
	if err != nil {
		return err
	}
	if s.gitDir != "" {
		_, _ = gitRun(s.root, s.env(), "update-ref", "refs/heads/last", s.commit)
	}
	return nil
}

func initSnapshotRepo(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return nil
	}
	if _, err := gitRun("", nil, "init", "--bare", "--quiet", dir); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "info", "exclude"), []byte(strings.Join(defaultExcludes, "\n")+"\n"), 0o644)
}
