package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	ucompute "github.com/nealrau/Ucompute"
)

const installOneLiner = "curl -fsSL https://raw.githubusercontent.com/" + repoSlug + "/main/install.sh | sh"

func cmdSetup(args []string) int {
	fs := newFlagSet("setup")
	var asWorker, noSkill, storeToken bool
	var name, relayFlag, shell string
	var parallel int
	fs.BoolVar(&asWorker, "worker", false, "")
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&relayFlag, "relay", "", "")
	fs.StringVar(&shell, "shell", "", "")
	fs.IntVar(&parallel, "max-parallel", 0, "")
	fs.BoolVar(&noSkill, "no-skill", false, "")
	fs.BoolVar(&storeToken, "store-token", false, "")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	ok := func(format string, a ...any) { fmt.Printf("✓ "+format+"\n", a...) }

	gv, err := gitVersion()
	if err != nil {
		return fail("git is required but was not found: %v", err)
	}
	if !gitAtLeast(gv, 2, 31) {
		return fail("git %s is too old; ucompute needs git 2.31 or newer", gv)
	}
	ok("git %s", gv)

	cfg, err := loadConfig()
	if errors.Is(err, errNotSetUp) {
		cfg = &Config{}
	} else if err != nil {
		return fail("%v", err)
	}
	if name != "" {
		if cfg.Name = sanitizeName(name); cfg.Name == "" {
			return fail("--name %q has no usable characters", name)
		}
	}
	if parallel > 0 {
		cfg.MaxParallel = parallel
	}
	if shell != "" {
		cfg.Shell = shell
	}
	if relayFlag != "" {
		cfg.Relay = relayFlag
	}
	cfg.fill()

	if !isLocalRelay(cfg.Relay) {
		tok, err := resolveToken()
		if err != nil {
			return fail("%v", err)
		}
		gh := newGH(tok)
		login, err := gh.user()
		if err != nil {
			return fail("checking your GitHub login: %v", err)
		}
		ok("GitHub account %s", login)
		if cfg.Relay == "" {
			cfg.Relay = login + "/ucompute-relay"
		}
		info, err := gh.repo(cfg.Relay)
		switch {
		case isStatus(err, 404):
			owner, repoName, _ := strings.Cut(cfg.Relay, "/")
			if !strings.EqualFold(owner, login) {
				return fail("relay repo %s does not exist, and ucompute can only create repos under your own account (%s)", cfg.Relay, login)
			}
			if _, err := gh.createRepo(repoName); err != nil {
				return fail("creating the relay repo: %v", err)
			}
			ok("created private relay repo github.com/%s", cfg.Relay)
		case err != nil:
			return fail("checking the relay repo: %v", err)
		case !info.Private:
			return fail("relay repo github.com/%s is PUBLIC. Make it private first:\n  gh repo edit %s --visibility private --accept-visibility-change-consequences", cfg.Relay, cfg.Relay)
		default:
			ok("relay repo github.com/%s (private)", cfg.Relay)
		}
		if err := gh.hardenRepo(cfg.Relay); err != nil {
			fmt.Printf("! could not turn off GitHub Actions on the relay (%v)\n  Turn it off by hand so synced workflows don't run there: https://github.com/%s/settings/actions\n", err, cfg.Relay)
		}
		if storeToken {
			if err := writeFileAtomic(filepath.Join(ucHome(), "token"), []byte(tok+"\n"), 0o600); err != nil {
				return fail("saving the token: %v", err)
			}
			ok("saved the GitHub token for the background worker (%s)", filepath.Join(ucHome(), "token"))
		}
	}
	if err := cfg.save(); err != nil {
		return fail("saving config: %v", err)
	}
	ok("this machine is %q (config: %s)", cfg.Name, configPath())

	if !noSkill {
		if p, err := installSkill(); err != nil {
			fmt.Printf("! could not install the Claude Code skill: %v\n", err)
		} else {
			ok("Claude Code skill installed (%s)", p)
		}
	}

	if !asWorker {
		fmt.Printf(`
Next:
  1. On the machine that should do the work, install ucompute and run:
       %s
       ucompute setup --worker
  2. Back here:  ucompute status          (the worker should show as online)
                 ucompute run -- npm test (or any command, from your project folder)
`, installOneLiner)
		return 0
	}

	if err := installService(); err != nil {
		return fail("installing the background worker: %v", err)
	}
	r, err := openRelay(cfg)
	if err != nil {
		return fail("%v", err)
	}
	if waitForWorker(r, cfg, 45*time.Second) {
		ok("worker %q is online and waiting for jobs", cfg.Name)
	} else {
		fmt.Printf("! the worker hasn't checked in yet. Look at its log: %s\n", workerLogPath())
	}
	fmt.Printf(`
This machine now runs jobs in the background (it starts again after a reboot).
Keep it awake and online while you want to use it.

On the machine you work on:
  %s
  ucompute setup
  ucompute run -- <command>
`, installOneLiner)
	return 0
}

func waitForWorker(r relay, cfg *Config, timeout time.Duration) bool {
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		sha, found, err := r.getRef(workerRef(cfg.Name))
		if err != nil || !found {
			continue
		}
		msg, err := r.commitMessage(sha)
		if err != nil {
			continue
		}
		if hb, err := parseHeartbeat(msg); err == nil && hb.MachineID == cfg.MachineID && !hb.Stopped && time.Since(hb.Seen) < time.Minute {
			return true
		}
	}
	return false
}

func installSkill() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".claude", "skills", "ucompute")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "SKILL.md")
	return p, os.WriteFile(p, []byte(ucompute.Skill), 0o644)
}

func cmdSkill(args []string) int {
	switch {
	case len(args) == 1 && args[0] == "install":
		p, err := installSkill()
		if err != nil {
			return fail("%v", err)
		}
		fmt.Println("ucompute: Claude Code skill installed at " + p)
		return 0
	case len(args) == 1 && args[0] == "print":
		fmt.Print(ucompute.Skill)
		return 0
	}
	return fail("usage: ucompute skill install|print")
}
