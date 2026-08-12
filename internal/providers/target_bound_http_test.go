package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

type staticTargetResolver struct {
	mu        sync.Mutex
	addresses []net.IPAddr
	err       error
	calls     int
	hosts     []string
}

func (r *staticTargetResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.hosts = append(r.hosts, host)
	return append([]net.IPAddr(nil), r.addresses...), r.err
}

func (r *staticTargetResolver) replace(addresses []net.IPAddr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addresses = append([]net.IPAddr(nil), addresses...)
}

func (r *staticTargetResolver) snapshot() (int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, append([]string(nil), r.hosts...)
}

type targetDialerFunc func(context.Context, string, string) (net.Conn, error)

func (fn targetDialerFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return fn(ctx, network, address)
}

func TestTargetBoundHTTPClientLoopbackAndBasePath(t *testing.T) {
	t.Run("ipv4", func(t *testing.T) {
		var seenPath, seenHost string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			seenPath = request.URL.Path
			seenHost = request.Host
			w.WriteHeader(http.StatusNoContent)
		}))
		defer upstream.Close()

		client, proof, err := NewTargetBoundHTTPClient(context.Background(), upstream.URL+"/tenant/api")
		if err != nil {
			t.Fatal(err)
		}
		defer client.CloseIdleConnections()
		response, err := client.Do(mustTargetRequest(t, upstream.URL+"/tenant/api/v1/messages"))
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if seenPath != "/tenant/api/v1/messages" {
			t.Fatalf("path = %q", seenPath)
		}
		parsed, _ := url.Parse(upstream.URL)
		if seenHost != parsed.Host {
			t.Fatalf("Host = %q, want %q", seenHost, parsed.Host)
		}
		if proof.Scope != TargetBindingScope || proof.AddressCount != 1 {
			t.Fatalf("proof = %+v", proof)
		}
	})

	t.Run("ipv6", func(t *testing.T) {
		listener, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			t.Skipf("IPv6 loopback unavailable: %v", err)
		}
		upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/v1/provider" {
				t.Errorf("path = %q", request.URL.Path)
			}
			if !strings.HasPrefix(request.Host, "[::1]:") {
				t.Errorf("Host = %q", request.Host)
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		upstream.Listener = listener
		upstream.Start()
		defer upstream.Close()

		client, proof, err := NewTargetBoundHTTPClient(context.Background(), upstream.URL+"/v1")
		if err != nil {
			t.Fatal(err)
		}
		defer client.CloseIdleConnections()
		response, err := client.Do(mustTargetRequest(t, upstream.URL+"/v1/provider"))
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if proof.AddressCount != 1 {
			t.Fatalf("address count = %d", proof.AddressCount)
		}
	})
}

