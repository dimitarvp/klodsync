package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func buckets(cfg *config) error {
	if _, err := hubScript(cfg, `mkdir -p "$HUBROOT/hooks" "$HUBROOT/skills" "$HUBROOT/plugins"`); err != nil {
		return err
	}
	base := []string{"--exclude=*.bak*", "--exclude=.DS_Store", "--exclude=statusline.sh"}
	pluginEx := []string{"--exclude=installed_plugins.json", "--exclude=known_marketplaces.json"}
	for _, bucket := range []string{"hooks", "skills", "plugins"} {
		src := filepath.Join(cfg.LRoot, bucket)
		if err := os.MkdirAll(src, 0o750); err != nil {
			return err
		}
		ex := append([]string{}, base...)
		if bucket == "plugins" {
			ex = append(ex, pluginEx...)
		}
		// newest-per-file, both ways, never delete
		for _, dir := range [][2]string{
			{hubArg(cfg) + "/" + bucket, src},
			{src, hubArg(cfg) + "/" + bucket},
		} {
			args := make([]string, 0, 2+len(ex)+2+2)
			args = append(args, "-au", "-r")
			args = append(args, ex...)
			args = append(args, rsyncE(cfg)...)
			args = append(args, dir[0]+"/", dir[1]+"/")
			if err := runCmd("rsync", args...); err != nil {
				return fmt.Errorf("bucket %s: %w", bucket, err)
			}
		}
	}
	return nil
}

func localCksum(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "missing"
	}
	defer func() { _ = f.Close() }()
	cmd := exec.Command("cksum")
	cmd.Stdin = f
	out, err := cmd.Output()
	if err != nil {
		return "missing"
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "missing"
	}
	return fields[0] + ":" + fields[1]
}

// syncFile: single files, newest mtime wins, only when content differs.
// Failures are reported but never abort the run (matches `|| true`).
func syncFile(cfg *config, rel string) {
	lf := filepath.Join(cfg.LRoot, rel)
	lsum := localCksum(lf)
	hline, err := hubScript(cfg, `
f="$HUBROOT/`+rel+`"
if [ -f "$f" ]; then
	printf "%s " "$(cksum < "$f" | awk "{print \$1\":\"\$2}")"
	stat -c %Y "$f" 2>/dev/null || stat -f %m "$f"
else echo "missing 0"; fi`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %s: hub probe failed, skipped (%v)\n", rel, err)
		return
	}
	fields := strings.Fields(hline)
	if len(fields) < 2 {
		return
	}
	hsum, hmtS := fields[0], fields[len(fields)-1]
	if lsum == hsum {
		return
	}
	report := func(dir string) { fmt.Printf("  %s: %s\n", rel, dir) }
	switch {
	case lsum == "missing":
		if rsyncOne(cfg, hubArg(cfg)+"/"+rel, lf) == nil {
			report("← hub (missing here)")
		}
	case hsum == "missing":
		if rsyncOne(cfg, lf, hubArg(cfg)+"/"+rel) == nil {
			report("→ hub (missing there)")
		}
	default:
		st, err := os.Stat(lf)
		if err != nil {
			return
		}
		var hmt int64
		hmt, _ = strconv.ParseInt(strings.TrimSpace(hmtS), 10, 64) // unparsable hub mtime = 0: hub counts as older, we push (safe)
		if st.ModTime().Unix() >= hmt {
			if rsyncOne(cfg, lf, hubArg(cfg)+"/"+rel) == nil {
				report("→ hub (newer here)")
			}
		} else {
			if rsyncOne(cfg, hubArg(cfg)+"/"+rel, lf) == nil {
				report("← hub (newer there)")
			}
		}
	}
}

const walkRewrite = `walk(if type == "string" and startswith($from) then $to + ltrimstr($from) else . end)`

