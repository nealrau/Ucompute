# Ucompute

**Run your heavy commands on your powerful computer, from anywhere.**

You code on a Chromebook or a laptop that overheats. At home there's a desktop with plenty of cores. With Ucompute, you (or Claude, or any coding agent) type

```bash
ucompute run -- npm test
```

on the laptop. Your project, uncommitted edits included, goes to the desktop. The tests run there, and the output streams back to the laptop as if the command had run locally. The exit code comes back too.

- **Free.** It runs on your existing GitHub account. No servers, no subscriptions, no port forwarding or VPN.
- **No setup screens.** Paste this repo's link into your coding agent and say "install this". The agent follows [AGENTS.md](AGENTS.md).
- **Built for agents.** It ships a Claude Code skill, so Claude knows when and how to send work to your other machine. Any agent that can run shell commands can use it.
- **Open source** (MIT). One small Go binary for macOS, Linux/ChromeOS and Windows.

## Install

### Let your agent do it

Paste this into Claude Code (or Codex, Cursor, ...) **on each machine**:

> Install Ucompute: https://github.com/nealrau/Ucompute

The agent installs the binary, checks your GitHub login, and asks whether the machine is the one that runs the jobs.

### Or by hand

1. Install the binary.

   macOS / Linux / ChromeOS:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/nealrau/Ucompute/main/install.sh | sh
   ```

   Windows (PowerShell):

   ```powershell
   irm https://raw.githubusercontent.com/nealrau/Ucompute/main/install.ps1 | iex
   ```

2. Make sure the [GitHub CLI](https://cli.github.com) is logged in: `gh auth login`. Use the same account on both machines.
3. On the **powerful machine**: `ucompute setup --worker`
4. On the **machine you work on**: `ucompute setup`
5. Check: `ucompute status`

## Use it

```bash
ucompute status                                   # which workers are online
ucompute run -- npm test                          # run in the current project
ucompute run -- "npm run lint && npm test"        # several steps, one shell line
ucompute run -e CI=true -- pytest -x              # with environment variables
ucompute run --include .env -- npm run e2e        # also upload a git-ignored file
ucompute run --fetch coverage/ -- npm run cov     # bring files back afterwards
ucompute run --no-sync -- nvidia-smi              # run without uploading a project
ucompute run --detach -- cargo test --release     # start now…
ucompute logs                                     # …and follow it later
```

Or just talk to your agent: *"Run Veramast's CI checks on my PC."* With the skill installed, Claude reads `.github/workflows`, pulls out the `run:` steps, and runs them through `ucompute run`. It then reads the results and fixes what failed, all while your laptop stays cool.

| Flag | Meaning |
| --- | --- |
| `-w, --worker NAME` | pick a worker (default: any online worker, preferring another machine) |
| `-t, --timeout DUR` | stop the job after `DUR`, e.g. `30m` or `600` (default `1h`) |
| `-e, --env K=V` | environment variable for the job (repeatable) |
| `--include PATH` | upload a git-ignored file too, e.g. `.env` (repeatable) |
| `--fetch PATH` | copy `PATH` back when the job ends (repeatable) |
| `--no-sync` | don't upload the project; run in the worker's scratch folder |
| `-d, --detach` | print the job id and return immediately |
| `-q, --quiet` | print only the command's output |

Exit codes: the remote command's own code; `124` timeout; `125` Ucompute error; `130` cancelled. Press Ctrl-C in `ucompute run` to cancel the remote job.

## How it works

```
 laptop                         GitHub (private repo)                desktop
 ──────                         ─────────────────────                ───────
 ucompute run -- npm test
   snapshot working tree ──git push──▶ job/desktop/<id>
                                                      ◀──polls (free)── ucompute worker
                                                      ──git fetch────▶  checkout into its workspace
                                                                         run `npm test`
   stream output        ◀──git fetch── result/desktop/<id> ◀─git push── log + exit code, every few s
```

- **The relay** is a private repo, `<you>/ucompute-relay`, created by `ucompute setup`. Both machines already have your GitHub login, so neither needs an open port, and it works behind any NAT, hotspot or school network.
- **Snapshots** are git commits of your working tree (tracked, modified and untracked files; ignored files skipped). They're made with a throwaway index, so your branch, index and stash are never touched. After the first upload, only changed files travel.
- **The worker** keeps one workspace per project. Ignored files such as `node_modules`, `target/` and `.venv` stay put between runs, so dependencies install once and later runs are fast.
- **Polling is free.** Workers watch the relay with conditional requests, and an unchanged answer (HTTP 304) doesn't count against GitHub's rate limit.
- **Old jobs** are removed from the relay after 24 hours. The last result of each job is also kept locally in `~/.ucompute/jobs/`.

## Security

Ucompute is remote command execution by design, so understand who can run commands on your worker: **anyone who can push to your relay repo.** That's you, plus anyone holding a GitHub token with `repo` access to your account.

To keep that boundary tight:

- The relay must be **private**. A worker refuses jobs if the repo is public, or if it has collaborators other than you (`allow_collaborators` overrides this).
- GitHub Actions is turned off in the relay, so the synced copies of your projects' workflows never run there.
- Tokens are passed to git through environment variables, never on command lines, and the worker never sees your laptop's token.
- Jobs run as your user on the worker, inside `~/.ucompute/work/`, but they are not sandboxed.
- `--env` values and `--include`d files are stored in your private relay repo for up to 24 hours.

## Troubleshooting

- **`no worker is online`**: on the worker, run `ucompute service status` and read `~/.ucompute/worker.log`. Make sure the computer is awake (turn off sleep while plugged in) and online.
- **Commands not found on the worker** (`npm: command not found`): the service remembers the `PATH` from the moment you ran `ucompute setup --worker`. Run it again from a terminal where the tools work.
- **Linux worker can't read the GitHub token** (GNOME keyring locked or missing): run `ucompute setup --worker --store-token`.
- **Windows**: jobs run in Git Bash when Git for Windows is installed, otherwise in `cmd.exe`. Choose with `ucompute setup --worker --shell <path>`.
- **Two machines with the same name**: `ucompute setup --worker --name <unique-name>`.
- **Update**: `ucompute update` (restarts the worker too).

## Limitations (v0.1)

- Git submodule contents and Git LFS files aren't synced yet. The worker sees LFS pointer files, not the real files.
- Files larger than 95 MB can't be synced (GitHub's limit). Add them to `.gitignore`.
- A worker runs one job per project at a time (up to `max_parallel` across projects, default 2).
- There's latency: a job takes a few seconds to reach the worker, and output arrives in chunks every 2–10 seconds rather than character by character.

## Development

```bash
go test ./...            # includes an end-to-end test with a real worker and a local relay
go build ./cmd/ucompute
```

A tag like `v0.1.1` builds and publishes release binaries through GitHub Actions.

## License

MIT
