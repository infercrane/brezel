package connector

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/securefile"
	"github.com/infercrane/brezel/internal/store"
)

const (
	leaseVersion    = "v1"
	maxRequestBody  = 16 << 20
	maxResponseBody = 16 << 20
)

var (
	ErrUnauthorized = errors.New("connector lease unauthorized")
	ErrForbidden    = errors.New("connector request forbidden")
	ErrUnavailable  = errors.New("connector unavailable")
)

type SecretResolver interface {
	Resolve(context.Context, string) ([]byte, error)
}

type Claims struct {
	ProjectID  string   `json:"project_id"`
	SandboxID  string   `json:"sandbox_id"`
	Connectors []string `json:"connectors"`
	IssuedAt   int64    `json:"issued_at"`
	ExpiresAt  int64    `json:"expires_at"`
	Nonce      string   `json:"nonce"`
}

type ClientFactory func(domain.Connector) *http.Client

type Broker struct {
	store      store.Store
	private    ed25519.PrivateKey
	public     ed25519.PublicKey
	resolver   SecretResolver
	gatewayURL *url.URL
	leaseTTL   time.Duration
	now        func() time.Time
	clients    ClientFactory
}

type Option func(*Broker)

func WithClientFactory(factory ClientFactory) Option { return func(b *Broker) { b.clients = factory } }
func WithClock(now func() time.Time) Option          { return func(b *Broker) { b.now = now } }

func NewBroker(st store.Store, private ed25519.PrivateKey, resolver SecretResolver, gatewayURL string, leaseTTL time.Duration, options ...Option) (*Broker, error) {
	if st == nil || resolver == nil {
		return nil, errors.New("store and secret resolver are required")
	}
	if len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid connector signing key")
	}
	u, err := url.Parse(gatewayURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname()))) {
		return nil, errors.New("connector gateway URL must use HTTPS unless it is loopback")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("connector gateway URL cannot contain credentials, query, or fragment")
	}
	if leaseTTL < 30*time.Second || leaseTTL > 15*time.Minute {
		return nil, errors.New("connector lease TTL must be between 30 seconds and 15 minutes")
	}
	b := &Broker{store: st, private: private, public: private.Public().(ed25519.PublicKey), resolver: resolver, gatewayURL: u, leaseTTL: leaseTTL, now: func() time.Time { return time.Now().UTC() }}
	b.clients = func(connector domain.Connector) *http.Client { return safeHTTPClient(connector.AllowPrivateNetwork) }
	for _, option := range options {
		option(b)
	}
	return b, nil
}

func (b *Broker) GatewayURL() string  { return strings.TrimSuffix(b.gatewayURL.String(), "/") }
func (b *Broker) GatewayHost() string { return b.gatewayURL.Hostname() }