func jqTo(out *bytes.Buffer, stdin []byte, args ...string) error {
	cmd := exec.Command("jq", args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout = out
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("jq: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// pluginMetaSync compares plugin metadata AFTER normalizing $HOME prefixes
// (each side keeps its own path format), newest mtime wins, receiver rewritten.
func pluginMetaSync(cfg *config, rel string) {
	lf := filepath.Join(cfg.LRoot, rel)
	hubRaw, err := hubScript(cfg, `cat "$HUBROOT/`+rel+`" 2>/dev/null || true`)
	if err != nil {
		return
	}
	var hubNorm bytes.Buffer
	if strings.TrimSpace(hubRaw) != "" && cfg.HHome != cfg.LHome {
		if jqTo(&hubNorm, []byte(hubRaw), "--arg", "from", cfg.HHome, "--arg", "to", cfg.LHome, walkRewrite) != nil {
			hubNorm.Reset()
			hubNorm.WriteString(hubRaw)
		}
	} else {
		hubNorm.WriteString(hubRaw)
	}
	lData, lErr := os.ReadFile(lf)
	hubMissing := strings.TrimSpace(hubNorm.String()) == ""
	if lErr != nil && hubMissing {
		return
	}
	// canonical-JSON equality (jq round-trips reformat; raw bytes always differ)
	if lErr == nil && !hubMissing {
		var lc, hc bytes.Buffer
		if jqTo(&lc, lData, "-S", ".") == nil && jqTo(&hc, hubNorm.Bytes(), "-S", ".") == nil && bytes.Equal(lc.Bytes(), hc.Bytes()) {
			return
		}
	}
	hubRewrite := func() {
		if cfg.HHome == cfg.LHome {
			return
		}
		_, _ = hubScript(cfg, `
f="$HUBROOT/`+rel+`"
[ -f "$f" ] || exit 0
tmp="$(mktemp)"
jq --arg from "`+cfg.LHome+`" --arg to "`+cfg.HHome+`" \
  "walk(if type == \"string\" and startswith(\$from) then \$to + ltrimstr(\$from) else . end)" \
  "$f" > "$tmp" && [ -s "$tmp" ] && mv "$tmp" "$f" || rm -f "$tmp"`)
	}
	report := func(dir string) { fmt.Printf("  %s: %s\n", rel, dir) }
	switch {
	case lErr != nil:
		if err := os.MkdirAll(filepath.Dir(lf), 0o750); err == nil {
			if os.WriteFile(lf, hubNorm.Bytes(), 0o600) == nil {
				report("← hub (missing here)")
			}
		}
	case hubMissing:
		if rsyncOne(cfg, lf, hubArg(cfg)+"/"+rel) == nil {
			hubRewrite()
			report("→ hub (missing there)")
		}
	default:
		st, err := os.Stat(lf)
		if err != nil {
			return
		}
		hmtS, err := hubScript(cfg, `f="$HUBROOT/`+rel+`"; stat -c %Y "$f" 2>/dev/null || stat -f %m "$f"`)
		if err != nil {
			return
		}
		var hmt int64
		hmt, _ = strconv.ParseInt(strings.TrimSpace(hmtS), 10, 64) // unparsable hub mtime = 0: hub counts as older, we push (safe)
		if st.ModTime().Unix() >= hmt {
			if rsyncOne(cfg, lf, hubArg(cfg)+"/"+rel) == nil {
				hubRewrite()
				report("→ hub (newer here)")
			}
		} else {
			if os.WriteFile(lf, hubNorm.Bytes(), 0o600) == nil {
				report("← hub (newer there)")
			}
		}
	}
}

func historyUnion(cfg *config) error {
	histL := filepath.Join(cfg.LRoot, "history.jsonl")
	hubRaw, err := hubScript(cfg, `cat "$HUBROOT/history.jsonl" 2>/dev/null || true`)
	if err != nil {
		return err
	}
	if _, err := os.Stat(histL); err != nil {
		if err := os.WriteFile(histL, nil, 0o600); err != nil {
			return err
		}
	}
	lData, _ := os.ReadFile(histL)
	if bytes.Equal(lData, []byte(hubRaw)) {
		return nil
	}
	hubFile := filepath.Join(cfg.Work, "hub.history.jsonl")
	if err := os.WriteFile(hubFile, []byte(hubRaw), 0o600); err != nil {
		return err
	}
	var merged bytes.Buffer
	err = jqTo(&merged, nil, "-c", "-s",
		"unique_by([.timestamp, .display, .project]) | sort_by(.timestamp) | .[]", histL, hubFile)
	if err != nil || merged.Len() == 0 {
		fmt.Fprintln(os.Stderr, "  history.jsonl: merge failed, left both sides untouched")
		return nil
	}
	mergedFile := filepath.Join(cfg.Work, "history.merged")
	if err := os.WriteFile(mergedFile, merged.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(histL, merged.Bytes(), 0o600); err != nil {
		return err
	}
	if cfg.Remote {
		if err := rsyncOne(cfg, mergedFile, hubArg(cfg)+"/history.jsonl"); err != nil {
			return err
		}
	} else {
		if err := os.WriteFile(filepath.Join(cfg.HRoot, "history.jsonl"), merged.Bytes(), 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("  history.jsonl: merged (%d entries)\n", bytes.Count(merged.Bytes(), []byte("\n")))
	return nil
}

func makeAliases(cfg *config) {
	if cfg.LPrefix == cfg.HPrefix {
		return
	}
	pdir := filepath.Join(cfg.LRoot, "projects")
	entries, err := os.ReadDir(pdir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		full := filepath.Join(pdir, name)
		st, err := os.Lstat(full)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if !strings.HasPrefix(name, cfg.HPrefix) {
			continue
		}
		alias := cfg.LPrefix + strings.TrimPrefix(name, cfg.HPrefix)
		aliasPath := filepath.Join(pdir, alias)
		if _, err := os.Lstat(aliasPath); err == nil {
			continue
		}
		if os.Symlink(full, aliasPath) == nil {
			fmt.Printf("  alias: %s -> %s\n", alias, name)
		}
	}
}

// ── adopt ────────────────────────────────────────────────────────

func adopt(cfg *config, target string) error {
	// target is one bare directory name under projects/; a path separator or a
	// dot-name would let a typo walk out of the tree.
	if target == "" || target == "." || target == ".." || strings.ContainsAny(target, `/\`) {
		return fmt.Errorf("adopt: target must be a bare project dir name, got %q", target)
	}
	p := filepath.Join(cfg.LRoot, "projects", target)
	if _, err := os.Lstat(p); err != nil {
		return fmt.Errorf("no such project dir: %s", p)
	}
	realPath, err := filepath.EvalSymlinks(p)
	if err != nil {
		return err
	}
	realName := filepath.Base(realPath)

	if cfg.HHome == cfg.LHome {
		fmt.Println("hub and local $HOME match — nothing to rewrite.")
	} else {
		fmt.Printf("adopting %s: rewriting \"cwd\" %s/ -> %s/ (surgical, cwd/originalPath fields only)\n",
			realName, cfg.HHome, cfg.LHome)
		n := 0
		fromCwd := []byte(`"cwd":"` + cfg.HHome + `/`)
		toCwd := []byte(`"cwd":"` + cfg.LHome + `/`)
		fromOP := []byte(`"originalPath":"` + cfg.HHome + `/`)
		toOP := []byte(`"originalPath":"` + cfg.LHome + `/`)
		// Root-scoped: every read and write stays inside the project dir even if
		// a symlink appears under it mid-walk.
		root, err := os.OpenRoot(realPath)
		if err != nil {
			return err
		}
		defer func() { _ = root.Close() }()
		err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".jsonl") && !strings.HasSuffix(path, ".json") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			data, err := root.ReadFile(path)
			if err != nil {
				return nil
			}
			out := bytes.ReplaceAll(data, fromCwd, toCwd)
			out = bytes.ReplaceAll(out, fromOP, toOP)
			if !bytes.Equal(out, data) {
				if err := root.WriteFile(path, out, info.Mode()); err != nil {
					return err
				}
				n++
			}
			return nil
		})
		if err != nil {
			return err
		}
		fmt.Printf("  rewrote %d files\n", n)
	}

	if strings.HasPrefix(realName, cfg.HPrefix) && cfg.LPrefix != cfg.HPrefix {
		alias := cfg.LPrefix + strings.TrimPrefix(realName, cfg.HPrefix)
		aliasPath := filepath.Join(cfg.LRoot, "projects", alias)
		if _, err := os.Lstat(aliasPath); err != nil {
			if os.Symlink(realPath, aliasPath) == nil {
				fmt.Printf("  alias: %s -> %s\n", alias, realName)
			}
		}
	}

	if _, err := os.Stat(cfg.LJSON); err == nil && cfg.HHome != cfg.LHome {
		prog := `
def m(p): p | gsub("[/.]"; "-");
(.projects // {}) as $p
| ( first($p | to_entries[] | select((.key | m(.)) == $real)) // null ) as $src
| if ($src == null) or ($src.key | startswith($hh) | not) then .
  else .projects[($lh + ($src.key | ltrimstr($hh)))] //= $src.value
  end`
		tmp := filepath.Join(cfg.Work, "adopt.json")
		err := jqFile2(cfg.LJSON, tmp, prog,
			"--arg", "hh", cfg.HHome, "--arg", "lh", cfg.LHome, "--arg", "real", realName)
		if err == nil {
			if os.Rename(tmp, cfg.LJSON) == nil {
				fmt.Printf("  .claude.json entry cloned under %s path\n", cfg.LHome)
			}
		}
	}
	fmt.Println("adopted. --continue/--resume will now match this project here.")
	return nil
}

// jqFile2 runs `jq [opts] prog in > out`, refusing empty output.
func jqFile2(in, out, prog string, opts ...string) error {
	args := append(append([]string{}, opts...), prog, in)
	cmd := exec.Command("jq", args...)
	var buf, errb bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("jq failed: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	if buf.Len() == 0 {
		return fmt.Errorf("jq produced empty output for %s", in)
	}
	return os.WriteFile(out, buf.Bytes(), 0o600)
}
