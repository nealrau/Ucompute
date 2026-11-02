// Command ucompute runs commands on another, more powerful computer. The two
// machines never talk directly: they meet in a private GitHub repository (the
// "relay") that both can reach with the user's existing GitHub login.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const repoSlug = "nealrau/Ucompute"

// Exit codes produced by ucompute itself. Otherwise `ucompute run` exits with
// the remote command's exit code.
const (
	exitTimeout   = 124
	exitUcompute  = 125
	exitCancelled = 130
)

const usageText = `ucompute — run commands on your more powerful computer

Setup (once per machine):
  ucompute setup              set up this machine to send jobs
  ucompute setup --worker     set up this machine to run jobs too (installs a background service)

Everyday:
  ucompute run -- <command>   sync this project to a worker, run <command> there, stream the output
  ucompute status             show workers and whether they are online
  ucompute logs [job-id]      re-attach to a job (default: the most recent one)

Run flags:
  -w, --worker NAME     worker to use (default: any online worker)
  -t, --timeout DUR     stop the job after DUR, e.g. 30m or 600 (default 1h)
  -e, --env KEY=VALUE   set an environment variable for the job (repeatable)
      --include PATH    also upload a git-ignored file such as .env (repeatable)
      --fetch PATH      copy PATH back from the worker when the job ends (repeatable)
      --no-sync         don't upload the project; run in the worker's scratch folder
  -d, --detach          submit, print the job id and exit; follow later with 'ucompute logs'
  -q, --quiet           print only the command's output

Other:
  ucompute worker                            run a worker in the foreground
  ucompute service install|uninstall|status  manage the background worker
  ucompute skill install                     install the Claude Code skill
  ucompute update                            update to the latest release
  ucompute version

Exit codes: the remote command's own code, 124 timeout, 125 ucompute error, 130 cancelled.
Docs: https://github.com/` + repoSlug + `
`

func main() {
	if version == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok && strings.HasPrefix(bi.Main.Version, "v") {
			version = strings.TrimPrefix(bi.Main.Version, "v")
		}
	}
	os.Exit(dispatch(os.Args[1:]))
}

func dispatch(args []string) int {
	if len(args) == 0 {
		fmt.Print(usageText)
		return 0
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run", "exec":
		return cmdRun(rest)
	case "status", "ps":
		return cmdStatus(rest)
	case "logs", "attach", "wait":
		return cmdLogs(rest)
	case "setup", "init":
		return cmdSetup(rest)
	case "worker":
		return cmdWorker(rest)
	case "service":
		return cmdService(rest)
	case "skill":
		return cmdSkill(rest)
	case "update", "upgrade":
		return cmdUpdate(rest)
	case "version", "--version", "-v":
		fmt.Println(version)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usageText)
		return 0
	}
	fmt.Fprintf(os.Stderr, "ucompute: unknown command %q\n\n%s", cmd, usageText)
	return 2
}

// fail prints an error and returns the ucompute error exit code.
func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "ucompute: "+format+"\n", a...)
	return exitUcompute
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	return fs
}

// parseFlags parses fs and maps -h to a clean exit.
func parseFlags(fs *flag.FlagSet, args []string) (exit int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 2, false
	}
	return 0, true
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// durationFlag accepts Go durations ("90s", "1h30m") or plain seconds ("600").
type durationFlag struct{ d *time.Duration }

func (f durationFlag) String() string {
	if f.d == nil {
		return ""
	}
	return f.d.String()
}

func (f durationFlag) Set(v string) error {
	if n, err := strconv.Atoi(v); err == nil {
		*f.d = time.Duration(n) * time.Second
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return errors.New("expected a duration like 30m or 600")
	}
	*f.d = d
	return nil
}
