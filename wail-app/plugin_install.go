package main

import (
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
)

// pluginBundles are the CLAP plugins WAIL ships and auto-installs on first launch
// (ADR-0007 Link Bridge). CLAP-only: VST3/AU are a future clap-wrapper target,
// not shipped here. Dev tools (transport-probe, linkbridge-spike) are built but
// deliberately NOT shipped or installed.
var pluginBundles = []string{
	"wail-linkbridge-send.clap",
	"wail-linkbridge-recv.clap",
}

// SystemPluginDir returns the per-user CLAP directory DAWs scan by default. The
// per-user location means first-launch install needs no administrator rights.
func SystemPluginDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Audio", "Plug-Ins", "CLAP"), nil
	case "linux":
		return filepath.Join(home, ".clap"), nil
	case "windows":
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" {
			localAppData = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(localAppData, "Programs", "Common", "CLAP"), nil
	default:
		return "", fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

// FindPluginDir locates bundled plugin files, checking resourceDir/plugins/ then
// {exe}/../lib/. Returns "" if none are found (e.g. a dev build that didn't bundle
// them) — auto-install then simply does nothing.
func FindPluginDir(resourceDir string) string {
	var candidates []string
	if resourceDir != "" {
		candidates = append(candidates, filepath.Join(resourceDir, "plugins"))
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "lib"))
	}
	for _, dir := range candidates {
		if hasPlugins(dir) {
			return dir
		}
	}
	return ""
}

func hasPlugins(dir string) bool {
	for _, name := range pluginBundles {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// InstallPluginsIfMissing copies any bundled CLAP plugins not already present in the
// system CLAP directory. Returns human-readable errors (empty on success) so the UI
// can point the user at the manual-install fallback. Unlike the retired plugin era
// there is no companion opus.dll to co-locate — the thin plugin has no codec.
func InstallPluginsIfMissing(pluginDir string) []string {
	destDir, err := SystemPluginDir()
	if err != nil {
		return []string{fmt.Sprintf("resolve CLAP dir: %v", err)}
	}
	var errs []string
	for _, name := range pluginBundles {
		src := filepath.Join(pluginDir, name)
		if _, err := os.Stat(src); err != nil {
			continue // not bundled in this build
		}
		dest := filepath.Join(destDir, name)
		if info, err := os.Lstat(dest); err == nil {
			if info.Mode()&os.ModeSymlink == 0 {
				continue // already installed
			}
			// A symlink here is stale: Homebrew's relative Cellar links break
			// when copied into the CLAP folder, and links into the Cellar go
			// dead on `brew uninstall`. Replace with a real copy.
			if err := os.Remove(dest); err != nil {
				errs = append(errs, fmt.Sprintf("%s: remove stale symlink: %v", name, err))
				continue
			}
		}
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			errs = append(errs, fmt.Sprintf("%s: create dir: %v", name, err))
			continue
		}
		if err := copyPath(src, dest); err != nil {
			errs = append(errs, fmt.Sprintf("%s: copy: %v", name, err))
			continue
		}
		log.Printf("[plugin-install] installed %s to %s", name, destDir)
	}
	return errs
}

// copyPath copies a file or directory recursively, resolving symlinks on src so
// Homebrew-linked bundles (symlinks into the Cellar) are copied correctly.
func copyPath(src, dst string) error {
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyDir(resolved, dst)
	}
	return copyFile(resolved, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if info, err := os.Stat(src); err == nil {
		os.Chmod(dst, info.Mode())
	}
	return nil
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}
