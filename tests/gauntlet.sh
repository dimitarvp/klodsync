#!/bin/bash
# klodsync behavior gauntlet — simulates machines A/B (Linux-like) + a hub
# with a macOS-like $HOME, all as local dirs. No ssh, no real ~/.claude.
#
# Tests the tool named in $CS. Default: build the current source and test
# that. The bash oracle (or any other implementation of the same CLI) can be
# tested with CS=/path/to/tool tests/gauntlet.sh
set -u
if [[ -z "${CS:-}" ]]; then
	REPO="$(cd "$(dirname "$0")/.." && pwd)"
	BUILDDIR="$(mktemp -d /tmp/klodsync-build.XXXXXX)"
	trap 'rm -rf "$BUILDDIR"' EXIT
	(cd "$REPO" && go build -o "$BUILDDIR/klodsync" .) || {
		echo "build failed" >&2
		exit 1
	}
	CS="$BUILDDIR/klodsync"
fi
TESTROOT="$(mktemp -d /tmp/cs-gauntlet.XXXXXX)"
PASS=0 FAIL=0
LHOME=/home/dimi HHOME=/Users/dimi
LPRE=-home-dimi HPRE=-Users-dimi

say() { printf '%s\n' "$*"; }
ok() {
	PASS=$((PASS + 1))
	say "  ✓ $*"
}
bad() {
	FAIL=$((FAIL + 1))
	say "  ✗ FAIL: $*"
}
check() { # check <desc> <cmd...>
	local d="$1"
	shift
	if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi
}
checknot() {
	local d="$1"
	shift
	if "$@" >/dev/null 2>&1; then bad "$d"; else ok "$d"; fi
}

fresh() { # fresh <name> -> sets M (machine root), CR (claude root), CJ (json)
	M="$TESTROOT/$1"
	CR="$M/claude"
	CJ="$M/claude.json"
	mkdir -p "$CR/projects"
	echo '{"oauthAccount":{"who":"'"$1"'"},"userID":"u-'"$1"'","projects":{}}' >"$CJ"
}

sess() { # sess <claude-root> <projdir> <sid> <ts1> [ts2...]  last ts = freshness
	local cr="$1" proj="$2" sid="$3"
	shift 3
	mkdir -p "$cr/projects/$proj"
	: >"$cr/projects/$proj/$sid.jsonl"
	local t
	for t in "$@"; do
		echo '{"type":"user","cwd":"'"$LHOME"'/x","timestamp":"'"$t"'"}' >>"$cr/projects/$proj/$sid.jsonl"
	done
	mkdir -p "$cr/file-history/$sid" "$cr/tasks/$sid"
	echo snap >"$cr/file-history/$sid/f1"
	echo '{"id":"1"}' >"$cr/tasks/$sid/t.json"
}

run_on() { # run_on <machine-root> <hubdir> <verb> [flags...]
	local m="$1" hub="$2" verb="$3"
	shift 3
	CLAUDE_SYNC_LOCAL_CLAUDE="$m/claude" CLAUDE_SYNC_LOCAL_JSON="$m/claude.json" \
		CLAUDE_SYNC_LOCAL_HOME="$LHOME" CLAUDE_SYNC_HUB_HOME="$HHOME" \
		"$CS" "$verb" --hub "$hub" "$@"
}

treesum() { (cd "$1" && find . -type f,l | sort | xargs -r sha256sum 2>/dev/null | sha256sum | cut -c1-16); }

# ══ T1: new-push + idempotency ═══════════════════════════════════
say "── T1 new-push, satellites travel, second run clean"
fresh A
HUB="$TESTROOT/hub"
mkdir -p "$HUB/projects"
echo '{"oauthAccount":{"who":"hub"},"userID":"u-hub","projects":{}}' >"$HUB.json"
sess "$CR" "$LPRE-proj1" aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa \
	2026-08-01T10:00:00.000Z 2026-08-01T11:00:00.000Z
OUT="$(run_on "$M" "$HUB" run 2>&1)"
check "transcript on hub" test -f "$HUB/projects/$LPRE-proj1/aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa.jsonl"
check "file-history on hub" test -f "$HUB/file-history/aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa/f1"
check "tasks on hub" test -f "$HUB/tasks/aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa/t.json"
check "verdict new-push" grep -q 'new-push' <<<"$OUT"
OUT2="$(run_on "$M" "$HUB" run 2>&1)"
check "second run clean" grep -q 'clean' <<<"$OUT2"
check "second run zero plan" grep -q 'plan: 0↑ 0↓ 0✗hub 0✗local' <<<"$OUT2"

