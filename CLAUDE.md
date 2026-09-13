# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->


## Build & Test

```bash
# Go (the service) — run from the repo root
gofmt -l . && go vet ./... && go test -race ./...

# Node (recorder) — PUPPETEER_SKIP_DOWNLOAD=1 avoids a ~150MB Chrome download
cd recorder && PUPPETEER_SKIP_DOWNLOAD=1 npm ci && npm test

# Docker — one image with the Go binary + Node + Chromium
docker build -t jitsi-capture .
```

CI (`.github/workflows/ci.yml`) runs these same three jobs (`go` / `node` /
`docker`) on every pull request and on pushes to `master`. Unit tests must pass
offline: no network, no Jitsi, no Zulip. Use `net/http/httptest` for HTTP
boundaries and a fake recorder shell script for the subprocess.

## Architecture Overview

`jitsi-capture` is the first of three services:

```
Zulip 🎙️ reaction -> jitsi-capture records the Jitsi call -> audio under DATA_DIR
  -> signed `recording.finished` webhook -> transcribetor (CPU transcription)
  -> Anarlog-format webhook -> tr2outline (Outline publisher)
  -> callback POST /notify on jitsi-capture -> "transcript ready" in the Zulip topic
```

This repo does ONLY: the Zulip bot (reaction flow), running the Node recorder as
a child process, persisting job state + audio on disk, sending the webhook, and
serving `/notify` + `/health`. No transcription and no Outline here.

- Go `package main` at the repo root, flat files (`config.go`, `job.go`, …),
  stdlib only — no new dependencies.
- `recorder/record.js` — Node + Puppeteer + Chromium, joins a Jitsi meeting
  headless and writes the audio; the Go service runs it as a child process and
  reads the single final JSON line from its stdout.
- Configuration is env-only (`config.go` is the single reader of `os.Getenv`).
  Never log secrets — log the variable NAME, not the value.

See `README.md` for the full spec.

## Conventions & Patterns

_Add your project-specific conventions here_
