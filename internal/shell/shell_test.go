package shell

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spedon/lcs/internal/config"
)

func TestActivation(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			binary, err := exec.LookPath(shell)
			if err != nil {
				t.Skip(err)
			}
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "user.name")
			t.Setenv("GIT_CONFIG_VALUE_0", "O'Brien; $(false)")
			t.Setenv("NO_PROXY", "example.com")
			t.Setenv("no_proxy", "internal.test")
			c := config.Config{Toolchain: true, Repositories: []string{"https://github.com/a/b.git"}}
			paths := config.Paths{State: filepath.Join(t.TempDir(), "O'Brien")}
			var out, diagnostics bytes.Buffer
			if err := Generate(c, paths, &out, &diagnostics); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(diagnostics.String(), "WARNING") || strings.Contains(out.String(), "WARNING") {
				t.Fatal("warning on wrong stream")
			}
			activation := out.String()
			command := exec.Command(binary, "-c", activation+"\nprintf '%s\\n' \"$GIT_CONFIG_COUNT\" \"$GIT_CONFIG_VALUE_0\" \"$NO_PROXY\" \"$https_proxy\"")
			result, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("eval: %v: %s", err, result)
			}
			if !strings.Contains(string(result), "2\nO'Brien; $(false)\n") || !strings.Contains(string(result), "example.com,127.0.0.1") {
				t.Fatalf("environment: %s", result)
			}
			t.Setenv("GIT_CONFIG_COUNT", "2")
			t.Setenv("GIT_CONFIG_KEY_1", "url."+config.Endpoint+"/git/github.com/a/b.git.insteadOf")
			t.Setenv("GIT_CONFIG_VALUE_1", "https://github.com/a/b.git")
			out.Reset()
			if err := Generate(c, paths, &out, &diagnostics); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "GIT_CONFIG_KEY_2") {
				t.Fatal("activation is not idempotent")
			}
			if _, err := os.Stat(paths.State); !os.IsNotExist(err) {
				t.Fatal("activation modified filesystem")
			}
		})
	}
}

func TestInvalidCountWritesNoStdout(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "-1")
	var out, diag bytes.Buffer
	if err := Generate(config.Config{}, config.Paths{State: t.TempDir()}, &out, &diag); err == nil || out.Len() != 0 {
		t.Fatalf("error=%v stdout=%q", err, out.String())
	}
}
