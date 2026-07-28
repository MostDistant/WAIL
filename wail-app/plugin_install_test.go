package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// makeFakeBundle creates a minimal .clap bundle (dir with one file) under dir.
func makeFakeBundle(t *testing.T, dir, name string) string {
	t.Helper()
	bundle := filepath.Join(dir, name)
	inner := filepath.Join(bundle, "Contents", "MacOS")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, name[:len(name)-len(".clap")]), []byte("mach-o"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func clapDestDir(t *testing.T) string {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(os.Getenv("HOME"), "Library", "Audio", "Plug-Ins", "CLAP")
	case "linux":
		return filepath.Join(os.Getenv("HOME"), ".clap")
	default:
		t.Skipf("unsupported test platform: %s", runtime.GOOS)
		return ""
	}
}

// staleLinkBridgeName matches an unprefixed linkbridge-send/-recv bundle name —
// the pre-rename spelling. The leading class rejects "wail-linkbridge-send" and
// the underscore source filenames (linkbridge_send.c).
var staleLinkBridgeName = regexp.MustCompile(`(?m)(^|[^-\w])linkbridge-(send|recv)`)

// The bundle list is duplicated across the build, the two installers, and the
// release workflow. A rename that misses one of them ships silently: the Homebrew
// formula's brace glob dropped both renamed Link Bridge bundles and still
// succeeded, so macOS users got two of four plugins with no error anywhere.
func TestPluginBundleListsInSync(t *testing.T) {
	files := []string{
		"homebrew/wail.rb",
		"scripts/wail-install-plugins.sh",
		"plugins/CMakeLists.txt",
		".github/workflows/release.yml",
	}
	for _, rel := range files {
		t.Run(rel, func(t *testing.T) {
			// ".." is the repo root; absent when the module is extracted alone.
			data, err := os.ReadFile(filepath.Join("..", rel))
			if err != nil {
				t.Skipf("not in a repo checkout: %v", err)
			}
			body := string(data)
			for _, bundle := range pluginBundles {
				// CMake targets and the workflow's shell loop name bundles without
				// the extension, so match on the base name.
				name := strings.TrimSuffix(bundle, ".clap")
				if !strings.Contains(body, name) {
					t.Errorf("%s does not mention bundle %q", rel, name)
				}
			}
			if m := staleLinkBridgeName.FindString(body); m != "" {
				t.Errorf("%s uses the pre-rename bundle name %q; expected the wail- prefix", rel, strings.TrimSpace(m))
			}
		})
	}
}

func TestInstallPluginsFreshInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pluginDir := t.TempDir()
	makeFakeBundle(t, pluginDir, "wail-send.clap")

	if errs := InstallPluginsIfMissing(pluginDir); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	dest := filepath.Join(clapDestDir(t), "wail-send.clap")
	info, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("dest not installed: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("dest should be a real directory, got mode %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(dest, "Contents", "MacOS", "wail-send")); err != nil {
		t.Fatalf("bundle contents missing: %v", err)
	}
}

// A previous wail-install-plugins.sh bug copied Homebrew's relative Cellar
// symlink verbatim, leaving a broken symlink in the CLAP folder. The app
// installer must replace such a symlink with a real copy instead of erroring.
func TestInstallPluginsReplacesBrokenSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pluginDir := t.TempDir()
	makeFakeBundle(t, pluginDir, "wail-send.clap")

	destDir := clapDestDir(t)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(destDir, "wail-send.clap")
	// Relative symlink, broken from the CLAP folder's perspective — exactly
	// what the buggy script produced (../Cellar/wail/<ver>/lib/...).
	if err := os.Symlink(filepath.Join("..", "Cellar", "wail", "0.0.0", "lib", "wail-send.clap"), dest); err != nil {
		t.Fatal(err)
	}

	if errs := InstallPluginsIfMissing(pluginDir); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	info, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("dest not installed: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("broken symlink was not replaced with a real copy")
	}
	if _, err := os.Stat(filepath.Join(dest, "Contents", "MacOS", "wail-send")); err != nil {
		t.Fatalf("bundle contents missing: %v", err)
	}
}
