# klodsync

## Why does this exist

Claude Code keeps sessions, project memory, and settings in `~/.claude`,
written for one machine. There is no built-in way to move them to another.
A plain rsync mirror is dangerous: one stale machine pushing can overwrite
weeks of newer history in a single run.

klodsync syncs `~/.claude` between machines through a hub machine, without
that risk. Newer history is never overwritten by older history, in either
direction, no matter which machine starts the sync.

## Usage

```
klodsync status              per-project verdicts, changes nothing
klodsync run [--dry-run]     sync both directions
klodsync adopt <project>     make a project from another machine resumable here

--hub HOST:DIR | DIR         default robeast:.claude ($CLAUDE_SYNC_HUB)
--allow-mass-delete          override the refusal to delete >50% of a side
```

## How it decides

Per file, the newer side wins. "Newer" comes from timestamps inside the
session files, not file mtimes, so copies and path rewrites do not confuse
it. A project with unique sessions on both sides merges by union: both sides
end up with everything, nothing is deleted, and the merge is reported.

Deletions propagate only through a state file that records the agreed file
list after the last fully successful run. No state means no deletions — the
run degrades to a pure merge. A file deleted on one side but modified on the
other survives. Live-process lock files never travel.

The `.claude.json` per-project entries (trust, permissions, MCP servers,
last session) move with their project. Prompt history merges as a union.
Hooks, skills, and plugin files sync newest-per-file.

## Requirements

Local machine: ssh, rsync, jq, perl, cksum. Hub: stock sh, perl, jq, rsync —
nothing to install; the scanner ships as perl over the ssh connection.

## Install

```
go install github.com/dimitarvp/klodsync@latest
```

## Tests

`go test ./...` covers the per-file decision engine and the transcript
parsing edge cases (timestamp-less final lines, >64KB lines).

`tests/gauntlet.sh` is the behavior suite: 65 checks across 18 scenarios
(stale machines, divergence, deletions, adopt, live sessions) run against
synthetic machines in a temp dir — no ssh, no real `~/.claude`. It builds
the current source by default; `CS=/path/to/tool` tests any other
implementation of the same CLI.