func (b *Broker) Issue(projectID, sandboxID string, connectors []string) (string, error) {
	if err := domain.ValidateProjectID(projectID); err != nil {
		return "", err
	}
	if sandboxID == "" || len(connectors) == 0 {
		return "", errors.New("sandbox and connector identities are required")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	now := b.now()
	claims := Claims{ProjectID: projectID, SandboxID: sandboxID, Connectors: append([]string(nil), connectors...), IssuedAt: now.Unix(), ExpiresAt: now.Add(b.leaseTTL).Unix(), Nonce: hex.EncodeToString(nonce)}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := ed25519.Sign(b.private, leaseMessage(encoded))
	return leaseVersion + "." + encoded + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (b *Broker) Verify(token, connectorRevision string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != leaseVersion {
		return Claims{}, ErrUnauthorized
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(b.public, leaseMessage(parts[1]), signature) {
		return Claims{}, ErrUnauthorized
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrUnauthorized
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, ErrUnauthorized
	}
	now := b.now().Unix()
	if claims.ExpiresAt < now || claims.IssuedAt > now+30 || claims.ExpiresAt-claims.IssuedAt > int64((15*time.Minute).Seconds()) {
		return Claims{}, ErrUnauthorized
	}
	if connectorRevision != "" && !slices.Contains(claims.Connectors, connectorRevision) {
		return Claims{}, ErrForbidden
	}
	if err := b.authorizeCurrentState(claims, connectorRevision); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func (b *Broker) Renew(token string) (string, error) {
	claims, err := b.Verify(token, "")
	if err != nil {
		return "", err
	}
	return b.Issue(claims.ProjectID, claims.SandboxID, claims.Connectors)
}

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/connector/v1/leases/renew" {
		b.renew(w, r)
		return
	}
	const prefix = "/connector/v1/proxy/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		brokerError(w, http.StatusNotFound, "not_found")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	revision, requestPath, ok := strings.Cut(rest, "/")
	if !ok || revision == "" {
		brokerError(w, http.StatusNotFound, "not_found")
		return
	}
	b.proxy(w, r, revision, "/"+requestPath)
}

func (b *Broker) renew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		brokerError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	token, ok := bearerToken(r)
	if !ok {
		brokerError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	renewed, err := b.Renew(token)
	if err != nil {
		brokerServiceError(w, err)
		return
	}
	writeBrokerJSON(w, http.StatusOK, map[string]any{"lease_token": renewed, "expires_in_seconds": int64(b.leaseTTL.Seconds())})
}

func (b *Broker) proxy(w http.ResponseWriter, r *http.Request, revision, requestPath string) {
	if path.Clean(requestPath) != requestPath || strings.Contains(r.URL.RawPath, "%2f") || strings.Contains(r.URL.RawPath, "%2F") {
		brokerError(w, http.StatusBadRequest, "invalid_path")
		return
	}
	token, ok := bearerToken(r)
	if !ok {
		brokerError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	claims, err := b.Verify(token, revision)
	if err != nil {
		brokerServiceError(w, err)
		return
	}
	connector, err := b.connector(claims.ProjectID, revision)
	if err != nil {
		brokerServiceError(w, err)
		return
	}
	if !methodAllowed(connector.AllowedMethods, r.Method) || !pathAllowed(connector.AllowedPaths, requestPath) {
		brokerError(w, http.StatusForbidden, "route_not_allowed")
		return
	}
	secret, err := b.resolver.Resolve(r.Context(), connector.CredentialRef)
	if err != nil || len(secret) == 0 {
		brokerError(w, http.StatusServiceUnavailable, "credential_unavailable")
		return
	}
	defer zero(secret)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil || len(body) > maxRequestBody {
		brokerError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	targetBase, _ := url.Parse(connector.Destination)
	target := *targetBase
	target.Path = strings.TrimSuffix(targetBase.Path, "/") + requestPath
	target.RawQuery = r.URL.RawQuery
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		brokerError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	copySafeRequestHeaders(upstream.Header, r.Header)
	upstream.Header.Set("Authorization", "Bearer "+string(secret))
	response, err := b.clients(connector).Do(upstream)
	if err != nil {
		brokerError(w, http.StatusBadGateway, "upstream_unavailable")
		return
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
	if err != nil || len(responseBody) > maxResponseBody {
		brokerError(w, http.StatusBadGateway, "upstream_response_too_large")
		return
	}
	responseBody = bytes.ReplaceAll(responseBody, secret, []byte("[REDACTED]"))
	copySafeResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(responseBody)
}

func (b *Broker) authorizeCurrentState(claims Claims, connectorRevision string) error {
	return b.store.View(func(state store.State) error {
		sandbox, ok := state.Sandboxes[store.ScopedKey(claims.ProjectID, claims.SandboxID)]
		if !ok || sandbox.State != domain.SandboxRunning {
			return ErrUnauthorized
		}
		if connectorRevision != "" {
			if !slices.Contains(sandbox.ConnectorRevisions, connectorRevision) {
				return ErrForbidden
			}
			if _, ok := state.Connectors[store.ScopedKey(claims.ProjectID, connectorRevision)]; !ok {
				return ErrForbidden
			}
		}
		return nil
	})
}

func (b *Broker) connector(projectID, revision string) (domain.Connector, error) {
	var connector domain.Connector
	err := b.store.View(func(state store.State) error {
		var ok bool
		connector, ok = state.Connectors[store.ScopedKey(projectID, revision)]
		if !ok {
			return ErrForbidden
		}
		return nil
	})
	return connector, err
}

func leaseMessage(encodedPayload string) []byte {
	return []byte("brezel-connector-lease-v1\x00" + encodedPayload)
}

func bearerToken(r *http.Request) (string, bool) {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(value, "Bearer ")
	return token, token != ""
}

func methodAllowed(allowed []string, method string) bool {
	for _, candidate := range allowed {
		if strings.EqualFold(candidate, method) {
			return true
		}
	}
	return false
}
func pathAllowed(allowed []string, path string) bool {
	for _, candidate := range allowed {
		if candidate == path {
			return true
		}
		if strings.HasSuffix(candidate, "/*") && strings.HasPrefix(path, strings.TrimSuffix(candidate, "*")) {
			return true
		}
	}
	return false
}

func copySafeRequestHeaders(dst, src http.Header) {
	for _, name := range []string{"Accept", "Content-Type", "User-Agent"} {
		if value := src.Values(name); len(value) > 0 {
			dst[name] = append([]string(nil), value...)
		}
	}
}
func copySafeResponseHeaders(dst, src http.Header) {
	for _, name := range []string{"Content-Type", "ETag", "Retry-After", "X-Request-ID"} {
		if value := src.Values(name); len(value) > 0 {
			dst[name] = append([]string(nil), value...)
		}
	}
}

func brokerServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnauthorized):
		brokerError(w, http.StatusUnauthorized, "unauthorized")
	case errors.Is(err, ErrForbidden):
		brokerError(w, http.StatusForbidden, "forbidden")
	default:
		brokerError(w, http.StatusServiceUnavailable, "connector_unavailable")
	}
}
func brokerError(w http.ResponseWriter, status int, code string) {
	writeBrokerJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}
func writeBrokerJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

type FileResolver struct{ root string }

func NewFileResolver(root string) (*FileResolver, error) {
	absolute, err := filepath.Abs(root)
	if err != nil || root == "" {
		return nil, errors.New("secret file directory is required")
	}
	return &FileResolver{root: absolute}, nil
}

func (r *FileResolver) Resolve(_ context.Context, handle string) ([]byte, error) {
	const prefix = "secret://file/"
	if !strings.HasPrefix(handle, prefix) {
		return nil, ErrUnavailable
	}
	name := strings.TrimPrefix(handle, prefix)
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, "\\/\x00") {
		return nil, ErrUnavailable
	}
	path := filepath.Join(r.root, name)
	data, err := securefile.Read(path, 64<<10)
	if err != nil {
		zero(data)
		return nil, ErrUnavailable
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, ErrUnavailable
	}
	return data, nil
}

func safeHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, ErrForbidden
			}
			addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(addresses) == 0 {
				return nil, ErrUnavailable
			}
			for _, candidate := range addresses {
				if ipForbidden(candidate.IP, allowPrivate) {
					return nil, ErrForbidden
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
		},
	}
	return &http.Client{Transport: transport, Timeout: 45 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func ipForbidden(ip net.IP, allowPrivate bool) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if ip.Equal(net.ParseIP("169.254.169.254")) || ip.Equal(net.ParseIP("100.100.100.200")) || ip.Equal(net.ParseIP("fd00:ec2::254")) {
		return true
	}
	return ip.IsPrivate() && !allowPrivate
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}