func TestTargetBoundHTTPClientPinsDNSBypassesProxyAndPreservesAuthority(t *testing.T) {
	var seenHost, seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seenHost = request.Host
		seenPath = request.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)

	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		http.Error(w, "proxy must not be used", http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	resolver := &staticTargetResolver{addresses: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}
	targetBase := "http://Provider-Target.Test.:" + upstreamURL.Port() + "/custom/base/"
	client, _, err := newTargetBoundHTTPClient(context.Background(), targetBase, resolver, &net.Dialer{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()

	// A later DNS answer cannot rebind this run; the transport never resolves
	// the hostname a second time.
	resolver.replace([]net.IPAddr{{IP: net.ParseIP("127.0.0.2")}})
	requestURL := "http://provider-target.test:" + upstreamURL.Port() + "/custom/base/v1/chat"
	response, err := client.Do(mustTargetRequest(t, requestURL))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()

	if proxyCalls.Load() != 0 {
		t.Fatalf("proxy calls = %d", proxyCalls.Load())
	}
	if seenHost != "provider-target.test:"+upstreamURL.Port() {
		t.Fatalf("Host = %q", seenHost)
	}
	if seenPath != "/custom/base/v1/chat" {
		t.Fatalf("path = %q", seenPath)
	}
	if calls, hosts := resolver.snapshot(); calls != 1 || len(hosts) != 1 || hosts[0] != "provider-target.test" {
		t.Fatalf("resolver calls=%d hosts=%v", calls, hosts)
	}
	transport, ok := client.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("target transport unexpectedly has a proxy function")
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.ServerName != "provider-target.test" {
		t.Fatalf("TLS ServerName = %q", transport.TLSClientConfig.ServerName)
	}
}

func TestTargetBindingCanonicalizesEquivalentSchemeHostPortAndPath(t *testing.T) {
	resolverA := &staticTargetResolver{addresses: []net.IPAddr{{IP: net.ParseIP("192.0.2.20")}}}
	resolverB := &staticTargetResolver{addresses: []net.IPAddr{{IP: net.ParseIP("192.0.2.20")}}}
	dialer := targetDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("not used")
	})
	first, firstProof, err := newTargetBoundHTTPClient(context.Background(), "HTTP://Provider-Target.Test.:80/tenant/api/", resolverA, dialer)
	if err != nil {
		t.Fatal(err)
	}
	second, secondProof, err := newTargetBoundHTTPClient(context.Background(), "http://provider-target.test/tenant/api", resolverB, dialer)
	if err != nil {
		t.Fatal(err)
	}
	if firstProof.EndpointDigest == secondProof.EndpointDigest {
		t.Fatal("exact configured endpoint digests unexpectedly match")
	}
	firstProof.EndpointDigest = ""
	secondProof.EndpointDigest = ""
	if firstProof != secondProof {
		t.Fatalf("canonical transport proofs differ: first=%+v second=%+v", firstProof, secondProof)
	}
	if first.binding.scheme != "http" || first.binding.hostname != "provider-target.test" || first.binding.port != "80" || first.binding.basePath != "/tenant/api" {
		t.Fatalf("canonical first binding = %+v", first.binding)
	}
	if second.binding.urlAuthority != "provider-target.test:80" || second.binding.hostHeader != "provider-target.test" {
		t.Fatalf("canonical second authority = %+v", second.binding)
	}
}

func TestTargetBoundHTTPClientRejectsAlternateRequestTargets(t *testing.T) {
	resolver := &staticTargetResolver{addresses: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}
	dials := 0
	dialer := targetDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("unexpected dial")
	})
	client, _, err := newTargetBoundHTTPClient(context.Background(), "http://provider-target.test:8080/v1", resolver, dialer)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		url    string
		mutate func(*http.Request)
	}{
		{name: "alternate scheme", url: "https://provider-target.test:8080/v1/ok"},
		{name: "alternate host", url: "http://other-target.test:8080/v1/ok"},
		{name: "alternate port", url: "http://provider-target.test:8081/v1/ok"},
		{name: "outside base path", url: "http://provider-target.test:8080/other"},
		{name: "segment boundary", url: "http://provider-target.test:8080/v10/escape"},
		{name: "userinfo", url: "http://user:secret@provider-target.test:8080/v1/ok"},
		{name: "fragment", url: "http://provider-target.test:8080/v1/ok#hidden"},
		{
			name: "escaped slash",
			url:  "http://provider-target.test:8080/v1/ok",
			mutate: func(request *http.Request) {
				request.URL.RawPath = "/v1%2Ftenant/ok"
			},
		},
		{
			name: "backslash",
			url:  "http://provider-target.test:8080/v1/ok",
			mutate: func(request *http.Request) {
				request.URL.Path = `/v1\escape`
			},
		},
		{
			name: "dot segment",
			url:  "http://provider-target.test:8080/v1/ok",
			mutate: func(request *http.Request) {
				request.URL.Path = "/v1/../escape"
			},
		},
		{
			name: "Host override",
			url:  "http://provider-target.test:8080/v1/ok",
			mutate: func(request *http.Request) {
				request.Host = "other-target.test:8080"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := mustTargetRequest(t, test.url)
			if test.mutate != nil {
				test.mutate(request)
			}
			_, err := client.Do(request)
			if !errors.Is(err, ErrProviderTargetViolation) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if dials != 0 {
		t.Fatalf("invalid requests reached dialer %d times", dials)
	}
	if calls, _ := resolver.snapshot(); calls != 1 {
		t.Fatalf("resolver calls = %d, want construction-only lookup", calls)
	}
}

func TestTargetBoundHTTPClientRejectsAmbiguousBaseURLsWithoutDisclosure(t *testing.T) {
	resolver := &staticTargetResolver{addresses: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}
	dialer := targetDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("must not dial")
	})
	tests := []string{
		"relative/path",
		"ftp://private-target.invalid/v1",
		"https://user:private-password@private-target.invalid/v1",
		"https://private-target.invalid/v1?token=private-token",
		"https://private-target.invalid/v1#private-fragment",
		"https://private-target.invalid/v1%2Ftenant",
		`https://private-target.invalid/v1\tenant`,
		"https://private-target.invalid/v1/../tenant",
		"http://[fe80::1%25private-zone]:8080/v1",
		"http://private-target.invalid:080/v1",
		"https:///missing-host",
	}
	for _, raw := range tests {
		t.Run(digestFields(raw)[:12], func(t *testing.T) {
			_, _, err := newTargetBoundHTTPClient(context.Background(), raw, resolver, dialer)
			if !errors.Is(err, ErrInvalidProviderTarget) {
				t.Fatalf("error = %v", err)
			}
			for _, secret := range []string{"private-target.invalid", "private-password", "private-token", "private-fragment", "private-zone", "/v1"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error disclosed %q: %v", secret, err)
				}
			}
		})
	}
	if calls, _ := resolver.snapshot(); calls != 0 {
		t.Fatalf("invalid targets caused %d DNS lookups", calls)
	}
}

