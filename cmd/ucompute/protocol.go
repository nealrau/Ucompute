package main

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Relay layout (all plain branches in the relay repo):
//
//	job/<worker>/<id>     a job: commit = project snapshot, message = jobSpec JSON
//	result/<worker>/<id>  the job's progress: chain of commits holding
//	                      result.json, log.txt and artifacts/
//	worker/<name>         worker heartbeat: empty commit, message = heartbeat JSON
//	base/<project>        latest snapshot per project, so the next upload
//	                      only sends what changed
const protocolVersion = 1

func jobRef(worker, id string) string    { return "job/" + worker + "/" + id }
func resultRef(worker, id string) string { return "result/" + worker + "/" + id }
func workerRef(name string) string       { return "worker/" + name }
func baseRef(project string) string      { return "base/" + project }

const (
	statusQueued    = "queued"
	statusRunning   = "running"
	statusExited    = "exited"
	statusTimeout   = "timeout"
	statusCancelled = "cancelled"
	statusError     = "error"
)

func isFinal(status string) bool {
	switch status {
	case statusExited, statusTimeout, statusCancelled, statusError:
		return true
	}
	return false
}

type jobSpec struct {
	V          int               `json:"v"`
	ID         string            `json:"id"`
	Command    string            `json:"command"`
	Sync       bool              `json:"sync"`              // run inside the synced project
	Project    string            `json:"project,omitempty"` // worker workspace name
	Dir        string            `json:"dir,omitempty"`     // working dir relative to the project root
	Env        map[string]string `json:"env,omitempty"`
	TimeoutSec int               `json:"timeout_sec,omitempty"`
	Fetch      []string          `json:"fetch,omitempty"` // paths to send back, relative to Dir
	Client     string            `json:"client,omitempty"`
	Created    time.Time         `json:"created"`
}

func (s *jobSpec) message() string {
	b, _ := json.MarshalIndent(s, "", "  ")
	return "ucompute job " + s.ID + "\n\n" + string(b) + "\n"
}

func parseJobSpec(msg string) (*jobSpec, error) {
	i := strings.IndexByte(msg, '{')
	if i < 0 {
		return nil, errors.New("no job description in the commit message")
	}
	var s jobSpec
	if err := json.Unmarshal([]byte(strings.TrimSpace(msg[i:])), &s); err != nil {
		return nil, err
	}
	if s.ID == "" || s.Command == "" {
		return nil, errors.New("job description is incomplete")
	}
	return &s, nil
}

type jobResult struct {
	ID         string    `json:"id"`
	Worker     string    `json:"worker"`
	Status     string    `json:"status"`
	ExitCode   int       `json:"exit_code"`
	Error      string    `json:"error,omitempty"`
	Waiting    string    `json:"waiting,omitempty"` // why a queued job hasn't started
	Notes      []string  `json:"notes,omitempty"`
	Artifacts  []string  `json:"artifacts,omitempty"`
	LogBytes   int64     `json:"log_bytes"`
	QueuedAt   time.Time `json:"queued_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

func (r *jobResult) final() bool { return isFinal(r.Status) }

type heartbeat struct {
	Name        string    `json:"name"`
	MachineID   string    `json:"machine_id"`
	Version     string    `json:"version"`
	OS          string    `json:"os"`
	Arch        string    `json:"arch"`
	CPUs        int       `json:"cpus"`
	MemBytes    uint64    `json:"mem_bytes,omitempty"`
	MaxParallel int       `json:"max_parallel"`
	Running     []string  `json:"running,omitempty"`
	Queued      int       `json:"queued,omitempty"`
	Paused      string    `json:"paused,omitempty"`
	Stopped     bool      `json:"stopped,omitempty"`
	Started     time.Time `json:"started"`
	Seen        time.Time `json:"seen"`
}

func parseHeartbeat(msg string) (*heartbeat, error) {
	i := strings.IndexByte(msg, '{')
	if i < 0 {
		return nil, errors.New("not a heartbeat")
	}
	var hb heartbeat
	if err := json.Unmarshal([]byte(strings.TrimSpace(msg[i:])), &hb); err != nil {
		return nil, err
	}
	return &hb, nil
}

const jobIDTimeLayout = "060102-150405"

// newJobID returns a sortable id such as 261101-193502-3fa9c1.
func newJobID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format(jobIDTimeLayout) + "-" + hex.EncodeToString(b[:])
}

func jobIDTime(id string) (time.Time, bool) {
	if len(id) < len(jobIDTimeLayout) {
		return time.Time{}, false
	}
	t, err := time.Parse(jobIDTimeLayout, id[:len(jobIDTimeLayout)])
	return t, err == nil
}

var unsafeName = regexp.MustCompile(`[^a-z0-9._-]+`)

// sanitizeName turns s into something safe for a git branch name component.
func sanitizeName(s string) string {
	s = unsafeName.ReplaceAllString(strings.ToLower(s), "-")
	for strings.Contains(s, "..") {
		s = strings.ReplaceAll(s, "..", ".")
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return strings.Trim(strings.TrimSuffix(strings.Trim(s, "-."), ".lock"), "-.")
}

// projectID names the worker-side workspace for a project. Clones of the same
// GitHub repo share one workspace (and its installed dependencies).
func projectID(root, origin string) string {
	if origin != "" {
		u := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(origin), "/"), ".git")
		parts := strings.FieldsFunc(u, func(r rune) bool { return r == '/' || r == ':' || r == '\\' })
		if len(parts) >= 2 {
			if id := sanitizeName(parts[len(parts)-2] + "-" + parts[len(parts)-1]); id != "" {
				return id
			}
		}
	}
	h := sha1.Sum([]byte(root))
	base := sanitizeName(filepath.Base(root))
	if base == "" {
		base = "project"
	}
	return base + "-" + hex.EncodeToString(h[:4])
}
