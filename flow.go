package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var sidRe = regexp.MustCompile(`^([0-9a-fA-F-]{36})\.jsonl$`)

const satRoots = "file-history tasks session-env"

func sync(cfg *config, verb string) error {
	t0 := time.Now()
	fmt.Println("klodsync: scanning both sides...")

	// both manifests concurrently: hub via the embedded perl, local natively
	type res struct {
		m   *manifest
		err error
	}
	hubCh := make(chan res, 1)
	go func() {
		out, err := hubPerl(cfg, manifestPerl)
		if err != nil {
			hubCh <- res{nil, err}
			return
		}
		hubCh <- res{parseManifest(out), nil}
	}()
	local, err := scanLocal(cfg.LRoot)
	if err != nil {
		return err
	}
	hr := <-hubCh
	if hr.err != nil {
		return hr.err
	}
	hub := hr.m
	base, hadBase := readBase(cfg.BaseFile)
	fmt.Printf("  scan: %ds (local %d entries, hub %d)\n", int(time.Since(t0).Seconds()), local.Lines, hub.Lines)

	versionWarn(cfg)

	p := buildPlan(local, hub, base, cfg.LPrefix, cfg.HPrefix)

	// ── status table ────────────────────────────────────────────
	fmt.Println()
	rows := [][]string{{"PROJECT", "LOCAL", "HUB", "VERDICT", "CHANGES"}}
	for i := range p.Projects {
		pp := &p.Projects[i]
		ch := fmt.Sprintf("↑%d ↓%d", len(pp.Push), len(pp.Pull))
		if len(pp.DelHub) > 0 {
			ch += fmt.Sprintf(" ✗hub:%d", len(pp.DelHub))
		}
		if len(pp.DelLocal) > 0 {
			ch += fmt.Sprintf(" ✗local:%d", len(pp.DelLocal))
		}
		rows = append(rows, []string{pp.Canon, tsShort(pp.LMax), tsShort(pp.HMax), pp.Verdict, ch})
	}
	printTable(rows)
	fmt.Println()

	var unions []string
	totPush, totPull, totDelH, totDelL := 0, 0, 0, 0
	for i := range p.Projects {
		pp := &p.Projects[i]
		totPush += len(pp.Push)
		totPull += len(pp.Pull)
		totDelH += len(pp.DelHub)
		totDelL += len(pp.DelLocal)
		if pp.Verdict == "union" {
			unions = append(unions, pp.Canon)
		}
	}
	if len(unions) > 0 {
		fmt.Printf("union (history on both sides — merging, no deletions): %s\n", strings.Join(unions, ", "))
	}
	if !hadBase {
		fmt.Println("note: no sync state for this hub yet — this run is a pure merge, no deletions.")
	}
	fmt.Printf("plan: %d↑ %d↓ %d✗hub %d✗local\n", totPush, totPull, totDelH, totDelL)

	if cfg.Dry {
		if verb == "run" {
			fmt.Println("(dry-run: nothing changed)")
		}
		return nil
	}

	// mass-delete guard (from the rclone playbook): a plan wanting to delete
	// most of a side is a damaged manifest, not intent.
	if !cfg.AllowMassDelete {
		if totDelH > 10 && totDelH*2 > p.HTotal {
			return fmt.Errorf("refusing to delete %d of %d hub files (>50%%) — rerun with --allow-mass-delete if intended", totDelH, p.HTotal)
		}
		if totDelL > 10 && totDelL*2 > p.LTotal {
			return fmt.Errorf("refusing to delete %d of %d local files (>50%%) — rerun with --allow-mass-delete if intended", totDelL, p.LTotal)
		}
	}

	// ── sid direction map: satellite dirs follow their transcripts ─
	pushSids, pullSids, delHSids, delLSids := sidSets(p)

	// ── transfers (same-name batched; cross-name per-project) ────
	t1 := time.Now()
	var pushList, pullList []string
	for i := range p.Projects {
		pp := &p.Projects[i]
		sameName := pp.HDir == "" || pp.LDir == "" || pp.HDir == pp.LDir
		if !sameName {
			continue
		}
		if pp.LDir != "" {
			for _, r := range pp.Push {
				pushList = append(pushList, "projects/"+pp.LDir+"/"+r)
			}
		}
		if pp.HDir != "" {
			for _, r := range pp.Pull {
				pullList = append(pullList, "projects/"+pp.HDir+"/"+r)
			}
			if len(pp.Pull) > 0 {
				if err := os.MkdirAll(filepath.Join(cfg.LRoot, "projects", pp.HDir), 0o750); err != nil {
					return err
				}
			}
		}
	}
	for sid := range pushSids {
		for _, satroot := range strings.Fields(satRoots) {
			if _, err := os.Stat(filepath.Join(cfg.LRoot, satroot, sid)); err == nil {
				pushList = append(pushList, satroot+"/"+sid+"/")
			}
		}
	}
	for sid := range pullSids {
		for _, satroot := range strings.Fields(satRoots) {
			if p.HSids[satroot][sid] {
				pullList = append(pullList, satroot+"/"+sid+"/")
			}
		}
	}
	sort.Strings(pushList)
	sort.Strings(pullList)

	if err := transferList(cfg, pullList, "pull.list", hubArg(cfg), cfg.LRoot); err != nil {
		return err
	}
	if err := transferList(cfg, pushList, "push.list", cfg.LRoot, hubArg(cfg)); err != nil {
		return err
	}

	// cross-name projects (same project, different dir names per side)
	for i := range p.Projects {
		pp := &p.Projects[i]
		if pp.LDir == "" || pp.HDir == "" || pp.LDir == pp.HDir {
			continue
		}
		if err := transferList(cfg, pp.Pull, "xpull.list",
			hubArg(cfg)+"/projects/"+pp.HDir, filepath.Join(cfg.LRoot, "projects", pp.LDir)); err != nil {
			return err
		}
		if err := transferList(cfg, pp.Push, "xpush.list",
			filepath.Join(cfg.LRoot, "projects", pp.LDir), hubArg(cfg)+"/projects/"+pp.HDir); err != nil {
			return err
		}
	}
	fmt.Printf("  transfer: %ds (%d↑ %d↓)\n", int(time.Since(t1).Seconds()), totPush, totPull)

	// ── deletions (validated relpaths only; fail closed) ─────────
	var delHub, delLocal []string
	for i := range p.Projects {
		pp := &p.Projects[i]
		if pp.HDir != "" {
			for _, r := range pp.DelHub {
				delHub = append(delHub, "projects/"+pp.HDir+"/"+r)
			}
		}
		if pp.LDir != "" {
			for _, r := range pp.DelLocal {
				delLocal = append(delLocal, "projects/"+pp.LDir+"/"+r)
			}
		}
	}
	for sid := range delHSids {
		for _, satroot := range strings.Fields(satRoots) {
			if p.HSids[satroot][sid] {
				delHub = append(delHub, satroot+"/"+sid+"/")
			}
		}
	}
	for sid := range delLSids {
		for _, satroot := range strings.Fields(satRoots) {
			if _, err := os.Stat(filepath.Join(cfg.LRoot, satroot, sid)); err == nil {
				delLocal = append(delLocal, satroot+"/"+sid+"/")
			}
		}
	}
	if err := deleteOnHub(cfg, delHub); err != nil {
		return err
	}
	if err := deleteLocal(cfg, delLocal); err != nil {
		return err
	}

	// ── ~/.claude.json per-project whitelist merge ───────────────
	if err := mergeProjectEntries(cfg, p); err != nil {
		return err
	}

	// ── shared config buckets + singletons ───────────────────────
	if err := buckets(cfg); err != nil {
		return err
	}
	syncFile(cfg, "CLAUDE.md")
	syncFile(cfg, "settings.json")
	syncFile(cfg, ".credentials.json") // Dimi 2026-08-26: LAN-only, creds included by his call
	pluginMetaSync(cfg, "plugins/installed_plugins.json")
	pluginMetaSync(cfg, "plugins/known_marketplaces.json")
	if err := historyUnion(cfg); err != nil {
		return err
	}
	makeAliases(cfg)

	// ── record agreed state (only after full success) ────────────
	if err := writeBase(cfg.BaseFile, p); err != nil {
		return err
	}
	fmt.Printf("done in %ds.\n", int(time.Since(t0).Seconds()))
	return nil
}

