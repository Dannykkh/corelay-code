package providers

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// TargetBindingProofVersion changes whenever the proof inputs or binding
	// semantics change.
	TargetBindingProofVersion = "provider-target-binding/v2"

	// TargetBindingScope is intentionally narrow. The binding covers requests
	// issued through this provider HTTP client only. It says nothing about tool,
	// plugin, MCP, hook, or other process egress.
	TargetBindingScope = "provider-http-egress-only"
)

var (
	ErrInvalidProviderTarget    = errors.New("provider target configuration is invalid")
	ErrProviderTargetResolution = errors.New("provider target resolution failed")
	ErrProviderTargetViolation  = errors.New("provider target binding rejected the request")
	ErrProviderRedirectBlocked  = errors.New("provider target redirect is blocked")
	ErrProviderTargetConnection = errors.New("provider target connection failed")
	ErrProviderTargetRequest    = errors.New("provider target request failed")
)

// TargetBindingProof is content-free evidence that a provider HTTP client was
// bound to one canonical target and one DNS result set. It deliberately omits
// the raw endpoint, hostname, path, addresses, and credentials.
type TargetBindingProof struct {
	Version string `json:"version"`
	Scope   string `json:"scope"`
	// EndpointDigest binds this proof to the exact configured BaseURL text
	// without retaining the endpoint itself. It intentionally matches the
	// endpoint digest used by capabilityprofile.TargetIdentity.
	EndpointDigest string `json:"endpointDigest"`
	TargetDigest   string `json:"targetDigest"`
	HostHash       string `json:"hostHash"`
	IPSetHash      string `json:"ipSetHash"`
	PathHash       string `json:"pathHash"`
	AddressCount   int    `json:"addressCount"`
}

// TargetBoundHTTPClient is an instance-scoped HTTPDoer. DNS is resolved once
// during construction, redirects and proxies are disabled, and every dial is
// limited to the pinned address set and configured port.
type TargetBoundHTTPClient struct {
	client  *http.Client
	binding *targetBinding
	proof   TargetBindingProof
}

// NewTargetBoundHTTPClient binds provider HTTP egress to rawBaseURL. The
// caller should construct it at the start of the run whose target it proves.
func NewTargetBoundHTTPClient(ctx context.Context, rawBaseURL string) (*TargetBoundHTTPClient, TargetBindingProof, error) {
	return newTargetBoundHTTPClient(ctx, rawBaseURL, net.DefaultResolver, &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	})
}

// Proof returns the immutable, content-free proof created with the client.
func (c *TargetBoundHTTPClient) Proof() TargetBindingProof {
	if c == nil {
		return TargetBindingProof{}
	}
	return c.proof
}

// CloseIdleConnections releases pooled connections owned by this client.
func (c *TargetBoundHTTPClient) CloseIdleConnections() {
	if c == nil || c.client == nil {
		return
	}
	c.client.CloseIdleConnections()
}