# ══ T2: new-pull of a hub-native (foreign-prefix) project ════════
say "── T2 new-pull foreign project: alias + claude.json entry translated"
mkdir -p "$HUB/projects/$HPRE-proj2"
printf '%s\n' '{"type":"user","cwd":"/Users/dimi/proj2","timestamp":"2026-08-02T09:00:00.000Z"}' \
	>"$HUB/projects/$HPRE-proj2/bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb.jsonl"
jq '.projects["/Users/dimi/proj2"] = {"allowedTools":["X"],"hasTrustDialogAccepted":true,"lastSessionId":"bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb","lastCost":9.99}' \
	"$HUB.json" >"$HUB.json.t" && mv "$HUB.json.t" "$HUB.json"
OUT="$(run_on "$M" "$HUB" run 2>&1)"
check "pulled transcript" test -f "$CR/projects/$HPRE-proj2/bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb.jsonl"
check "alias symlink created" test -L "$CR/projects/$LPRE-proj2"
check "entry under local path" jq -e '.projects["/home/dimi/proj2"].allowedTools == ["X"]' "$CJ"
check "metrics NOT copied" jq -e '.projects["/home/dimi/proj2"] | has("lastCost") | not' "$CJ"
check "local auth untouched" jq -e '.userID == "u-A"' "$CJ"

# ══ T3: stale satellite can never clobber (the robotko scenario) ═
say "── T3 stale satellite: hub advanced, stale B syncs → pull only"
fresh B
run_on "$M" "$HUB" run >/dev/null 2>&1 # B baselines from hub (proj1+proj2)
# hub advances proj1: sid gains a line + a brand-new session appears
printf '%s\n' '{"type":"user","cwd":"/home/dimi/x","timestamp":"2026-08-10T08:00:00.000Z"}' \
	>>"$HUB/projects/$LPRE-proj1/aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa.jsonl"
printf '%s\n' '{"type":"user","cwd":"/home/dimi/x","timestamp":"2026-08-10T09:00:00.000Z"}' \
	>"$HUB/projects/$LPRE-proj1/cccccccc-3333-4333-8333-cccccccccccc.jsonl"
HUBSUM_PRE="$(treesum "$HUB")"
OUT="$(run_on "$M" "$HUB" run 2>&1)"
check "B pulled the new session" test -f "$M/claude/projects/$LPRE-proj1/cccccccc-3333-4333-8333-cccccccccccc.jsonl"
check "B's stale sid updated" grep -q '2026-08-10T08:00' "$M/claude/projects/$LPRE-proj1/aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa.jsonl"
HUBSUM_POST="$(treesum "$HUB")"
check "hub untouched by stale B" test "$HUBSUM_PRE" = "$HUBSUM_POST"
checknot "no push happened" grep -qE 'plan: [1-9][0-9]*↑' <<<"$OUT"

# ══ T4: divergence → union, nothing deleted ══════════════════════
say "── T4 divergence: unique sessions both sides → union"
sess "$TESTROOT/A/claude" "$LPRE-proj1" dddddddd-4444-4444-8444-dddddddddddd 2026-08-11T10:00:00.000Z # A-only
printf '%s\n' '{"type":"user","cwd":"/home/dimi/x","timestamp":"2026-08-11T11:00:00.000Z"}' \
	>"$HUB/projects/$LPRE-proj1/eeeeeeee-5555-4555-8555-eeeeeeeeeeee.jsonl" # hub-only
OUT="$(run_on "$TESTROOT/A" "$HUB" run 2>&1)"
check "union verdict announced" grep -q 'union' <<<"$OUT"
check "A got hub-only session" test -f "$TESTROOT/A/claude/projects/$LPRE-proj1/eeeeeeee-5555-4555-8555-eeeeeeeeeeee.jsonl"
check "hub got A-only session" test -f "$HUB/projects/$LPRE-proj1/dddddddd-4444-4444-8444-dddddddddddd.jsonl"
check "nothing deleted anywhere" grep -qv '✗hub:[1-9]' <<<"$OUT"

