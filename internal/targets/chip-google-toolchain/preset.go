package toolchain

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

type redirectTarget struct{ expires time.Time }

type Preset struct {
	mu        sync.Mutex
	redirects map[string]redirectTarget
}

func NewPreset() *Preset { return &Preset{redirects: map[string]redirectTarget{}} }

var instanceID = regexp.MustCompile(`^[A-Za-z0-9_-]{40,64}$`)

// Identity admits CIPD artifact paths and exact redirects, not entire hosts.
func (p *Preset) Identity(u *url.URL) (string, bool) {
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.RawPath != "" || u.Port() != "" && u.Port() != "443" {
		return "", false
	}
	p.mu.Lock()
	target, known := p.redirects[u.String()]
	if known && time.Now().After(target.expires) {
		delete(p.redirects, u.String())
		known = false
	}
	p.mu.Unlock()
	valid := known
	if !valid {
		switch strings.ToLower(u.Hostname()) {
		case "chrome-infra-packages.appspot.com":
			if u.Path == "/client" {
				q, err := url.ParseQuery(u.RawQuery)
				valid = err == nil && len(q) == 2 && len(q["platform"]) == 1 && len(q["version"]) == 1 && (q.Get("platform") == "linux-amd64" || q.Get("platform") == "linux-arm64") && q.Get("version") != ""
			} else {
				valid = strings.HasPrefix(u.Path, "/dl/") && instanceID.MatchString(path.Base(u.Path)) && u.RawQuery == ""
			}
		case "storage.googleapis.com":
			valid = u.RawQuery == "" && strings.HasPrefix(u.Path, "/chrome-infra-packages/") && instanceID.MatchString(path.Base(u.Path))
		}
	}
	if !valid {
		return "", false
	}
	digest := sha256.Sum256([]byte(u.String()))
	return hex.EncodeToString(digest[:]), true
}

func (p *Preset) Intercept(host string) bool {
	switch host {
	case "chrome-infra-packages.appspot.com", "storage.googleapis.com":
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for raw, target := range p.redirects {
		if time.Now().After(target.expires) {
			delete(p.redirects, raw)
			continue
		}
		u, err := url.Parse(raw)
		if err == nil && u.Hostname() == host {
			return true
		}
	}
	return false
}

func (p *Preset) Learn(request *http.Request, response *http.Response) {
	if response.StatusCode < 300 || response.StatusCode > 399 || response.StatusCode == 304 || len(response.Header.Values("Set-Cookie")) != 0 || forbidden(response.Header) {
		return
	}
	u, err := request.URL.Parse(response.Header.Get("Location"))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Port() != "" && u.Port() != "443" {
		return
	}
	// Google may move public CIPD blobs between buckets. Never extend interception
	// to GitHub, package indexes, or arbitrary third-party redirect targets.
	host := strings.ToLower(u.Hostname())
	if host != "storage.googleapis.com" && host != "chrome-infra-packages.appspot.com" && !strings.HasSuffix(host, ".googleusercontent.com") {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for raw, target := range p.redirects {
		if time.Now().After(target.expires) {
			delete(p.redirects, raw)
		}
	}
	if len(p.redirects) >= 1024 {
		return
	}
	p.redirects[u.String()] = redirectTarget{expires: time.Now().Add(10 * time.Minute)}
}
