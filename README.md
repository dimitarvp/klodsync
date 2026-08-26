# klodsync

Per-project bidirectional sync for Claude Code data (`~/.claude`) across
machines over LAN ssh, with one machine acting as the hub.

Go port of the proven `claude_sync` bash tool — same semantics, same state
files, same CLI; the two are drop-in interchangeable and validate each other.

    klodsync status              per-project verdicts, changes nothing
    klodsync run [--dry-run]     sync both directions, per project
    klodsync adopt <project>     make a foreign-machine project resumable here

    --hub HOST:DIR | DIR         default robeast:.claude ($CLAUDE_SYNC_HUB)
    --allow-mass-delete          override the >50% deletion guard

Semantics: per FILE, newest internal timestamp wins (read from inside session
jsonl, final-128KB window, last match; mtime fallback) — older history never
overwrites newer, in either direction. Divergence merges by union, loudly,
deleting nothing. Deletions propagate only via the per-hub agreed-state file
written after fully successful runs; no state = pure merge. Modify beats
delete. Live-process lock files and .DS_Store never travel.

The hub needs only stock tools (sh, perl, jq, rsync) — the manifest emitter
ships as embedded perl over ssh. Local side needs rsync, jq, cksum, ssh.

Tests: `go test ./...` for the decision engine and the measured parsing
hazards; the full behavior gauntlet lives in the fleet scripts —
`CS=$(command -v klodsync) claude_sync_test` (65 checks, shared with the bash
oracle).