# ══ T5: deletion propagation via state, both directions ══════════
say "── T5 deletions: propagate with state; modify-beats-delete"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1 # settle: A==hub, base fresh
rm -f "$TESTROOT/A/claude/projects/$LPRE-proj1/dddddddd-4444-4444-8444-dddddddddddd.jsonl"
rm -rf "$TESTROOT/A/claude/file-history/dddddddd-4444-4444-8444-dddddddddddd" \
	"$TESTROOT/A/claude/tasks/dddddddd-4444-4444-8444-dddddddddddd"
OUT="$(run_on "$TESTROOT/A" "$HUB" run 2>&1)"
check "hub transcript deleted" test ! -e "$HUB/projects/$LPRE-proj1/dddddddd-4444-4444-8444-dddddddddddd.jsonl"
check "hub file-history deleted" test ! -e "$HUB/file-history/dddddddd-4444-4444-8444-dddddddddddd"
check "delete was reported" grep -qE '✗hub:[1-9]' <<<"$OUT"
# hub-side delete propagates to A
rm -f "$HUB/projects/$LPRE-proj1/eeeeeeee-5555-4555-8555-eeeeeeeeeeee.jsonl"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "local transcript deleted" test ! -e "$TESTROOT/A/claude/projects/$LPRE-proj1/eeeeeeee-5555-4555-8555-eeeeeeeeeeee.jsonl"
# modify-beats-delete: A deletes a file the hub has since advanced
rm -f "$TESTROOT/A/claude/projects/$LPRE-proj1/cccccccc-3333-4333-8333-cccccccccccc.jsonl"
printf '%s\n' '{"type":"user","cwd":"/home/dimi/x","timestamp":"2026-08-12T09:00:00.000Z"}' \
	>>"$HUB/projects/$LPRE-proj1/cccccccc-3333-4333-8333-cccccccccccc.jsonl"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "advanced file resurrected, not deleted" grep -q '2026-08-12T09:00' \
	"$TESTROOT/A/claude/projects/$LPRE-proj1/cccccccc-3333-4333-8333-cccccccccccc.jsonl"
check "hub kept the advanced file" test -f "$HUB/projects/$LPRE-proj1/cccccccc-3333-4333-8333-cccccccccccc.jsonl"

# ══ T6: same internal ts, different bytes → stable, untouched ════
say "── T6 format-divergent but ts-equal files stay untouched"
printf '%s\n' '{"type":"user","cwd":"/home/dimi/y","timestamp":"2026-08-13T10:00:00.000Z"}' \
	>"$TESTROOT/A/claude/projects/$LPRE-proj1/ffffffff-6666-4666-8666-ffffffffffff.jsonl"
printf '%s\n' '{"type":"user","cwd":"/Users/dimi/y","timestamp":"2026-08-13T10:00:00.000Z"}' \
	>"$HUB/projects/$LPRE-proj1/ffffffff-6666-4666-8666-ffffffffffff.jsonl"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "local kept its format" grep -q '/home/dimi/y' "$TESTROOT/A/claude/projects/$LPRE-proj1/ffffffff-6666-4666-8666-ffffffffffff.jsonl"
check "hub kept its format" grep -q '/Users/dimi/y' "$HUB/projects/$LPRE-proj1/ffffffff-6666-4666-8666-ffffffffffff.jsonl"
OUT="$(run_on "$TESTROOT/A" "$HUB" status 2>&1)"
check "status stays clean" grep -q 'plan: 0↑ 0↓ 0✗hub 0✗local' <<<"$OUT"

# ══ T7: settings.json newest-wins, content-gated ═════════════════
say "── T7 settings.json: newest wins; identical content → no churn"
echo '{"hooks":{"A":1},"model":"m1"}' >"$TESTROOT/A/claude/settings.json"
echo '{"hooks":{"A":1},"model":"m2"}' >"$HUB/settings.json"
touch -d '2026-08-20 10:00' "$TESTROOT/A/claude/settings.json"
touch -d '2026-08-21 10:00' "$HUB/settings.json"
OUT="$(run_on "$TESTROOT/A" "$HUB" run 2>&1)"
check "hub settings won (newer)" jq -e '.model == "m2"' "$TESTROOT/A/claude/settings.json"
OUT="$(run_on "$TESTROOT/A" "$HUB" run 2>&1)"
checknot "no settings churn when identical" grep -q 'settings.json' <<<"$OUT"

