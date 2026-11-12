package agent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/EvansLeeSin/stardew-sync/internal/savefmt"
)

// Platform abstracts device specific operations (mocked in tests).
type Platform interface {
	GameRunning() bool
	GameVersion() string
	// Apply atomically replaces <savesDir>/<id> with the archive content.
	Apply(savesDir, id string, arc *savefmt.Archive) error
	Notify(title, text string)
}

// AndroidPlatform is the real implementation used on the device.
type AndroidPlatform struct {
	Package string
}

// GameRunning scans /proc/*/cmdline for the game package.
func (p *AndroidPlatform) GameRunning() bool {
	matches, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	want := []byte(p.Package)
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil || len(b) == 0 {
			continue
		}
		if i := bytes.IndexByte(b, 0); i >= 0 {
			b = b[:i]
		}
		if bytes.Equal(b, want) || bytes.HasPrefix(b, append(append([]byte{}, want...), ':')) {
			return true
		}
	}
	return false
}

var versionNameRe = regexp.MustCompile(`versionName=([^\s]+)`)

// GameVersion returns the installed game versionName via dumpsys.
func (p *AndroidPlatform) GameVersion() string {
	out, err := exec.Command("dumpsys", "package", p.Package).Output()
	if err != nil {
		return ""
	}
	if m := versionNameRe.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// Notify posts a system notification via `cmd notification` (best effort).
func (p *AndroidPlatform) Notify(title, text string) {
	cmd := exec.Command("cmd", "notification", "post", "-S", "bigtext", "-t", title, "stardew-sync", text)
	if err := cmd.Run(); err != nil {
		// Some ROMs require the shell uid.
		exec.Command("su", "2000", "-c", fmt.Sprintf("cmd notification post -S bigtext -t %q stardew-sync %q", title, text)).Run()
	}
}

func (p *AndroidPlatform) Apply(savesDir, id string, arc *savefmt.Archive) error {
	return applyReplace(savesDir, id, arc)
}

// applyReplace writes the archive into a temp dir on the same filesystem and
// swaps it in with renames. Ownership/permissions/SELinux labels are copied
// from the existing save folder (or the Saves folder) by fixOwnership.
func applyReplace(savesDir, id string, arc *savefmt.Archive) error {
	st, err := os.Stat(savesDir)
	if err != nil {
		return fmt.Errorf("存档目录不存在（请先启动一次游戏）: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", savesDir)
	}
	target := filepath.Join(savesDir, id)
	ref := savesDir
	var refFile string
	if fi, err := os.Stat(target); err == nil && fi.IsDir() {
		ref = target
		if _, err := os.Stat(filepath.Join(target, id)); err == nil {
			refFile = filepath.Join(target, id)
		}
	}
	tmpRoot := filepath.Join(filepath.Dir(savesDir), ".stardew-sync-tmp")
	os.RemoveAll(tmpRoot)
	if err := os.MkdirAll(tmpRoot, 0o770); err != nil {
		return err
	}
	defer os.RemoveAll(tmpRoot)
	newDir := filepath.Join(tmpRoot, id)
	if err := os.Mkdir(newDir, 0o770); err != nil {
		return err
	}
	if err := arc.WriteFiles(newDir); err != nil {
		return err
	}
	names := make([]string, 0, len(arc.Files))
	for n := range arc.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	if err := fixOwnership(ref, refFile, newDir, names); err != nil {
		return fmt.Errorf("fix ownership: %w", err)
	}
	oldDir := filepath.Join(tmpRoot, id+".old")
	hadOld := false
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, oldDir); err != nil {
			return err
		}
		hadOld = true
	}
	if err := os.Rename(newDir, target); err != nil {
		if hadOld {
			os.Rename(oldDir, target)
		}
		return err
	}
	return nil
}

// Backup zips the current local save into <dataDir>/backups, keeping the newest keep files.
func Backup(dataDir, savesDir, id, reason string, keep int) (string, error) {
	dir := filepath.Join(savesDir, id)
	if _, err := os.Stat(dir); err != nil {
		return "", nil
	}
	data, err := savefmt.PackDir(dir)
	if err != nil {
		return "", err
	}
	bdir := filepath.Join(dataDir, "backups")
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s-%s.zip", id, time.Now().Format("20060102-150405.000"), reason)
	p := filepath.Join(bdir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(bdir)
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".zip") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files) // timestamped names sort chronologically per save id
	for len(files) > keep {
		os.Remove(filepath.Join(bdir, files[0]))
		files = files[1:]
	}
	return p, nil
}
