package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWebFrameRedirectBoundary(t *testing.T) {
	var privateHits atomic.Int32
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		privateHits.Add(1)
		fmt.Fprint(w, "PRIVATE_FIXTURE_SENTINEL")
	}))
	defer private.Close()
	for _, mode := range []string{"direct-other-host", "direct-other-port", "redirect-other-host", "redirect-other-port", "redirect-credentials", "allowed", "loop"} {
		t.Run(mode, func(t *testing.T) {
			privateHits.Store(0)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				switch r.URL.Path {
				case "/":
					src := "/hop"
					if mode == "direct-other-host" {
						src = strings.Replace(private.URL, "127.0.0.1", "localhost", 1)
					} else if mode == "direct-other-port" {
						src = private.URL
					}
					fmt.Fprintf(w, `<p>Public fixture</p><iframe src="%s"></iframe>`, src)
				case "/hop":
					http.Redirect(w, r, "/next", http.StatusFound)
				case "/next":
					target := private.URL
					switch mode {
					case "redirect-other-host":
						target = strings.Replace(target, "127.0.0.1", "localhost", 1)
					case "redirect-credentials":
						target = "http://fixture:fixture@" + r.Host + "/content"
					case "allowed":
						target = "/content"
					case "loop":
						target = "/hop"
					}
					http.Redirect(w, r, target, http.StatusFound)
				case "/content":
					fmt.Fprint(w, "ALLOWED_FRAME_SENTINEL")
				}
			}))
			defer server.Close()
			result, err := directWebFetch(context.Background(), server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if privateHits.Load() != 0 || strings.Contains(result.Content, "PRIVATE_FIXTURE_SENTINEL") {
				t.Fatal("automatic frame reached a disallowed service")
			}
			if got := strings.Contains(result.Content, "ALLOWED_FRAME_SENTINEL"); got != (mode == "allowed") {
				t.Fatalf("frame content included=%v for mode %s", got, mode)
			}
		})
	}
}

func TestWebFrameOriginPolicy(t *testing.T) {
	for _, tc := range []struct {
		parent, child string
		allowed       bool
	}{
		{"https://example.com", "https://EXAMPLE.com:443/frame", true},
		{"http://example.com", "http://example.com:80/frame", true},
		{"https://example.com", "http://example.com/frame", false},
		{"http://example.com", "https://example.com/frame", false},
		{"https://example.com", "https://example.com:8443/frame", false},
		{"https://example.com", "https://fixture@example.com/frame", false},
		{"https://blog.naver.com", "https://m.blog.naver.com/frame", true},
		{"https://blog.naver.com", "https://naver.com.example.com/frame", false},
		{"https://blog.naver.com", "http://m.blog.naver.com/frame", false},
	} {
		t.Run(tc.parent+"->"+tc.child, func(t *testing.T) {
			parent, _ := url.Parse(tc.parent)
			child, _ := url.Parse(tc.child)
			if got := canFetchFrame(parent, child); got != tc.allowed {
				t.Fatalf("allowed=%v, want %v", got, tc.allowed)
			}
		})
	}
}

func TestWebTopLevelRedirectStillWorks(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "EXPLICIT_TARGET_SENTINEL")
	}))
	defer target.Close()
	start := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer start.Close()
	result, err := directWebFetch(context.Background(), start.URL)
	if err != nil || !strings.Contains(result.Content, "EXPLICIT_TARGET_SENTINEL") {
		t.Fatalf("explicit target redirect failed: %v", err)
	}
}
