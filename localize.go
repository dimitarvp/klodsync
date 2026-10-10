package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// localize moves every real project folder that carries the other side's
// $HOME slug (a pull or push made before v0.1.6) under this machine's slug
// and removes the symlink aliases that scheme left behind. Claude Code lists
// ~/.claude/projects by isDirectory() once a project has git worktrees, so an
// alias is invisible to --resume and the /resume picker; only a real folder
// under the local slug is found. The "other side" is the hub; on the hub
// machine itself CLAUDE_SYNC_HUB_HOME names the satellite $HOME whose slug
// its old pushes carry.
//
// Folder contents are never touched (mv undoes a rename). One conflict, a real
// folder or a symlink to somewhere else already under a target name, stops the
// whole verb before anything moves: the names are printed and the exit status
// is 1. Merging two histories is a human's job. When both sides share one
// slug (the hub itself) there is nothing to rename and only the sweep of
// dangling symlinks remains.
func localize(cfg *config) error {
	pdir := filepath.Join(cfg.LRoot, "projects")
	entries, err := os.ReadDir(pdir)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Println("nothing to localize.")
		return nil
	}
	if err != nil {
		return err
	}

	type step struct {
		rename   bool   // else: remove a symlink
		name, to string // the rename target, or the symlink's destination for the report
	}
	var steps []step
	var conflicts []string
	srcReal := map[string]bool{} // resolved paths of the folders about to move
	aliasAt := map[string]bool{} // symlinks sitting under a target name, removed before the rename

	// pass 1: the renames; every target is checked before anything moves
	for _, e := range entries { // os.ReadDir sorts by name
		name := e.Name()
		if e.Type()&fs.ModeSymlink != 0 || !e.IsDir() || !strings.HasPrefix(name, cfg.HPrefix) {
			continue
		}
		to := sideName(name, cfg.HPrefix, cfg.LPrefix)
		if to == name {
			continue
		}
		resolved, err := filepath.EvalSymlinks(filepath.Join(pdir, name))
		if err != nil {
			return err
		}
		srcReal[resolved] = true
		tp := filepath.Join(pdir, to)
		st, err := os.Lstat(tp)
		if errors.Is(err, fs.ErrNotExist) {
			steps = append(steps, step{rename: true, name: name, to: to})
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode()&fs.ModeSymlink == 0 {
			conflicts = append(conflicts, fmt.Sprintf("%s and %s both exist", name, to))
			continue
		}
		// a symlink under the target name: the old alias (pointing here, or at
		// nothing any more) goes; one pointing at live data elsewhere is a conflict
		if dest, derr := filepath.EvalSymlinks(tp); derr == nil && dest != resolved {
			conflicts = append(conflicts, fmt.Sprintf("%s is a symlink to %s, not to %s", to, dest, name))
			continue
		}
		aliasAt[to] = true
		steps = append(steps, step{name: to, to: linkText(tp)}, step{rename: true, name: name, to: to})
	}

	// pass 2: symlinks that dangle now, or will once their folder has moved
	for _, e := range entries {
		name := e.Name()
		if e.Type()&fs.ModeSymlink == 0 || aliasAt[name] {
			continue
		}
		p := filepath.Join(pdir, name)
		if dest, err := filepath.EvalSymlinks(p); err == nil && !srcReal[dest] {
			continue // a live symlink to something that stays: not ours to judge
		}
		steps = append(steps, step{name: name, to: linkText(p)})
	}

	if len(conflicts) > 0 {
		for _, c := range conflicts {
			fmt.Fprintf(os.Stderr, "  conflict: %s\n", c)
		}
		return fmt.Errorf("%d naming conflict(s): merge by hand, nothing changed", len(conflicts))
	}
	if len(steps) == 0 {
		fmt.Println("nothing to localize.")
		return nil
	}
	would := ""
	if cfg.Dry {
		would = "would "
	}
	for _, s := range steps {
		if s.rename {
			fmt.Printf("  %srename: %s -> %s\n", would, s.name, s.to)
			if !cfg.Dry {
				if err := os.Rename(filepath.Join(pdir, s.name), filepath.Join(pdir, s.to)); err != nil {
					return err
				}
			}
			continue
		}
		fmt.Printf("  %sremove symlink: %s -> %s\n", would, s.name, s.to)
		if !cfg.Dry {
			if err := os.Remove(filepath.Join(pdir, s.name)); err != nil {
				return err
			}
		}
	}
	if cfg.Dry {
		fmt.Printf("localize: %d change(s) (dry-run: nothing changed)\n", len(steps))
		return nil
	}
	fmt.Printf("localize: %d change(s)\n", len(steps))
	return nil
}

func linkText(p string) string {
	t, err := os.Readlink(p)
	if err != nil {
		return "?"
	}
	return t
}
