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

			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
			t.Setenv("GIT_CONFIG_COUNT", "2")
			t.Setenv("GIT_CONFIG_KEY_0", "user.name")
			t.Setenv("GIT_CONFIG_VALUE_0", "O'Brien; $(false)")
			t.Setenv("GIT_CONFIG_KEY_1", "http.proxy")
			t.Setenv("GIT_CONFIG_VALUE_1", "http://corporate.invalid:8080")
			t.Setenv("NO_PROXY", "example.com")
			t.Setenv("no_proxy", "internal.test")
			t.Setenv("https_proxy", "http://corporate.invalid:8080")
			t.Setenv("REQUESTS_CA_BUNDLE", "/custom/roots.pem")

			home := filepath.Join(t.TempDir(), "O'Brien")
			t.Setenv("HOME", home)

			c := config.Config{Repositories: []string{"https://github.com/a/o'b.git"}}
			var out bytes.Buffer
			if err := Generate(c, &out); err != nil {
				t.Fatal(err)
			}

			command := exec.Command(binary, "-c", out.String()+"\nprintf '%s\\n' \"$GIT_CONFIG_COUNT\" \"$GIT_CONFIG_VALUE_0\" \"$NO_PROXY\" \"$no_proxy\" \"$https_proxy\" \"$REQUESTS_CA_BUNDLE\" \"$GIT_CONFIG_VALUE_3\"\ngit config --get-urlmatch http.proxy http://127.0.0.1:7777/git/github.com/a/repo.git\ngit config --get-urlmatch http.proxy https://github.com/a/repo.git")
			result, err := command.CombinedOutput()
			want := "4\nO'Brien; $(false)\nexample.com\ninternal.test\nhttp://corporate.invalid:8080\n/custom/roots.pem\nhttps://github.com/a/o'b.git\n\nhttp://corporate.invalid:8080\n"
			if err != nil || string(result) != want {
				t.Fatalf("activation: %v: %s", err, result)
			}

			t.Setenv("GIT_CONFIG_COUNT", "4")
			t.Setenv("GIT_CONFIG_KEY_2", "http."+config.Endpoint+"/git/.proxy")
			t.Setenv("GIT_CONFIG_VALUE_2", "")
			t.Setenv("GIT_CONFIG_KEY_3", "url."+config.Endpoint+"/git/github.com/a/o'b.git.insteadOf")
			t.Setenv("GIT_CONFIG_VALUE_3", "https://github.com/a/o'b.git")
			out.Reset()
			if err := Generate(c, &out); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "GIT_CONFIG_KEY_4") {
				t.Fatal("activation is not idempotent")
			}

			if _, err := os.Stat(home); !os.IsNotExist(err) {
				t.Fatal("activation modified the home directory")
			}
		})
	}
}

func TestActivationClearsOnlyLegacyLCSProxies(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("http_proxy", config.Endpoint)
	t.Setenv("HTTPS_PROXY", "http://lcs-intercept:1@"+config.Listen)
	t.Setenv("HTTP_PROXY", "http://corporate.invalid:8080")
	t.Setenv("https_proxy", "http://corporate.invalid:8080")

	var out bytes.Buffer
	if err := Generate(config.Config{}, &out); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("bash", "-c", out.String()+"\nprintf '%s\\n' \"${http_proxy+set}\" \"${HTTPS_PROXY+set}\" \"$HTTP_PROXY\" \"$https_proxy\"")
	result, err := command.CombinedOutput()
	if err != nil || string(result) != "\n\nhttp://corporate.invalid:8080\nhttp://corporate.invalid:8080\n" {
		t.Fatalf("legacy proxy cleanup: %v: %q", err, result)
	}
}

func TestInvalidCountWritesNoStdout(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "-1")

	var out bytes.Buffer
	if err := Generate(config.Config{}, &out); err == nil || out.Len() != 0 {
		t.Fatalf("error=%v stdout=%q", err, out.String())
	}
}
