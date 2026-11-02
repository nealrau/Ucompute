package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
)

// relay is where clients and workers meet. Code and results travel as git
// pushes and fetches; the API calls below only watch and tidy up branches.
type relay interface {
	describe() string
	url() string     // git remote for push/fetch
	gitConfig() []kv // config git needs to talk to url()
	listRefs(prefix string) (map[string]string, error)
	getRef(branch string) (sha string, ok bool, err error)
	deleteRef(branch string) error
	commitMessage(sha string) (string, error)
}

func isLocalRelay(s string) bool { return strings.HasPrefix(s, "file://") || filepath.IsAbs(s) }

func openRelay(cfg *Config) (relay, error) {
	if isLocalRelay(cfg.Relay) {
		return &localRelay{dir: strings.TrimPrefix(cfg.Relay, "file://")}, nil
	}
	if strings.Count(cfg.Relay, "/") != 1 {
		return nil, fmt.Errorf("relay %q should look like owner/repo", cfg.Relay)
	}
	tok, err := resolveToken()
	if err != nil {
		return nil, err
	}
	return &githubRelay{repo: cfg.Relay, token: tok, gh: newGH(tok)}, nil
}

// relayAlias is the remote URL git sees. It is rewritten to the real https
// URL by our own insteadOf rule, which also stops a user's global
// "https://github.com/ -> git@github.com:" rewrite from hijacking it (git
// applies only one rewrite).
const relayAlias = "ucompute-relay:"

type githubRelay struct {
	repo  string
	token string
	gh    *ghClient
	msgs  sync.Map // commit sha -> message; commits never change
}

func (r *githubRelay) describe() string { return "github.com/" + r.repo }
func (r *githubRelay) url() string      { return relayAlias }

func (r *githubRelay) gitConfig() []kv {
	auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + r.token))
	return []kv{
		{"url.https://github.com/" + r.repo + ".git.insteadOf", relayAlias},
		{"http.https://github.com/.extraHeader", "Authorization: Basic " + auth},
		{"credential.helper", ""},
	}
}

type ghRef struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

func (r *githubRelay) listRefs(prefix string) (map[string]string, error) {
	body, _, err := r.gh.call(http.MethodGet, "/repos/"+r.repo+"/git/matching-refs/heads/"+strings.TrimSuffix(prefix, "/"), nil, true)
	if isStatus(err, http.StatusConflict) { // empty repository
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var refs []ghRef
	if err := json.Unmarshal(body, &refs); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(refs))
	for _, ref := range refs {
		if name := strings.TrimPrefix(ref.Ref, "refs/heads/"); strings.HasPrefix(name, prefix) {
			out[name] = ref.Object.SHA
		}
	}
	return out, nil
}

func (r *githubRelay) getRef(branch string) (string, bool, error) {
	body, _, err := r.gh.call(http.MethodGet, "/repos/"+r.repo+"/git/ref/heads/"+branch, nil, true)
	if isStatus(err, http.StatusNotFound) || isStatus(err, http.StatusConflict) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var ref ghRef
	if err := json.Unmarshal(body, &ref); err != nil {
		return "", false, err
	}
	return ref.Object.SHA, ref.Object.SHA != "", nil
}

func (r *githubRelay) deleteRef(branch string) error {
	_, _, err := r.gh.call(http.MethodDelete, "/repos/"+r.repo+"/git/refs/heads/"+branch, nil, false)
	if isStatus(err, http.StatusUnprocessableEntity) || isStatus(err, http.StatusNotFound) {
		return nil // already gone
	}
	return err
}

func (r *githubRelay) commitMessage(sha string) (string, error) {
	if m, ok := r.msgs.Load(sha); ok {
		return m.(string), nil
	}
	var c struct {
		Message string `json:"message"`
	}
	if err := r.gh.getJSON("/repos/"+r.repo+"/git/commits/"+sha, &c); err != nil {
		return "", err
	}
	r.msgs.Store(sha, c.Message)
	return c.Message, nil
}

// localRelay is a bare git repository on disk.
type localRelay struct{ dir string }

func (r *localRelay) describe() string { return r.dir }
func (r *localRelay) url() string      { return r.dir }
func (r *localRelay) gitConfig() []kv  { return nil }

func (r *localRelay) listRefs(prefix string) (map[string]string, error) {
	out, err := gitRun("", nil, "--git-dir", r.dir, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(line, " ")
		if name := strings.TrimPrefix(ref, "refs/heads/"); ok && strings.HasPrefix(name, prefix) {
			refs[name] = sha
		}
	}
	return refs, nil
}

func (r *localRelay) getRef(branch string) (string, bool, error) {
	sha, err := gitRun("", nil, "--git-dir", r.dir, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil || sha == "" {
		return "", false, nil
	}
	return sha, true, nil
}

func (r *localRelay) deleteRef(branch string) error {
	_, err := gitRun("", nil, "--git-dir", r.dir, "update-ref", "-d", "refs/heads/"+branch)
	return err
}

func (r *localRelay) commitMessage(sha string) (string, error) {
	out, err := gitRun("", nil, "--git-dir", r.dir, "cat-file", "commit", sha)
	if err != nil {
		return "", err
	}
	_, msg, _ := strings.Cut(out, "\n\n")
	return msg, nil
}