func TestTargetBindingProofContainsOnlyDigestsCountAndScope(t *testing.T) {
	const rawEndpoint = "https://proof-private.invalid:8443/tenant-secret"
	const rawIP = "192.0.2.44"
	resolver := &staticTargetResolver{addresses: []net.IPAddr{{IP: net.ParseIP(rawIP)}}}
	dialer := targetDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("not used")
	})
	client, proof, err := newTargetBoundHTTPClient(context.Background(), rawEndpoint, resolver, dialer)
	if err != nil {
		t.Fatal(err)
	}
	if client.Proof() != proof {
		t.Fatal("Proof did not return the construction proof")
	}
	if proof.Version != TargetBindingProofVersion || proof.Scope != TargetBindingScope || proof.AddressCount != 1 {
		t.Fatalf("proof metadata = %+v", proof)
	}
	for name, digest := range map[string]string{
		"endpoint": proof.EndpointDigest,
		"target":   proof.TargetDigest,
		"host":     proof.HostHash,
		"ip set":   proof.IPSetHash,
		"path":     proof.PathHash,
	} {
		if len(digest) != 64 {
			t.Fatalf("%s digest length = %d", name, len(digest))
		}
	}
	if proof.EndpointDigest != digestEndpoint(rawEndpoint) {
		t.Fatalf("endpoint digest = %q", proof.EndpointDigest)
	}
	encoded, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{rawEndpoint, "proof-private.invalid", rawIP, "tenant-secret", "8443"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("proof disclosed %q: %s", secret, encoded)
		}
	}
}

func TestTargetBindingProofDistinguishesExactEndpointTextFromCanonicalTransport(t *testing.T) {
	resolver := &staticTargetResolver{addresses: []net.IPAddr{{IP: net.ParseIP("192.0.2.45")}}}
	dialer := targetDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("not used")
	})
	_, first, err := newTargetBoundHTTPClient(context.Background(), "https://proof-private.invalid/tenant", resolver, dialer)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := newTargetBoundHTTPClient(context.Background(), "HTTPS://PROOF-PRIVATE.INVALID:443/tenant/", resolver, dialer)
	if err != nil {
		t.Fatal(err)
	}
	if first.TargetDigest != second.TargetDigest {
		t.Fatalf("canonical transport digests differ: %q != %q", first.TargetDigest, second.TargetDigest)
	}
	if first.EndpointDigest == second.EndpointDigest {
		t.Fatal("exact configured endpoint digests unexpectedly match")
	}
}