func sidSets(p *plan) (push, pull, delH, delL map[string]bool) {
	push, pull, delH, delL = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	collect := func(rels []string, into map[string]bool) {
		for _, r := range rels {
			if m := sidRe.FindStringSubmatch(r); m != nil {
				into[m[1]] = true
			}
		}
	}
	for i := range p.Projects {
		pp := &p.Projects[i]
		collect(pp.Push, push)
		collect(pp.Pull, pull)
		collect(pp.DelHub, delH)
		collect(pp.DelLocal, delL)
	}
	return
}

func transferList(cfg *config, rels []string, name, src, dst string) error {
	if len(rels) == 0 {
		return nil
	}
	listPath := filepath.Join(cfg.Work, name)
	if err := os.WriteFile(listPath, []byte(strings.Join(rels, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return rsyncFiles(cfg, listPath, src, dst)
}

func tsShort(ts string) string {
	if ts == "-" || len(ts) < 16 {
		return ts
	}
	return ts[:16]
}

func printTable(rows [][]string) {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			if w := len([]rune(cell)); w > widths[i] {
				widths[i] = w
			}
		}
	}
	for _, row := range rows {
		var b strings.Builder
		for i, cell := range row {
			b.WriteString(cell)
			if i < len(row)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len([]rune(cell))+2))
			}
		}
		fmt.Println(strings.TrimRight(b.String(), " "))
	}
}

