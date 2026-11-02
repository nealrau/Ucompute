# Installing Ucompute — instructions for AI coding agents

The user gave you this repository and asked you to install Ucompute. Follow these steps in order and run the commands yourself. Stop and ask the user only where a step says to.

Ucompute lets this machine send commands (tests, CI checks, builds) to another, more powerful machine owned by the same person. Both machines need it installed and set up with the **same GitHub account**.

## 1. Install the binary

macOS, Linux, ChromeOS (Linux terminal):

```bash
curl -fsSL https://raw.githubusercontent.com/nealrau/Ucompute/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/nealrau/Ucompute/main/install.ps1 | iex
```

If the installer says its folder is not on PATH, call `ucompute` by the full path it printed for the rest of this session. Also tell the user which line to add to their shell profile.

## 2. Check the GitHub login

Ucompute signs in with the GitHub CLI's login. It creates nothing else and costs nothing.

```bash
gh auth status
```

- `gh` not installed: install it (macOS `brew install gh`, Debian/Ubuntu/ChromeOS `sudo apt install gh`, Windows `winget install --id GitHub.cli`).
- Not logged in: **ask the user** to run `gh auth login` themselves (it is interactive: GitHub.com, HTTPS, login with a web browser). Wait until they say it's done.
- Logged in, but to a different account than on their other machine: point this out, because both machines must use the same account.

## 3. Pick this machine's role

If the user hasn't said, **ask**: "Is this the powerful machine that should run the jobs, or the machine you work on?"

- Machine that runs the jobs (the "worker"):

  ```bash
  ucompute setup --worker
  ```

- Machine the user works on (the "client"):

  ```bash
  ucompute setup
  ```

`setup` is safe to run again. It creates a private repository `<github-user>/ucompute-relay` if it doesn't exist yet (the two machines exchange jobs through it), turns off GitHub Actions in that repository, and installs a Claude Code skill. With `--worker`, it also installs a background service that starts at login. No administrator rights are needed.

If two worker machines have the same hostname, give each its own name: `ucompute setup --worker --name gaming-pc`.

## 4. Verify

```bash
ucompute status
```

On a client, the worker should be listed as `online`. If the other machine isn't set up yet, tell the user to paste this repository's link to the coding agent on that machine.

Smoke test, once a worker is online:

```bash
ucompute run --no-sync -- "echo hello from \$(hostname)"
```

## 5. Report back

Tell the user:

- what was installed, and where;
- whether a worker is online;
- that from now on they can say things like "run the checks on my PC", and you (or any agent with the skill) will use `ucompute run -- <command>`.

For everyday usage, read [`skills/ucompute/SKILL.md`](skills/ucompute/SKILL.md).
