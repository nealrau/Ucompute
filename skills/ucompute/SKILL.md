---
name: ucompute
description: Run heavy commands (test suites, CI checks, builds, linters, type-checks, benchmarks) on the user's more powerful computer through Ucompute instead of on this machine. Use when the user asks to run checks, tests or builds remotely, "on my PC", "on my desktop" or "with ucompute", or when this machine is weak or overheating and a Ucompute worker is online. Also use to check whether the user's other machine is reachable.
---

# Ucompute: run heavy commands on the user's powerful computer

`ucompute` sends the current project to a worker machine (committed, modified and untracked files; git-ignored files stay behind), runs a command there, and streams the output back. It exits with the remote command's exit code. Nothing needs to be committed or pushed first.

## 1. Check that a worker is online

```bash
ucompute status
```

If no worker is online, tell the user. Run the command locally only if they agree.

## 2. Run the command

Run from the project folder. If you're in a subfolder, the command runs in the same subfolder on the worker.

```bash
ucompute run -- npm test
ucompute run -- "npm run lint && npm run typecheck && npm test"
```

- The output streams live. ucompute's own status lines go to stderr and start with `ucompute:`.
- Exit codes: the remote command's code; 124 = timed out; 130 = cancelled; 125 = ucompute itself failed (read its message).
- Git-ignored files (node_modules, build output, .env) are **not** uploaded. The worker keeps its own copy of ignored files between runs, so dependencies stay installed. Run the install step (`npm ci`, `pip install -r requirements.txt`, ...) on the worker the first time, and again when dependencies change.
- Need an ignored file such as `.env`? Add `--include .env`.
- Need files back (a coverage report, build output)? Add `--fetch coverage/`. The path is relative to the current folder.
- Environment variables: `-e CI=true -e NODE_ENV=test`.
- Several workers: pick one with `-w NAME`.
- A command that doesn't need the project: `ucompute run --no-sync -- nvidia-smi`.
- The default time limit is 1 hour; change it with `--timeout 2h`.

## 3. Long jobs

Your shell tool may time out before the job ends, and if that kills `ucompute run`, the remote job gets cancelled. For anything that could take more than a couple of minutes:

- give the shell call a long timeout (up to 10 minutes), or
- detach: `ucompute run --detach -- <cmd>` prints a job id right away. Then `ucompute logs <id>` streams the output and exits with the job's exit code. If `ucompute logs` itself gets interrupted, the job keeps running, so you can call it again. `ucompute logs` with no id re-attaches to the most recent job.

## Running a project's CI checks remotely

1. Read `.github/workflows/*.yml` and collect the `run:` steps of the relevant jobs. Skip `uses:` steps such as checkout or setup-node; the worker already has its toolchains.
2. Run them as one job so they share the workspace, for example:
   `ucompute run -e CI=true -- "npm ci && npm run lint && npm run typecheck && npm test"`
3. Report failures with the relevant output, fix them locally, and run again. Every run syncs your latest local edits.

## Rules

- No interactive commands, dev servers or watch modes: stdin is closed, so nothing can answer a prompt.
- The worker is the user's real computer. Don't run destructive commands outside the project.
- If `ucompute` is missing, see https://github.com/nealrau/Ucompute. Ask the user before setting it up.
