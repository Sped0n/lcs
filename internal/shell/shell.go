package shell

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/spedon/lcs/internal/cert"
	"github.com/spedon/lcs/internal/config"
)

func Quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func Generate(c config.Config, paths config.Paths, stdout, stderr io.Writer) error {
	trusted := false
	var err error
	if c.Toolchain {
		trusted, err = cert.Trusted(paths.State)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	bundle := "/etc/ssl/certs/ca-certificates.crt"
	if trusted {
		for _, name := range []string{"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "SSL_CERT_FILE", "PIP_CERT"} {
			if value := os.Getenv(name); value != "" && !bundleTrusts(value, filepath.Join(paths.State, "ca.crt")) {
				if _, err := fmt.Fprintf(stderr, "WARNING: %s does not trust the LCS CA. HTTPS interception is disabled.\n", name); err != nil {
					return err
				}
				trusted = false
				break
			}
		}
		if trusted && !bundleTrusts(bundle, filepath.Join(paths.State, "ca.crt")) {
			trusted = false
		}
	}
	if c.Toolchain && !trusted {
		warning := "WARNING: LCS CA is not trusted. HTTPS requests will use a tunnel."
		if f, ok := stderr.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			warning = "\x1b[33m" + warning + "\x1b[0m"
		}
		if _, err := fmt.Fprintf(stderr, "%s\n\nInstall the certificate on Ubuntu/Debian:\n\n  sudo install -m 0644 %s /usr/local/share/ca-certificates/lcs.crt\n  sudo update-ca-certificates\n\nThen run:\n\n  eval \"$(lcsctl shell)\"\n", warning, Quote(filepath.Join(paths.State, "ca.crt"))); err != nil {
			return err
		}
	}
	var out bytes.Buffer
	export := func(key, value string) { fmt.Fprintf(&out, "export %s=%s\n", key, Quote(value)) }
	proxy := config.Endpoint
	if trusted {
		proxy = "http://lcs-intercept:1@" + config.Listen
	}
	for _, key := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY"} {
		export(key, proxy)
	}
	exclusions := []string{}
	seen := map[string]bool{}
	for _, value := range []string{os.Getenv("no_proxy"), os.Getenv("NO_PROXY"), "127.0.0.1,localhost,::1"} {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part != "" && !seen[part] {
				exclusions = append(exclusions, part)
				seen[part] = true
			}
		}
	}
	for _, key := range []string{"no_proxy", "NO_PROXY"} {
		export(key, strings.Join(exclusions, ","))
	}
	if trusted {
		for _, key := range []string{"REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "SSL_CERT_FILE", "PIP_CERT"} {
			if os.Getenv(key) == "" {
				export(key, bundle)
			}
		}
	}
	count := 0
	if value := os.Getenv("GIT_CONFIG_COUNT"); value != "" {
		count, err = strconv.Atoi(value)
		if err != nil || count < 0 || count > 10000 {
			return fmt.Errorf("invalid GIT_CONFIG_COUNT %q", value)
		}
	}
	type pair struct{ key, value string }
	var pairs []pair
	for _, repository := range c.Repositories {
		rest := strings.TrimPrefix(repository, "https://")
		pairs = append(pairs, pair{"url." + config.Endpoint + "/git/" + rest + ".insteadOf", repository})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	for _, p := range pairs {
		found := false
		for i := range count {
			key := os.Getenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", i))
			value := os.Getenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i))
			if key == p.key && value == p.value {
				found = true
				break
			}
		}
		if !found {
			export(fmt.Sprintf("GIT_CONFIG_KEY_%d", count), p.key)
			export(fmt.Sprintf("GIT_CONFIG_VALUE_%d", count), p.value)
			count++
		}
	}
	export("GIT_CONFIG_COUNT", strconv.Itoa(count))
	_, err = stdout.Write(out.Bytes())
	return err
}

func bundleTrusts(path, caPath string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return false
	}
	data, err = os.ReadFile(caPath)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return false
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	_, err = crt.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err == nil
}
