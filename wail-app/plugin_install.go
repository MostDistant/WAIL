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

// Plugin bundles we expect to find and install.
var pluginBundles = []struct {
	name    string
	format  string // "clap" or "vst3"
}{
	{"wail-plugin-send.clap", "clap"},
	{"wail-plugin-recv.clap", "clap"},
	{"wail-plugin-send.vst3", "vst3"},
	{"wail-plugin-recv.vst3", "vst3"},
}

// SystemPluginDir returns the system plugin directory for a given format.
func SystemPluginDir(format string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Audio", "Plug-Ins", pluginDirName(format)), nil
	case "linux":
		if format == "clap" {
			return filepath.Join(home, ".clap"), nil
		}
		return filepath.Join(home, ".vst3"), nil
	case "windows":
		// Use the per-user "Programs\Common" location so first-launch install
		// works without Administrator rights. DAWs scan this path by default.
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" {
			localAppData = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(localAppData, "Programs", "Common", pluginDirName(format)), nil
	default:
		return "", fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func pluginDirName(format string) string {
	switch format {
	case "clap":
		return "CLAP"
	case "vst3":
		return "VST3"
	default:
		return format
	}
}

// FindPluginDir searches for bundled plugin files.
// Checks: resourceDir/plugins/, then {exe}/../lib/.
func FindPluginDir(resourceDir string) string {
	candidates := []string{}
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
	for _, p := range pluginBundles {
		path := filepath.Join(dir, p.name)
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// InstallPluginsIfMissing copies plugin bundles to system directories.
// Returns a list of errors (empty if all succeeded).
func InstallPluginsIfMissing(pluginDir string) []string {
	var errors []string
	installedClap := false
	for _, p := range pluginBundles {
		src := filepath.Join(pluginDir, p.name)
		if _, err := os.Stat(src); err != nil {
			continue // bundle not found, skip
		}
		destDir, err := SystemPluginDir(p.format)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", p.name, err))
			continue
		}
		dest := filepath.Join(destDir, p.name)
		if _, err := os.Stat(dest); err == nil {
			continue // already installed
		}
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			errors = append(errors, fmt.Sprintf("%s: create dir: %v", p.name, err))
			continue
		}
		if err := copyPath(src, dest); err != nil {
			errors = append(errors, fmt.Sprintf("%s: copy: %v", p.name, err))
			continue
		}
		log.Printf("[plugin-install] Installed %s to %s", p.name, destDir)
		if p.format == "clap" {
			installedClap = true
		}
	}

	// On Windows the CLAP plugin is a single .dll that depends on opus.dll.
	// VST3 bundles already carry opus.dll in their Contents folder, but CLAP
	// has no bundle layout — drop opus.dll alongside the .clap files so the
	// DAW's LoadLibraryEx finds it via the altered search path.
	if runtime.GOOS == "windows" && installedClap {
		opusSrc := filepath.Join(pluginDir, "opus.dll")
		if _, err := os.Stat(opusSrc); err == nil {
			if clapDir, err := SystemPluginDir("clap"); err == nil {
				opusDest := filepath.Join(clapDir, "opus.dll")
				if _, err := os.Stat(opusDest); err != nil {
					if err := copyFile(opusSrc, opusDest); err != nil {
						errors = append(errors, fmt.Sprintf("opus.dll: copy: %v", err))
					} else {
						log.Printf("[plugin-install] Installed opus.dll to %s", clapDir)
					}
				}
			}
		}
	}

	return errors
}

// copyPath copies a file or directory recursively.
// Resolves symlinks on src so that Homebrew-linked plugin bundles
// (symlinks in /opt/homebrew/lib/ → Cellar) are copied correctly.
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
	// Preserve executable permissions
	info, _ := os.Stat(src)
	if info != nil {
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