func versionWarn(cfg *config) {
	if !cfg.Remote {
		return
	}
	lv := firstLine(exec.Command("claude", "--version"))
	hvOut, err := hubScript(cfg, `claude --version 2>/dev/null || "$HOME"/.local/bin/claude --version 2>/dev/null || true`)
	if err != nil {
		return
	}
	first, _, _ := strings.Cut(hvOut, "\n")
	hv := strings.TrimSpace(first)
	if lv != "" && hv != "" && lv != hv {
		fmt.Printf("  WARNING: Claude Code versions differ — local '%s' vs hub '%s'.\n", lv, hv)
		fmt.Println("           Transcript format is version-internal; storing as archive is fine,")
		fmt.Println("           but match versions before actively resuming the other side's sessions.")
	}
}

func firstLine(cmd *exec.Cmd) string {
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(first)
}

// ── ~/.claude.json whitelist merge ───────────────────────────────

var entryWhitelist = []string{
	"allowedTools", "mcpServers", "enabledMcpjsonServers", "disabledMcpjsonServers",
	"hasTrustDialogAccepted", "hasClaudeMdExternalIncludesApproved",
	"hasClaudeMdExternalIncludesWarningShown", "mcpContextUris", "lastSessionId",
}

type kv struct {
	Key string
	Raw json.RawMessage
}

// orderedProjects returns .projects entries in file order (jq's first() picks
// document order; Go maps would randomize it).
func orderedProjects(raw []byte) []kv {
	dec := json.NewDecoder(bytes.NewReader(raw))
	t, err := dec.Token()
	if err != nil || t != json.Delim('{') {
		return nil
	}
	var out []kv
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return out
		}
		key, _ := keyTok.(string)
		if key == "projects" {
			t, err := dec.Token()
			if err != nil || t != json.Delim('{') {
				var skip json.RawMessage
				_ = dec.Decode(&skip)
				continue
			}
			for dec.More() {
				pk, err := dec.Token()
				if err != nil {
					return out
				}
				var val json.RawMessage
				if err := dec.Decode(&val); err != nil {
					return out
				}
				out = append(out, kv{pk.(string), val})
			}
			_, _ = dec.Token() // closing }
		} else {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return out
			}
		}
	}
	return out
}

type entryEdit struct {
	DestKey string          `json:"dest_key"`
	Entry   json.RawMessage `json:"entry"`
}

