package e2b

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/infercrane/brezel/internal/backend"
)

const portCredentialTTL = 30 * time.Second

type portCredential struct {
	token     string
	domain    string
	expiresAt time.Time
}

func (c *Client) RoundTripPort(ctx context.Context, sandboxID string, port uint16, incoming *http.Request) (*http.Response, error) {
	if incoming == nil || incoming.URL == nil {
		return nil, errors.New("port request is required")
	}
	if err := c.ValidatePort(port); err != nil {
		return nil, err
	}
	connection, err := c.portConnection(ctx, sandboxID, port)
	if err != nil {
		return nil, err
	}
	outgoing := incoming.Clone(ctx)
	outgoing.RequestURI = ""
	outgoing.URL = cloneURL(incoming.URL)
	outgoing.URL.Scheme = connection.baseURL.Scheme
	outgoing.URL.Host = connection.baseURL.Host
	outgoing.URL.Path = joinURLPath(connection.baseURL.Path, incoming.URL.Path)
	outgoing.URL.RawPath = ""
	outgoing.Host = connection.baseURL.Host
	outgoing.Header = incoming.Header.Clone()
	stripHopHeaders(outgoing.Header)
	for _, header := range []string{"X-Access-Token", "E2b-Sandbox-Id", "E2b-Sandbox-Port", "E2B-Traffic-Access-Token"} {
		outgoing.Header.Del(header)
	}
	connection.setRoutingHeaders(outgoing.Header)
	response, err := c.guestHTTPClient.Do(outgoing)
	if err != nil {
		return nil, fmt.Errorf("application port request: %w", err)
	}
	return response, nil
}

// OpenPortWebSocket opens an authenticated WebSocket to one application port.
// The substrate traffic credential is injected only on this internal hop and
// is never returned to the product caller.
func (c *Client) OpenPortWebSocket(ctx context.Context, sandboxID string, port uint16, path string) (*websocket.Conn, *http.Response, error) {
	if err := c.ValidatePort(port); err != nil {
		return nil, nil, err
	}
	if !validWebSocketPath(path) {
		return nil, nil, errors.New("application WebSocket path is invalid")
	}
	connection, err := c.portConnection(ctx, sandboxID, port)
	if err != nil {
		return nil, nil, err
	}
	target := cloneURL(connection.baseURL)
	switch target.Scheme {
	case "http":
		target.Scheme = "ws"
	case "https":
		target.Scheme = "wss"
	default:
		return nil, nil, errors.New("application WebSocket endpoint has an invalid scheme")
	}
	target.Path = joinURLPath(connection.baseURL.Path, path)
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	outgoing := make(http.Header)
	connection.setRoutingHeaders(outgoing)
	conn, response, err := websocket.Dial(ctx, target.String(), &websocket.DialOptions{
		HTTPClient: c.guestHTTPClient,
		HTTPHeader: outgoing,
	})
	if err != nil {
		return nil, response, fmt.Errorf("application WebSocket: %w", err)
	}
	return conn, response, nil
}

func validWebSocketPath(value string) bool {
	return strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") && !strings.ContainsAny(value, "\x00\r\n?#")
}

func (c *Client) ValidatePort(port uint16) error {
	if port == 0 || port == envdPort {
		return errors.New("application port is invalid or reserved")
	}
	return nil
}

func (c *Client) portConnection(ctx context.Context, sandboxID string, port uint16) (guestConnection, error) {
	if !safeSandboxID.MatchString(sandboxID) {
		return guestConnection{}, errors.New("invalid backend sandbox id")
	}
	if credential, ok := c.cachedPortCredential(sandboxID); ok {
		return c.resolvePortConnection(sandboxID, port, credential)
	}
	var detail sandboxResponse
	// The engine deliberately returns the traffic credential from connect,
	// not from the read-only sandbox detail response. Connect is authenticated,
	// preserves a longer existing TTL, and also gives the proxy a supported
	// route for auto-resuming a paused sandbox.
	if err := c.do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(sandboxID)+"/connect", map[string]int{"timeout": 60}, &detail, http.StatusOK, http.StatusCreated); err != nil {
		return guestConnection{}, err
	}
	if detail.SandboxID != "" && detail.SandboxID != sandboxID {
		return guestConnection{}, errors.New("backend returned a mismatched sandbox id")
	}
	if detail.TrafficAccessToken == nil || strings.TrimSpace(*detail.TrafficAccessToken) == "" {
		return guestConnection{}, errors.New("microVM engine did not confirm authenticated port access")
	}
	c.rememberPortCredential(detail)
	credential, ok := c.cachedPortCredential(sandboxID)
	if !ok {
		return guestConnection{}, errors.New("microVM engine port credential was not retained")
	}
	return c.resolvePortConnection(sandboxID, port, credential)
}

func (c *Client) resolvePortConnection(sandboxID string, port uint16, credential portCredential) (guestConnection, error) {
	var domain *string
	if credential.domain != "" {
		domain = &credential.domain
	}
	baseURL, err := c.resolveGuestURL(sandboxID, domain, port)
	if err != nil {
		return guestConnection{}, err
	}
	return guestConnection{
		baseURL:      baseURL,
		trafficToken: credential.token,
		sandboxID:    sandboxID,
		port:         port,
	}, nil
}

func (c *Client) rememberPortCredential(detail sandboxResponse) {
	if !safeSandboxID.MatchString(detail.SandboxID) || detail.TrafficAccessToken == nil {
		return
	}
	token := strings.TrimSpace(*detail.TrafficAccessToken)
	if token == "" {
		return
	}
	domain := ""
	if detail.Domain != nil {
		domain = strings.TrimSpace(*detail.Domain)
	}
	now := time.Now()
	c.portMu.Lock()
	defer c.portMu.Unlock()
	for id, credential := range c.portCredentials {
		if !now.Before(credential.expiresAt) {
			delete(c.portCredentials, id)
		}
	}
	c.portCredentials[detail.SandboxID] = portCredential{token: token, domain: domain, expiresAt: now.Add(portCredentialTTL)}
}

func (c *Client) cachedPortCredential(sandboxID string) (portCredential, bool) {
	now := time.Now()
	c.portMu.Lock()
	defer c.portMu.Unlock()
	credential, ok := c.portCredentials[sandboxID]
	if !ok || !now.Before(credential.expiresAt) {
		delete(c.portCredentials, sandboxID)
		return portCredential{}, false
	}
	return credential, true
}

func (c *Client) forgetPortCredential(sandboxID string) {
	c.portMu.Lock()
	delete(c.portCredentials, sandboxID)
	c.portMu.Unlock()
}

func cloneURL(source *url.URL) *url.URL {
	copy := *source
	return &copy
}

func joinURLPath(prefix, suffix string) string {
	if prefix == "" || prefix == "/" {
		if strings.HasPrefix(suffix, "/") {
			return suffix
		}
		return "/" + suffix
	}
	return strings.TrimRight(prefix, "/") + "/" + strings.TrimLeft(suffix, "/")
}

func stripHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		header.Del(name)
	}
}

var _ backend.PortRuntime = (*Client)(nil)
var _ backend.WebSocketPortRuntime = (*Client)(nil)
