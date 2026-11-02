package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// kv is a git config key/value pair.
type kv [2]string

// gitEnv builds the environment for every git command ucompute runs. Config
// goes through GIT_CONFIG_* variables instead of -c flags so tokens never show
// up in process listings, and so it wins over repo and user config: no hooks,
// no signing prompts, no credential helpers popping up dialogs.
func gitEnv(extra ...kv) []string {
	n := 0
	env := make([]string, 0, len(os.Environ())+2*len(extra)+16)
	for _, e := range os.Environ() {
		k, v, _ := strings.Cut(e, "=")
		switch k {
		case "GIT_CONFIG_COUNT":
			n, _ = strconv.Atoi(v)
			continue
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_PREFIX":
			continue
		}
		env = append(env, e)
	}
	cfg := append([]kv{
		{"core.hooksPath", filepath.Join(ucHome(), "no-hooks")},
		{"commit.gpgSign", "false"},
		{"advice.detachedHead", "false"},
	}, extra...)
	for i, p := range cfg {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n+i, p[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n+i, p[1]))
	}
	return append(env,
		"GIT_CONFIG_COUNT="+strconv.Itoa(n+len(cfg)),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_AUTHOR_NAME=ucompute", "GIT_AUTHOR_EMAIL=ucompute@localhost",
		"GIT_COMMITTER_NAME=ucompute", "GIT_COMMITTER_EMAIL=ucompute@localhost",
	)
}

// gitRun runs git in dir and returns stdout without the trailing newline.
// A nil env means gitEnv().
func gitRun(dir string, env []string, args ...string) (string, error) {
	return gitRunIn(dir, env, nil, args...)
}

func gitRunIn(dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	out, err := gitOut(dir, env, stdin, args...)
	return strings.TrimRight(string(out), "\r\n"), err
}

// gitOut runs git and returns its raw stdout.
func gitOut(dir string, env []string, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if env == nil {
		env = gitEnv()
	}
	cmd.Env = env
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %s", gitSubcommand(args), msg)
	}
	return stdout.Bytes(), nil
}

func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--git-dir" || a == "-C" || a == "-c":
			i++
		case !strings.HasPrefix(a, "-"):
			return a
		}
	}
	return ""
}

// gitVersion returns git's version as "2.43.0".
func gitVersion() (string, error) {
	out, err := gitRun("", nil, "version")
	if err != nil {
		return "", err
	}
	f := strings.Fields(out) // "git version 2.43.0 (Apple Git-..)"
	if len(f) < 3 {
		return "", fmt.Errorf("unexpected `git version` output %q", out)
	}
	return f[2], nil
}

// gitAtLeast reports whether version v is at least major.minor.
func gitAtLeast(v string, major, minor int) bool {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return false
	}
	ma, _ := strconv.Atoi(parts[0])
	mi, _ := strconv.Atoi(parts[1])
	return ma > major || (ma == major && mi >= minor)
}