func mergeProjectEntries(cfg *config, p *plan) error {
	hubJSON, err := hubScript(cfg, `cat "$HUBJSON" 2>/dev/null || echo "{}"`)
	if err != nil {
		return err
	}
	if strings.TrimSpace(hubJSON) == "" {
		hubJSON = "{}"
	}
	localJSON, err := os.ReadFile(cfg.LJSON)
	if err != nil {
		localJSON = []byte("{}")
	}
	lEntries := orderedProjects(localJSON)
	hEntries := orderedProjects([]byte(hubJSON))

	pick := func(entries []kv, ownHome, canon string) *kv {
		var cands []kv
		for _, e := range entries {
			if canonOf(munge(e.Key), cfg.LPrefix, cfg.HPrefix) == canon {
				cands = append(cands, e)
			}
		}
		for i := range cands {
			if strings.HasPrefix(cands[i].Key, ownHome) {
				return &cands[i]
			}
		}
		if len(cands) > 0 {
			return &cands[0]
		}
		return nil
	}
	toDest := func(key, fromHome, toHome string) string {
		if strings.HasPrefix(key, fromHome) {
			return toHome + strings.TrimPrefix(key, fromHome)
		}
		return key
	}

	var localEdits, hubEdits []entryEdit
	for i := range p.Projects {
		pp := &p.Projects[i]
		if pp.Verdict == "clean" {
			continue
		}
		winner := "hub"
		switch pp.Verdict {
		case "push", "new-push":
			winner = "local"
		case "pull", "new-pull":
			winner = "hub"
		default:
			if pp.LMax >= pp.HMax {
				winner = "local"
			}
		}
		var src *kv
		if winner == "local" {
			src = pick(lEntries, cfg.LHome, pp.Canon)
		} else {
			src = pick(hEntries, cfg.HHome, pp.Canon)
		}
		if src == nil {
			continue
		}
		var full map[string]json.RawMessage
		if err := json.Unmarshal(src.Raw, &full); err != nil {
			continue
		}
		filtered := map[string]json.RawMessage{}
		for _, k := range entryWhitelist {
			if v, ok := full[k]; ok {
				filtered[k] = v
			}
		}
		if len(filtered) == 0 {
			continue
		}
		fraw, _ := json.Marshal(filtered)
		if winner == "local" {
			hubEdits = append(hubEdits, entryEdit{toDest(src.Key, cfg.LHome, cfg.HHome), fraw})
		} else {
			localEdits = append(localEdits, entryEdit{toDest(src.Key, cfg.HHome, cfg.LHome), fraw})
		}
	}

	const reduceProg = `reduce $edits[0][] as $e (.; .projects[$e.dest_key] = ((.projects[$e.dest_key] // {}) + $e.entry))`

	if len(localEdits) > 0 {
		if _, err := os.Stat(cfg.LJSON); err == nil {
			ej, _ := json.Marshal(localEdits)
			edFile := filepath.Join(cfg.Work, "local_edits.json")
			if err := os.WriteFile(edFile, ej, 0o600); err != nil {
				return err
			}
			tmp := filepath.Join(cfg.Work, "lj.new")
			if err := jqFile2(cfg.LJSON, tmp, reduceProg, "--slurpfile", "edits", edFile); err != nil {
				return err
			}
			if err := os.Rename(tmp, cfg.LJSON); err != nil {
				return err
			}
		}
		fmt.Printf("  .claude.json: %d project entr(y/ies) updated locally\n", len(localEdits))
	}
	if len(hubEdits) > 0 {
		ej, _ := json.Marshal(hubEdits)
		edFile := filepath.Join(cfg.Work, "hub_edits.json")
		if err := os.WriteFile(edFile, ej, 0o600); err != nil {
			return err
		}
		if cfg.Remote {
			htmp, err := hubScript(cfg, "mktemp")
			if err != nil {
				return err
			}
			htmp = strings.TrimSpace(htmp)
			if err := rsyncOne(cfg, edFile, cfg.HHost+":"+htmp); err != nil {
				return err
			}
			script := `
[ -f "$HUBJSON" ] || exit 0
tmp="$(mktemp)"
jq --slurpfile edits "` + htmp + `" \
  '` + reduceProg + `' \
  "$HUBJSON" > "$tmp" && [ -s "$tmp" ] && mv "$tmp" "$HUBJSON" || rm -f "$tmp"
rm -f "` + htmp + `"`
			if _, err := hubScript(cfg, script); err != nil {
				return err
			}
		} else if _, err := os.Stat(cfg.HJSON); err == nil {
			tmp := filepath.Join(cfg.Work, "hj.new")
			if err := jqFile2(cfg.HJSON, tmp, reduceProg, "--slurpfile", "edits", edFile); err != nil {
				return err
			}
			if err := os.Rename(tmp, cfg.HJSON); err != nil {
				return err
			}
		}
		fmt.Printf("  .claude.json: %d project entr(y/ies) updated on hub\n", len(hubEdits))
	}
	return nil
}
