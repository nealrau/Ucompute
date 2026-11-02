package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// cmdUpdate replaces this binary with the latest GitHub release and restarts
// the background worker if there is one.
func cmdUpdate(args []string) int {
	exe, err := os.Executable()
	if err != nil {
		return fail("%v", err)
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	latest, err := latestVersion()
	if err != nil {
		return fail("checking for updates: %v", err)
	}
	if latest == version {
		fmt.Printf("ucompute %s is the latest version\n", version)
		return 0
	}
	asset := "ucompute_" + runtime.GOOS + "_" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	base := "https://github.com/" + repoSlug + "/releases/download/v" + latest + "/"
	bin, err := download(base + asset)
	if err != nil {
		return fail("downloading %s: %v", asset, err)
	}
	sums, err := download(base + "checksums.txt")
	if err != nil {
		return fail("downloading checksums: %v", err)
	}
	sum := sha256.Sum256(bin)
	if !strings.Contains(string(sums), hex.EncodeToString(sum[:])+"  "+asset) {
		return fail("checksum mismatch for %s; not updating", asset)
	}
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return fail("%v (try reinstalling: %s)", err, installOneLiner)
	}
	if out, err := exec.Command(tmp, "version").Output(); err != nil || strings.TrimSpace(string(out)) == "" {
		os.Remove(tmp)
		return fail("the downloaded binary doesn't run on this machine")
	}
	if runtime.GOOS == "windows" { // a running .exe can be renamed but not overwritten
		_ = os.Remove(exe + ".old")
		if err := os.Rename(exe, exe+".old"); err != nil {
			return fail("%v", err)
		}
	}
	if err := os.Rename(tmp, exe); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("ucompute updated %s → %s\n", version, latest)
	if serviceInstalled() {
		if err := installService(); err != nil {
			return fail("restarting the background worker: %v", err)
		}
		fmt.Println("background worker restarted")
	}
	home, _ := os.UserHomeDir()
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "ucompute", "SKILL.md")); err == nil {
		_ = exec.Command(exe, "skill", "install").Run() // refresh it with the new version's skill
	}
	return 0
}

func latestVersion() (string, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+repoSlug+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok, err := resolveToken(); err == nil {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	return strings.TrimPrefix(rel.Tag, "v"), nil
}

func download(url string) ([]byte, error) {
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
