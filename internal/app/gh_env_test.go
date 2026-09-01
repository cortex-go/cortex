package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvValue(t *testing.T) {
	env := []string{"A=1", "B=two", "C="}
	if got := envValue(env, "B"); got != "two" {
		t.Fatalf("envValue(B)=%q want two", got)
	}
	if got := envValue(env, "C"); got != "" {
		t.Fatalf("envValue(C)=%q want empty", got)
	}
	if got := envValue(env, "D"); got != "" {
		t.Fatalf("envValue(D)=%q want empty", got)
	}
}

func TestGHConfigDirEnv(t *testing.T) {
	tmp := t.TempDir()
	mkdir(t, filepath.Join(tmp, "xdg", "gh"))
	writeFile(t, filepath.Join(tmp, "xdg", "gh", "hosts.yml"), "github.com:\n")
	mkdir(t, filepath.Join(tmp, "home", ".config", "gh"))
	writeFile(t, filepath.Join(tmp, "home", ".config", "gh", "hosts.yml"), "github.com:\n")
	mkdir(t, filepath.Join(tmp, "home2", ".config", "gh"))

	t.Run("respects explicit GH_CONFIG_DIR with hosts.yml", func(t *testing.T) {
		explicit := filepath.Join(tmp, "custom")
		mkdir(t, explicit)
		writeFile(t, filepath.Join(explicit, "hosts.yml"), "github.com:\n")
		env := []string{"GH_CONFIG_DIR=" + explicit}
		got := ghConfigDirEnv(env)
		if v := envValue(got, "GH_CONFIG_DIR"); v != explicit {
			t.Fatalf("explicit GH_CONFIG_DIR changed to %q", v)
		}
	})

	t.Run("drops explicit GH_CONFIG_DIR without hosts.yml", func(t *testing.T) {
		t.Setenv("HOME", filepath.Join(tmp, "nohome"))
		explicit := filepath.Join(tmp, "custom-empty")
		mkdir(t, explicit)
		env := []string{"GH_CONFIG_DIR=" + explicit}
		got := ghConfigDirEnv(env)
		if v := envValue(got, "GH_CONFIG_DIR"); v != "" {
			t.Fatalf("GH_CONFIG_DIR=%q passed without hosts.yml", v)
		}
	})

	t.Run("resolves from XDG_CONFIG_HOME", func(t *testing.T) {
		env := []string{"XDG_CONFIG_HOME=" + filepath.Join(tmp, "xdg")}
		got := ghConfigDirEnv(env)
		if v := envValue(got, "GH_CONFIG_DIR"); v != filepath.Join(tmp, "xdg", "gh") {
			t.Fatalf("GH_CONFIG_DIR=%q want %q", v, filepath.Join(tmp, "xdg", "gh"))
		}
		if v := envValue(got, "XDG_CONFIG_HOME"); v != filepath.Join(tmp, "xdg") {
			t.Fatalf("XDG_CONFIG_HOME was altered to %q", v)
		}
	})

	t.Run("resolves from HOME when XDG unset", func(t *testing.T) {
		t.Setenv("HOME", filepath.Join(tmp, "home"))
		env := []string{"PATH=/bin"}
		got := ghConfigDirEnv(env)
		if v := envValue(got, "GH_CONFIG_DIR"); v != filepath.Join(tmp, "home", ".config", "gh") {
			t.Fatalf("GH_CONFIG_DIR=%q want %q", v, filepath.Join(tmp, "home", ".config", "gh"))
		}
	})

	t.Run("does not add GH_CONFIG_DIR when hosts.yml is missing", func(t *testing.T) {
		t.Setenv("HOME", filepath.Join(tmp, "home2"))
		env := []string{"PATH=/bin"}
		got := ghConfigDirEnv(env)
		if v := envValue(got, "GH_CONFIG_DIR"); v != "" {
			t.Fatalf("GH_CONFIG_DIR added (%q) without hosts.yml", v)
		}
	})

	t.Run("does not alter an empty env", func(t *testing.T) {
		t.Setenv("HOME", filepath.Join(tmp, "nohome"))
		got := ghConfigDirEnv(nil)
		if v := envValue(got, "GH_CONFIG_DIR"); v != "" {
			t.Fatalf("GH_CONFIG_DIR=%q on empty env", v)
		}
	})
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
