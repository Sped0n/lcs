package cli

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func isolatePaths(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("STATE_DIRECTORY", "")
	t.Setenv("CACHE_DIRECTORY", "")

	return root
}

func TestServeConfigOverride(t *testing.T) {
	root := isolatePaths(t)
	path := filepath.Join(root, "custom config.toml")

	for _, flags := range [][]string{{"--config", path}, {"--config=" + path}} {
		var out, diagnostics bytes.Buffer
		err := Run(t.Context(), append([]string{"serve"}, flags...), &out, &diagnostics)

		var pathErr *os.PathError
		if !errors.As(err, &pathErr) || pathErr.Path != path || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("serve %v did not read the requested config: %v", flags, err)
		}
		if out.Len() != 0 {
			t.Fatalf("failed command wrote stdout: %q", out.String())
		}
	}
}

func TestShellUsesDaemon(t *testing.T) {
	root := isolatePaths(t)
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	localConfig := filepath.Join(root, "config", "lcs", "config.toml")
	if err := os.MkdirAll(filepath.Dir(localConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localConfig, []byte("not valid TOML"), 0o600); err != nil {
		t.Fatal(err)
	}

	state := filepath.Join(root, "state", "lcs")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("unix", filepath.Join(state, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}

	daemon := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/shell" {
			t.Errorf("daemon received %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"Repositories":["https://example.com/owner/selected.git"]}`)); err != nil {
			t.Error(err)
		}
	})}

	done := make(chan error, 1)
	go func() { done <- daemon.Serve(listener) }()
	t.Cleanup(func() {
		if err := daemon.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})

	var out, diagnostics bytes.Buffer
	if err := Run(t.Context(), []string{"shell"}, &out, &diagnostics); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("bash", "-c", out.String()+"\ngit config --get 'url.http://127.0.0.1:7777/git/example.com/owner/selected.git.insteadOf'")
	result, err := command.CombinedOutput()
	if err != nil || strings.TrimSpace(string(result)) != "https://example.com/owner/selected.git" {
		t.Fatalf("daemon activation: %v: %s", err, result)
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localConfig, []byte("cache_size = '1MB'\ncache = ['git:https://example.com/owner/local.git']\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := Run(t.Context(), []string{"shell"}, &out, &diagnostics); err == nil || out.Len() != 0 {
		t.Fatalf("activation without daemon: error=%v stdout=%q", err, out.String())
	}
}

func TestInvalidArguments(t *testing.T) {
	isolatePaths(t)

	for _, args := range [][]string{
		{"serve", "--config"},
		{"serve", "--unknown"},
		{"serve", "extra"},
		{"shell", "--config", "custom.toml"},
		{"status", "--config", "custom.toml"},
		{"clear", "extra"},
	} {
		var out, diagnostics bytes.Buffer
		if err := Run(t.Context(), args, &out, &diagnostics); err == nil || out.Len() != 0 {
			t.Fatalf("%v: error=%v stdout=%q", args, err, out.String())
		}
	}
}
