package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The background worker is a per-user service: a launchd agent on macOS, a
// systemd user unit on Linux (ChromeOS included), and a hidden startup script
// on Windows. None of them need administrator rights.

const serviceLabel = "dev.ucompute.worker"

func cmdService(args []string) int {
	if len(args) == 0 {
		return fail("usage: ucompute service install|uninstall|restart|status")
	}
	switch args[0] {
	case "install", "start", "restart":
		if err := installService(); err != nil {
			return fail("%v", err)
		}
		fmt.Println("ucompute: background worker installed and started (log: " + workerLogPath() + ")")
	case "uninstall", "stop", "remove":
		if err := uninstallService(); err != nil {
			return fail("%v", err)
		}
		fmt.Println("ucompute: background worker removed")
	case "status":
		fmt.Println(serviceStatus())
		fmt.Println("log: " + workerLogPath())
	default:
		return fail("usage: ucompute service install|uninstall|restart|status")
	}
	return 0
}

func workerLogPath() string { return filepath.Join(ucHome(), "worker.log") }

func workerExe() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if strings.Contains(p, "go-build") {
		return "", errors.New("install ucompute first; a `go run` binary can't be used as a service")
	}
	return p, nil
}

// serviceEnv is the environment the service runs with. PATH is captured now,
// from the user's shell, so jobs find the same node/python/cargo as a terminal.
func serviceEnv() map[string]string {
	env := map[string]string{"PATH": os.Getenv("PATH")}
	for _, k := range []string{"UCOMPUTE_HOME", "GH_CONFIG_DIR", "LANG"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func installService() error {
	if _, err := loadConfig(); err != nil {
		return err
	}
	exe, err := workerExe()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(exe)
	case "linux":
		return installSystemd(exe)
	case "windows":
		return installWindows(exe)
	}
	return startDetached(exe, "no background service support on "+runtime.GOOS)
}

func uninstallService() error {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("launchctl", "bootout", launchdDomain()+"/"+serviceLabel).Run()
		_ = os.Remove(launchdPlist())
	case "linux":
		if _, err := os.Stat(systemdUnitPath()); err == nil {
			_ = exec.Command("systemctl", "--user", "disable", "--now", "ucompute.service").Run()
			_ = os.Remove(systemdUnitPath())
			_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		}
	case "windows":
		_ = os.Remove(windowsStartupScript())
	}
	stopPidFile()
	return nil
}

func serviceInstalled() bool {
	var p string
	switch runtime.GOOS {
	case "darwin":
		p = launchdPlist()
	case "linux":
		p = systemdUnitPath()
	case "windows":
		p = windowsStartupScript()
	default:
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func serviceStatus() string {
	running := workerPid() > 0
	switch {
	case !serviceInstalled() && running:
		return "not installed (a worker is running in the background)"
	case !serviceInstalled():
		return "not installed (this machine only sends jobs)"
	case running:
		return "installed, running"
	}
	return "installed, NOT running — see " + workerLogPath()
}

// workerPid returns the pid of this machine's running worker, or 0.
func workerPid() int {
	b, err := os.ReadFile(filepath.Join(ucHome(), "worker.pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if !processAlive(pid) {
		return 0
	}
	return pid
}

func stopPidFile() {
	if pid := workerPid(); pid > 0 && pid != os.Getpid() {
		stopProcess(pid)
		for i := 0; i < 50 && processAlive(pid); i++ {
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// startDetached starts a worker that lives on after this command exits but
// won't come back after a reboot.
func startDetached(exe, why string) error {
	stopPidFile()
	cmd := exec.Command(exe, "worker", "--log", workerLogPath())
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	fmt.Fprintf(os.Stderr, "ucompute: %s, so the worker was started in the background instead.\n"+
		"          It won't restart after a reboot; run `ucompute service install` again then.\n", why)
	return nil
}

// ---- macOS ----

func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchdPlist() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
}

func installLaunchd(exe string) error {
	env := serviceEnv()
	var envXML strings.Builder
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&envXML, "\t\t<key>%s</key><string>%s</string>\n", xmlEscape(k), xmlEscape(env[k]))
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>worker</string>
		<string>--log</string>
		<string>%s</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
%s	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>10</integer>
</dict>
</plist>
`, serviceLabel, xmlEscape(exe), xmlEscape(workerLogPath()), envXML.String())
	p := launchdPlist()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", launchdDomain()+"/"+serviceLabel).Run()
	var last error
	for i := 0; i < 10; i++ { // bootout finishes asynchronously
		out, err := exec.Command("launchctl", "bootstrap", launchdDomain(), p).CombinedOutput()
		if err == nil {
			return nil
		}
		last = fmt.Errorf("launchctl bootstrap: %s", strings.TrimSpace(string(out)))
		time.Sleep(time.Second)
	}
	return last
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// ---- Linux ----

func systemdUnitPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", "ucompute.service")
}

func installSystemd(exe string) error {
	if err := exec.Command("systemctl", "--user", "show-environment").Run(); err != nil {
		return startDetached(exe, "systemd user services are not available here")
	}
	env := serviceEnv()
	var lines strings.Builder
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(&lines, "Environment=%s\n", systemdQuote(k+"="+env[k]))
	}
	unit := fmt.Sprintf(`[Unit]
Description=Ucompute worker (runs jobs sent from your other machines)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s worker --log %s
Restart=always
RestartSec=10
%s
[Install]
WantedBy=default.target
`, systemdQuote(exe), systemdQuote(workerLogPath()), lines.String())
	p := systemdUnitPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "ucompute.service"},
		{"--user", "restart", "ucompute.service"},
	} {
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
		}
	}
	// Keep the worker running when nobody is logged in (best effort).
	if u, err := user.Current(); err == nil {
		_ = exec.Command("loginctl", "enable-linger", u.Username).Run()
	}
	return nil
}

func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")
	return `"` + r.Replace(s) + `"`
}

// ---- Windows ----

func windowsStartupScript() string {
	return filepath.Join(os.Getenv("APPDATA"), "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "ucompute-worker.vbs")
}

func installWindows(exe string) error {
	stopPidFile()
	q := func(s string) string { return `""` + s + `""` } // a quote inside a VBScript string is doubled
	vbs := "' Starts the Ucompute worker hidden when you log in. Remove with: ucompute service uninstall\r\n" +
		`CreateObject("WScript.Shell").Run "` + q(exe) + ` worker --log ` + q(workerLogPath()) + `", 0, False` + "\r\n"
	p := windowsStartupScript()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(vbs), 0o644); err != nil {
		return err
	}
	return exec.Command("wscript.exe", p).Start()
}
