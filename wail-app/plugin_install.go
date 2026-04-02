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
		commonFiles := os.Getenv("COMMONPROGRAMFILES")
		if commonFiles == "" {
			commonFiles = filepath.Join("C:", "Program Files", "Common Files")
		}
		return filepath.Join(commonFiles, pluginDirName(format)), nil
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
		} else {
			log.Printf("[plugin-install] Installed %s to %s", p.name, destDir)
		}
	}
	return errors
}

// copyPath copies a file or directory recursively.
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return copyDir(src, dst)
	}
	return copyFile(src, dst)
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