func TestTargetBoundHTTPClientBlocksRedirectWithoutLocationDisclosure(t *testing.T) {
	const redirectLocation = "http://redirect-private.invalid/tenant-secret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, redirectLocation, http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	client, _, err := NewTargetBoundHTTPClient(context.Background(), upstream.URL+"/base")
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()

	_, err = client.Do(mustTargetRequest(t, upstream.URL+"/base/start"))
	if !errors.Is(err, ErrProviderRedirectBlocked) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), redirectLocation) || strings.Contains(err.Error(), "redirect-private.invalid") {
		t.Fatalf("redirect error disclosed Location: %v", err)
	}
}

func TestTargetBoundProviderErrorDoesNotDiscloseEndpointCredentialOrDialError(t *testing.T) {
	const (
		rawEndpoint = "http://private-provider.invalid:9444/tenant-secret"
		apiKey      = "private-provider-api-key"
	)
	resolver := &staticTargetResolver{addresses: []net.IPAddr{{IP: net.ParseIP("192.0.2.91")}}}
	dialer := targetDialerFunc(func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("dial leaked %s with %s", rawEndpoint, apiKey)
	})
	client, _, err := newTargetBoundHTTPClient(context.Background(), rawEndpoint, resolver, dialer)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := CreateWithOptions("openai", &types.ProviderConfig{APIKey: apiKey, BaseURL: rawEndpoint}, CreateOptions{HTTPDoer: client})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.StreamMessage(context.Background(), &types.MessagesRequest{
		Model:     "test-model",
		MaxTokens: 1,
		Messages:  []types.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
	}, &types.StreamOptions{})
	if err == nil {
		t.Fatal("expected connection failure")
	}
	for _, secret := range []string{rawEndpoint, "private-provider.invalid", "tenant-secret", apiKey, "192.0.2.91"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("provider error disclosed %q: %v", secret, err)
		}
	}
}

func TestProviderRequestConstructionErrorsDoNotDiscloseRawEndpoint(t *testing.T) {
	const rawEndpoint = "http://request-private.invalid/%zz-tenant-secret"
	doerCalls := 0
	doer := httpDoerFunc(func(*http.Request) (*http.Response, error) {
		doerCalls++
		return nil, errors.New("must not be called")
	})
	for _, providerName := range []string{"anthropic", "openai", "gemini"} {
		t.Run(providerName, func(t *testing.T) {
			provider, err := CreateWithOptions(providerName, &types.ProviderConfig{
				APIKey:  "request-private-api-key",
				BaseURL: rawEndpoint,
			}, CreateOptions{HTTPDoer: doer})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.StreamMessage(context.Background(), &types.MessagesRequest{
				Model:     "test-model",
				MaxTokens: 1,
				Messages:  []types.Message{{Role: "user", Content: json.RawMessage(`"hello"`)}},
			}, &types.StreamOptions{})
			if err == nil {
				t.Fatal("expected request construction failure")
			}
			for _, secret := range []string{rawEndpoint, "request-private.invalid", "tenant-secret", "request-private-api-key"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("request error disclosed %q: %v", secret, err)
				}
			}
		})
	}
	if doerCalls != 0 {
		t.Fatalf("invalid requests reached injected doer %d times", doerCalls)
	}
}

func TestTargetBoundHTTPClientSanitizesDNSFailure(t *testing.T) {
	const rawEndpoint = "https://dns-private.invalid/tenant-secret"
	resolver := &staticTargetResolver{err: errors.New("lookup dns-private.invalid using private resolver failed")}
	client, proof, err := newTargetBoundHTTPClient(context.Background(), rawEndpoint, resolver, &net.Dialer{})
	if client != nil || proof != (TargetBindingProof{}) || !errors.Is(err, ErrProviderTargetResolution) {
		t.Fatalf("client=%v proof=%+v err=%v", client, proof, err)
	}
	if strings.Contains(err.Error(), "dns-private.invalid") || strings.Contains(err.Error(), "tenant-secret") {
		t.Fatalf("resolution error disclosed target: %v", err)
	}
}

func mustTargetRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	return request
}
