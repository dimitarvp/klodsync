package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func timeISO(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hubScript runs a POSIX sh script on the hub with HUBROOT/HUBJSON set. Data
// travels through files and rsync, never through the script text, so nothing
// read from a machine can be eaten as code.
func hubScript(cfg *config, script string) (string, error) {
	var cmd *exec.Cmd
	if cfg.Remote {
		remote := fmt.Sprintf("HUBROOT='%s' HUBJSON='%s' sh -c %s", cfg.HRoot, cfg.HJSON, shellQuote(script))
		cmd = exec.Command("ssh", append(cfg.SSHOpts, cfg.HHost, remote)...)
	} else {
		cmd = exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "HUBROOT="+cfg.HRoot, "HUBJSON="+cfg.HJSON)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("hub script failed: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func hubPerl(cfg *config, program string) (string, error) {
	var cmd *exec.Cmd
	if cfg.Remote {
		cmd = exec.Command("ssh", append(cfg.SSHOpts, cfg.HHost, fmt.Sprintf("MROOT='%s' perl -", cfg.HRoot))...)
	} else {
		cmd = exec.Command("perl", "-")
		cmd.Env = append(os.Environ(), "MROOT="+cfg.HRoot)
	}
	cmd.Stdin = strings.NewReader(program)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("hub manifest failed: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func hubArg(cfg *config) string {
	if cfg.Remote {
		return cfg.HHost + ":" + cfg.HRoot
	}
	return cfg.HRoot
}

func rsyncE(cfg *config) []string {
	if cfg.Remote {
		return []string{"-e", "ssh " + strings.Join(cfg.SSHOpts, " ")}
	}
	return nil
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// rsyncFiles transfers the relpaths in list (one per line) from src root to
// dst root. Locks and .DS_Store never travel, matching the manifests.
func rsyncFiles(cfg *config, listPath, src, dst string) error {
	args := append([]string{"-a", "-r", "--exclude=.DS_Store", "--exclude=*.lock", "--files-from=" + listPath}, rsyncE(cfg)...)
	return runCmd("rsync", append(args, src+"/", dst+"/")...)
}

func rsyncOne(cfg *config, src, dst string) error {
	args := append([]string{"-a"}, rsyncE(cfg)...)
	return runCmd("rsync", append(args, src, dst)...)
}

// ── deletions ────────────────────────────────────────────────────

var (
	projDelRe = regexp.MustCompile(`^projects/[^/]+/.+`)
	satDelRe  = regexp.MustCompile(`^(file-history|tasks|session-env)/[0-9a-fA-F-]{36}/$`)
)

// validateDeletes fails closed: one unexpected path aborts the whole side.
func validateDeletes(paths []string) error {
	bad := false
	for _, p := range paths {
		if strings.HasPrefix(p, "/") || strings.Contains(p, "..") ||
			!projDelRe.MatchString(p) && !satDelRe.MatchString(p) {
			fmt.Fprintf(os.Stderr, "REFUSING unexpected delete path: %s\n", p)
			bad = true
		}
	}
	if bad {
		return fmt.Errorf("unexpected delete paths")
	}
	return nil
}

func deleteOnHub(cfg *config, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if err := validateDeletes(paths); err != nil {
		return fmt.Errorf("aborting deletions on hub: %w", err)
	}
	var b bytes.Buffer
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte(0)
	}
	if cfg.Remote {
		cmd := exec.Command("ssh", append(cfg.SSHOpts, cfg.HHost,
			fmt.Sprintf("cd '%s' && xargs -0 rm -rf --", cfg.HRoot))...)
		cmd.Stdin = &b
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("hub delete failed: %w", err)
		}
	} else {
		for _, p := range paths {
			if err := os.RemoveAll(filepath.Join(cfg.HRoot, p)); err != nil {
				return err
			}
		}
	}
	fmt.Printf("  deleted %d path(s) on hub\n", len(paths))
	return nil
}

func deleteLocal(cfg *config, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if err := validateDeletes(paths); err != nil {
		return fmt.Errorf("aborting deletions on local: %w", err)
	}
	for _, p := range paths {
		if err := os.RemoveAll(filepath.Join(cfg.LRoot, p)); err != nil {
			return err
		}
	}
	fmt.Printf("  deleted %d path(s) on local\n", len(paths))
	return nil
}