# ══ T8: plugins metadata path rewrite on both sides ══════════════
say "── T8 plugins metadata: newest-wins + \$HOME rewrite per side"
mkdir -p "$TESTROOT/A/claude/plugins" "$HUB/plugins"
echo '{"plugins":{"x":[{"installPath":"/home/dimi/claude-stub/plugins/cache/x"}]}}' \
	>"$TESTROOT/A/claude/plugins/installed_plugins.json"
touch -d '2026-08-22 10:00' "$TESTROOT/A/claude/plugins/installed_plugins.json"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "hub copy rewritten to /Users" grep -q '/Users/dimi/claude-stub' "$HUB/plugins/installed_plugins.json"
check "local copy still /home" grep -q '/home/dimi/claude-stub' "$TESTROOT/A/claude/plugins/installed_plugins.json"

# ══ T9: hooks bucket newest-per-file, no delete, exclusions ══════
say "── T9 hooks bucket"
mkdir -p "$TESTROOT/A/claude/hooks" "$HUB/hooks"
echo v1 >"$TESTROOT/A/claude/hooks/h1.sh"
touch -d '2026-08-01 10:00' "$TESTROOT/A/claude/hooks/h1.sh"
echo v2 >"$HUB/hooks/h1.sh"
touch -d '2026-08-02 10:00' "$HUB/hooks/h1.sh"
echo hubonly >"$HUB/hooks/h2.sh"
echo localbak >"$TESTROOT/A/claude/hooks/h1.sh.bak-pre-x"
echo sl >"$TESTROOT/A/claude/statusline.sh"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "newer hub hook won" grep -q v2 "$TESTROOT/A/claude/hooks/h1.sh"
check "hub-only hook filled" test -f "$TESTROOT/A/claude/hooks/h2.sh"
checknot "bak not synced" test -e "$HUB/hooks/h1.sh.bak-pre-x"
checknot "statusline not synced" test -e "$HUB/statusline.sh"

# ══ T10: history union ═══════════════════════════════════════════
say "── T10 history.jsonl union"
printf '%s\n' \
	'{"display":"one","timestamp":1000,"project":"/home/dimi/p"}' \
	'{"display":"two","timestamp":2000,"project":"/home/dimi/p"}' >"$TESTROOT/A/claude/history.jsonl"
printf '%s\n' \
	'{"display":"two","timestamp":2000,"project":"/home/dimi/p"}' \
	'{"display":"three","timestamp":3000,"project":"/Users/dimi/q"}' >"$HUB/history.jsonl"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "3 unique entries local" test "$(wc -l <"$TESTROOT/A/claude/history.jsonl")" = 3
check "sides identical" cmp -s "$TESTROOT/A/claude/history.jsonl" "$HUB/history.jsonl"

# ══ T11: dry-run/status write nothing ════════════════════════════
say "── T11 status + dry-run are read-only"
sess "$TESTROOT/A/claude" "$LPRE-proj1" 99999999-7777-4777-8777-999999999999 2026-08-25T10:00:00.000Z
A_PRE="$(treesum "$TESTROOT/A")"
H_PRE="$(treesum "$HUB")"
run_on "$TESTROOT/A" "$HUB" status >/dev/null 2>&1
run_on "$TESTROOT/A" "$HUB" run --dry-run >/dev/null 2>&1
check "local unchanged" test "$A_PRE" = "$(treesum "$TESTROOT/A")"
check "hub unchanged" test "$H_PRE" = "$(treesum "$HUB")"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1 # settle for later tests

# ══ T12: cross-name project (different real dir names per side) ══
say "── T12 cross-name: same project, -home dir here, -Users dir there"
mkdir -p "$TESTROOT/A/claude/projects/$LPRE-cross" "$HUB/projects/$HPRE-cross"
printf '%s\n' '{"type":"user","cwd":"/home/dimi/cross","timestamp":"2026-08-26T10:00:00.000Z"}' \
	>"$TESTROOT/A/claude/projects/$LPRE-cross/11111111-8888-4888-8888-111111111111.jsonl"