// Do implements HTTPDoer while preventing the standard library's URL-bearing
// transport errors from escaping this security boundary.
func (c *TargetBoundHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if c == nil || c.client == nil || c.binding == nil || req == nil || req.URL == nil {
		return nil, ErrProviderTargetViolation
	}
	if err := c.binding.validateRequest(req); err != nil {
		return nil, ErrProviderTargetViolation
	}

	clone := req.Clone(req.Context())
	cloneURL := *req.URL
	clone.URL = &cloneURL
	clone.URL.Scheme = c.binding.scheme
	clone.URL.Host = c.binding.urlAuthority
	clone.URL.Path = canonicalPath(clone.URL.Path)
	clone.URL.RawPath = ""
	clone.Host = c.binding.hostHeader
	clone.Header.Del("Host")

	resp, err := c.client.Do(clone)
	if err == nil {
		return resp, nil
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if req.Context() != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	switch {
	case errors.Is(err, ErrProviderRedirectBlocked):
		return nil, ErrProviderRedirectBlocked
	case errors.Is(err, ErrProviderTargetViolation):
		return nil, ErrProviderTargetViolation
	case errors.Is(err, ErrProviderTargetConnection):
		return nil, ErrProviderTargetConnection
	default:
		return nil, ErrProviderTargetRequest
	}
}

type targetResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type targetDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type targetBinding struct {
	scheme       string
	hostname     string
	port         string
	basePath     string
	urlAuthority string
	hostHeader   string
	addresses    []netip.Addr
	dialer       targetDialer
}

func newTargetBoundHTTPClient(ctx context.Context, rawBaseURL string, resolver targetResolver, dialer targetDialer) (*TargetBoundHTTPClient, TargetBindingProof, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if resolver == nil || dialer == nil {
		return nil, TargetBindingProof{}, ErrInvalidProviderTarget
	}
	target, err := parseProviderTarget(rawBaseURL)
	if err != nil {
		return nil, TargetBindingProof{}, ErrInvalidProviderTarget
	}
	addresses, err := resolvePinnedAddresses(ctx, resolver, target.hostname)
	if err != nil {
		return nil, TargetBindingProof{}, err
	}
	target.addresses = addresses
	target.dialer = dialer

	ipParts := make([]string, 0, len(addresses))
	for _, address := range addresses {
		ipParts = append(ipParts, address.String())
	}
	ipSetHash := digestFields(ipParts...)
	proof := TargetBindingProof{
		Version:        TargetBindingProofVersion,
		Scope:          TargetBindingScope,
		EndpointDigest: digestEndpoint(rawBaseURL),
		HostHash:       digestFields(target.hostname),
		IPSetHash:      ipSetHash,
		PathHash:       digestFields(target.basePath),
		AddressCount:   len(addresses),
	}
	proof.TargetDigest = digestFields(
		proof.Version,
		proof.Scope,
		target.scheme,
		target.hostname,
		target.port,
		target.basePath,
		ipSetHash,
	)

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: target.hostname,
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           target.dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       tlsConfig,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return ErrProviderRedirectBlocked
		},
	}
	return &TargetBoundHTTPClient{client: client, binding: target, proof: proof}, proof, nil
}

func digestEndpoint(rawBaseURL string) string {
	sum := sha256.Sum256([]byte(rawBaseURL))
	return hex.EncodeToString(sum[:])
}

func parseProviderTarget(rawBaseURL string) (*targetBinding, error) {
	if rawBaseURL == "" || strings.TrimSpace(rawBaseURL) != rawBaseURL || containsControl(rawBaseURL) {
		return nil, ErrInvalidProviderTarget
	}
	parsed, err := url.Parse(rawBaseURL)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" {
		return nil, ErrInvalidProviderTarget
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, ErrInvalidProviderTarget
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return nil, ErrInvalidProviderTarget
	}
	if parsed.RawPath != "" || strings.Contains(parsed.Path, "\\") || hasDotPathSegment(parsed.Path) {
		return nil, ErrInvalidProviderTarget
	}

	hostname, err := canonicalHostname(parsed.Hostname())
	if err != nil {
		return nil, ErrInvalidProviderTarget
	}
	port, err := canonicalPort(scheme, parsed.Port())
	if err != nil {
		return nil, ErrInvalidProviderTarget
	}
	basePath := canonicalPath(parsed.Path)
	urlAuthority := net.JoinHostPort(hostname, port)
	hostHeader := urlAuthority
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		if strings.Contains(hostname, ":") {
			hostHeader = "[" + hostname + "]"
		} else {
			hostHeader = hostname
		}
	}
	return &targetBinding{
		scheme:       scheme,
		hostname:     hostname,
		port:         port,
		basePath:     basePath,
		urlAuthority: urlAuthority,
		hostHeader:   hostHeader,
	}, nil
}

func canonicalHostname(raw string) (string, error) {
	if raw == "" || containsControl(raw) || strings.Contains(raw, "%") {
		return "", ErrInvalidProviderTarget
	}
	if address, err := netip.ParseAddr(raw); err == nil {
		return address.Unmap().String(), nil
	}
	hostname := strings.TrimSuffix(strings.ToLower(raw), ".")
	if hostname == "" || len(hostname) > 253 {
		return "", ErrInvalidProviderTarget
	}
	for _, label := range strings.Split(hostname, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrInvalidProviderTarget
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", ErrInvalidProviderTarget
			}
		}
	}
	return hostname, nil
}

