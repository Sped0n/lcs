package toolchain

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAdmissionBoundary(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		eligible bool
	}{
		{artifactURL, true},
		{"https://chrome-infra-packages.appspot.com/dl/infra/tools/cipd/linux-amd64/+/" + strings.Repeat("a", 40), true},
		{"https://storage.googleapis.com/chrome-infra-packages/" + strings.Repeat("a", 40), true},
		{"https://chrome-infra-packages.appspot.com/client?platform=linux-amd64&version=latest&token=secret", false},
		{"https://chrome-infra-packages.appspot.com/client?platform=linux-amd64&platform=linux-arm64&version=latest", false},
		{"https://storage.googleapis.com/private-bucket/object", false},
		{"https://github.com/project-chip/connectedhomeip/archive/master.tar.gz", false},
		{"https://files.pythonhosted.org/packages/aa/bb/" + strings.Repeat("a", 60) + "/package.whl", false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			var calls atomic.Int64
			cache, store, _ := newCache(t, 10000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Cache-Control", "max-age=3600")
				writePayload(t, w)
			}))
			client := &http.Client{Transport: cache}
			for range 2 {
				if got := readResponse(t, client, tc.raw, nil); got != payload {
					t.Fatal(got)
				}
			}
			want := int64(2)
			if tc.eligible {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("upstream downloads=%d; want %d", calls.Load(), want)
			}
			stats, err := store.Stats()
			if err != nil {
				t.Fatal(err)
			}
			if !tc.eligible && stats.Toolchain != 0 {
				t.Fatal("unselected download was stored")
			}
		})
	}
}

func TestThirdPartyRedirectsBypass(t *testing.T) {
	for _, target := range []string{"https://github.com/arbitrary/file", "https://files.pythonhosted.org/arbitrary/file", "https://storage.googleapis.com.evil.test/object"} {
		t.Run(target, func(t *testing.T) {
			var calls atomic.Int64
			cache, store, _ := newCache(t, 10000, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/client" {
					w.Header().Set("Location", target)
					w.WriteHeader(302)
					return
				}
				w.Header().Set("Cache-Control", "max-age=3600")
				writePayload(t, w)
			}))
			client := &http.Client{Transport: cache}
			for range 2 {
				readResponse(t, client, artifactURL, nil)
			}
			stats, err := store.Stats()
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 4 || stats.Toolchain != 0 {
				t.Fatalf("redirect widened admission: calls=%d stats=%+v", calls.Load(), stats)
			}
		})
	}
}