printf '%s\n' '{"type":"user","cwd":"/Users/dimi/cross","timestamp":"2026-08-20T10:00:00.000Z"}' \
	>"$HUB/projects/$HPRE-cross/22222222-9999-4999-8999-222222222222.jsonl"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "A-only sid landed in hub's OWN dir" test -f "$HUB/projects/$HPRE-cross/11111111-8888-4888-8888-111111111111.jsonl"
checknot "no duplicate dir on hub" test -e "$HUB/projects/$LPRE-cross"
check "hub-only sid landed in A's OWN dir" test -f "$TESTROOT/A/claude/projects/$LPRE-cross/22222222-9999-4999-8999-222222222222.jsonl"
checknot "no duplicate dir on A" test -d "$TESTROOT/A/claude/projects/$HPRE-cross/22222222-9999-4999-8999-222222222222.jsonl"

# ══ T13: adopt ═══════════════════════════════════════════════════
say "── T13 adopt foreign project"
OUT="$(run_on "$TESTROOT/A" "$HUB" adopt "$HPRE-proj2" 2>&1)"
check "cwd rewritten" grep -q '"cwd":"/home/dimi/proj2"' "$TESTROOT/A/claude/projects/$HPRE-proj2/bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb.jsonl"
check "adopt reported rewrite" grep -q 'rewrote' <<<"$OUT"
check "alias still resolves" test -e "$TESTROOT/A/claude/projects/$LPRE-proj2"
# post-adopt sync must not re-transfer (ts unchanged)
OUT="$(run_on "$TESTROOT/A" "$HUB" status 2>&1)"
check "post-adopt still clean" grep -q 'plan: 0↑ 0↓ 0✗hub 0✗local' <<<"$OUT"

# ══ T14: memory-only project (mtime freshness) ═══════════════════
say "── T14 memory files ride along; deletions propagate for them too"
mkdir -p "$TESTROOT/A/claude/projects/$LPRE-proj1/memory"
echo fact >"$TESTROOT/A/claude/projects/$LPRE-proj1/memory/boarding.md"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "memory file on hub" test -f "$HUB/projects/$LPRE-proj1/memory/boarding.md"
rm -f "$TESTROOT/A/claude/projects/$LPRE-proj1/memory/boarding.md"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "boarding-file delete propagated (no zombie)" test ! -e "$HUB/projects/$LPRE-proj1/memory/boarding.md"
OUT="$(run_on "$TESTROOT/A" "$HUB" run 2>&1)"
check "and stays gone (no resurrection)" grep -q 'plan: 0↑ 0↓ 0✗hub 0✗local' <<<"$OUT"

# ══ T15: three-way convergence A -> hub -> B ═════════════════════
say "── T15 subsume: A and B each push own project; both see everything"
fresh C
sess "$CR" "$LPRE-projC" 33333333-aaaa-4aaa-8aaa-333333333333 2026-08-26T12:00:00.000Z
run_on "$M" "$HUB" run >/dev/null 2>&1
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
run_on "$TESTROOT/B" "$HUB" run >/dev/null 2>&1
check "A sees C's project" test -f "$TESTROOT/A/claude/projects/$LPRE-projC/33333333-aaaa-4aaa-8aaa-333333333333.jsonl"
check "B sees C's project" test -f "$TESTROOT/B/claude/projects/$LPRE-projC/33333333-aaaa-4aaa-8aaa-333333333333.jsonl"
check "B sees A's proj1 memory-less state consistently" test -d "$TESTROOT/B/claude/projects/$LPRE-proj1"

# ══ T16: hub-format dir name + local-format json key still moves entry ══
say "── T16 entry push despite dir-name/json-key format mismatch"
mkdir -p "$TESTROOT/A/claude/projects/$HPRE-legacy" # real dir named in HUB format (old-sync legacy)
printf '%s\n' '{"type":"user","cwd":"/home/dimi/legacy","timestamp":"2026-08-26T15:00:00.000Z"}' \
	>"$TESTROOT/A/claude/projects/$HPRE-legacy/44444444-bbbb-4bbb-8bbb-444444444444.jsonl"
jq '.projects["/home/dimi/legacy"] = {"allowedTools":["T"],"lastSessionId":"44444444-bbbb-4bbb-8bbb-444444444444","hasTrustDialogAccepted":true}' \
	"$TESTROOT/A/claude.json" >"$TESTROOT/A/claude.json.t" && mv "$TESTROOT/A/claude.json.t" "$TESTROOT/A/claude.json"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "hub json got the entry under /Users key" \
	jq -e '.projects["/Users/dimi/legacy"].lastSessionId == "44444444-bbbb-4bbb-8bbb-444444444444"' "$HUB.json"