func canonicalPort(scheme, raw string) (string, error) {
	if raw == "" {
		if scheme == "https" {
			return "443", nil
		}
		return "80", nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != raw {
		return "", ErrInvalidProviderTarget
	}
	return raw, nil
}

func canonicalPath(raw string) string {
	if raw == "" || raw == "/" {
		return "/"
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(raw, "/"))
	return strings.TrimSuffix(cleaned, "/")
}

func hasDotPathSegment(raw string) bool {
	for _, segment := range strings.Split(raw, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func containsControl(value string) bool {
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return true
		}
	}
	return false
}

func resolvePinnedAddresses(ctx context.Context, resolver targetResolver, hostname string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(hostname); err == nil {
		return []netip.Addr{address.Unmap()}, nil
	}
	resolved, err := resolver.LookupIPAddr(ctx, hostname)
	if err != nil {
		return nil, ErrProviderTargetResolution
	}
	unique := make(map[string]netip.Addr, len(resolved))
	for _, item := range resolved {
		if item.Zone != "" {
			return nil, ErrProviderTargetResolution
		}
		address, ok := netip.AddrFromSlice(item.IP)
		if !ok || !address.IsValid() {
			continue
		}
		address = address.Unmap()
		unique[address.String()] = address
	}
	if len(unique) == 0 {
		return nil, ErrProviderTargetResolution
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	addresses := make([]netip.Addr, 0, len(keys))
	for _, key := range keys {
		addresses = append(addresses, unique[key])
	}
	return addresses, nil
}

func (b *targetBinding) validateRequest(req *http.Request) error {
	if req.RequestURI != "" || req.URL == nil || req.URL.Opaque != "" || req.URL.User != nil || req.URL.Fragment != "" || req.URL.RawFragment != "" {
		return ErrProviderTargetViolation
	}
	if strings.ToLower(req.URL.Scheme) != b.scheme {
		return ErrProviderTargetViolation
	}
	hostname, err := canonicalHostname(req.URL.Hostname())
	if err != nil || hostname != b.hostname {
		return ErrProviderTargetViolation
	}
	port, err := canonicalPort(b.scheme, req.URL.Port())
	if err != nil || port != b.port {
		return ErrProviderTargetViolation
	}
	if req.Host != "" {
		overrideHost, overridePort, err := canonicalRequestAuthority(req.Host, b.scheme)
		if err != nil || overrideHost != b.hostname || overridePort != b.port {
			return ErrProviderTargetViolation
		}
	}
	if req.URL.RawPath != "" || strings.Contains(req.URL.Path, "\\") || hasDotPathSegment(req.URL.Path) {
		return ErrProviderTargetViolation
	}
	requestPath := canonicalPath(req.URL.Path)
	if b.basePath != "/" && requestPath != b.basePath && !strings.HasPrefix(requestPath, b.basePath+"/") {
		return ErrProviderTargetViolation
	}
	return nil
}

func canonicalRequestAuthority(raw, scheme string) (string, string, error) {
	if raw == "" || containsControl(raw) || strings.Contains(raw, "@") {
		return "", "", ErrProviderTargetViolation
	}
	parsed, err := url.Parse("//" + raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" {
		return "", "", ErrProviderTargetViolation
	}
	hostname, err := canonicalHostname(parsed.Hostname())
	if err != nil {
		return "", "", ErrProviderTargetViolation
	}
	port, err := canonicalPort(scheme, parsed.Port())
	if err != nil {
		return "", "", ErrProviderTargetViolation
	}
	return hostname, port, nil
}

func (b *targetBinding) dialContext(ctx context.Context, network, requested string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(requested)
	if err != nil {
		return nil, ErrProviderTargetViolation
	}
	hostname, err := canonicalHostname(host)
	if err != nil || hostname != b.hostname || port != b.port {
		return nil, ErrProviderTargetViolation
	}
	for _, address := range b.addresses {
		if network == "tcp4" && !address.Is4() {
			continue
		}
		if network == "tcp6" && !address.Is6() {
			continue
		}
		connection, dialErr := b.dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), b.port))
		if dialErr == nil {
			return connection, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	return nil, ErrProviderTargetConnection
}

func digestFields(fields ...string) string {
	hash := sha256.New()
	var size [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