check "whitelist filtered" jq -e '.projects["/Users/dimi/legacy"] | has("allowedTools")' "$HUB.json"

# ══ T17: .credentials.json syncs (Dimi's LAN-only call), no churn ══
say "── T17 credentials sync"
echo '{"claudeAiOauth":{"accessToken":"tok-A"}}' >"$TESTROOT/A/claude/.credentials.json"
chmod 600 "$TESTROOT/A/claude/.credentials.json"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "creds pushed to hub" grep -q 'tok-A' "$HUB/.credentials.json"
check "mode preserved" test "$(stat -c %a "$HUB/.credentials.json")" = 600
echo '{"claudeAiOauth":{"accessToken":"tok-NEWER"}}' >"$HUB/.credentials.json"
touch -d '2030-01-01 10:00' "$HUB/.credentials.json"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "newer hub creds pulled" grep -q 'tok-NEWER' "$TESTROOT/A/claude/.credentials.json"
OUT="$(run_on "$TESTROOT/A" "$HUB" run 2>&1)"
checknot "no creds churn when identical" grep -q 'credentials' <<<"$OUT"

# ══ T18: live-process lock files never travel ══════════════════════
say "── T18 lock files excluded from sync"
SID18=55555555-cccc-4ccc-8ccc-555555555555
sess "$TESTROOT/A/claude" "$LPRE-proj1" "$SID18" 2026-08-27T10:00:00.000Z
touch "$TESTROOT/A/claude/tasks/$SID18/.lock"
touch "$TESTROOT/A/claude/projects/$LPRE-proj1/stray.lock"
run_on "$TESTROOT/A" "$HUB" run >/dev/null 2>&1
check "transcript synced" test -f "$HUB/projects/$LPRE-proj1/$SID18.jsonl"
check "task payload synced" test -f "$HUB/tasks/$SID18/t.json"
checknot "task .lock did NOT travel" test -e "$HUB/tasks/$SID18/.lock"
checknot "project stray.lock did NOT travel" test -e "$HUB/projects/$LPRE-proj1/stray.lock"
OUT="$(run_on "$TESTROOT/A" "$HUB" run 2>&1)"
check "locks don't cause churn" grep -q 'plan: 0↑ 0↓ 0✗hub 0✗local' <<<"$OUT"


# ══ T19: the per-machine lock is kernel-owned: an interrupted run leaves nothing behind ══
say "── T19 lock: interrupted status, legacy oracle dir, orphan work dirs"
run_on "$TESTROOT/A" "$HUB" status 2>/dev/null | head -c 1 >/dev/null # the pager closes the pipe at once
OUT="$(run_on "$TESTROOT/A" "$HUB" run --dry-run 2>&1)"
checknot "run after an interrupted status is not blocked" grep -q 'another klodsync' <<<"$OUT"
check "lock is a plain file that stays" test -f "$TESTROOT/A/claude/.claude-sync/klodsync.lock"
mkdir -p "$TESTROOT/A/claude/.claude-sync/lock"
OUT="$(run_on "$TESTROOT/A" "$HUB" run --dry-run 2>&1)"
RC=$?
check "legacy mkdir lock dir refuses the run" test "$RC" -ne 0
check "…and names it" grep -q 'legacy lock dir' <<<"$OUT"
rmdir "$TESTROOT/A/claude/.claude-sync/lock"
mkdir -p "$TESTROOT/A/claude/.claude-sync/work.orphan"
run_on "$TESTROOT/A" "$HUB" run --dry-run >/dev/null 2>&1
checknot "orphan work dir swept by the next run" test -e "$TESTROOT/A/claude/.claude-sync/work.orphan"
checknot "no work dir left behind after a run" test -n "$(ls -d "$TESTROOT/A/claude/.claude-sync"/work.* 2>/dev/null)"

say ""
say "════ RESULT: $PASS passed, $FAIL failed ════"
say "(fixtures in $TESTROOT — removed on success)"
[[ $FAIL -eq 0 ]] && rm -rf "$TESTROOT"
exit "$FAIL"
