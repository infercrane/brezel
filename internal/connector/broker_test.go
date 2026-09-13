package connector

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
	"github.com/infercrane/sandbox-runtime-lab/internal/store"
)

func TestBrokerInjectsCredentialOutsideGuestAndScrubsResponse(t *testing.T) {
	secretDirectory := t.TempDir()
	secretPath := filepath.Join(secretDirectory, "model-api")
	if err := os.WriteFile(secretPath, []byte("upstream-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, err := NewFileResolver(secretDirectory)
	if err != nil {
		t.Fatal(err)
	}

	var receivedAuthorization string
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "must-not-cross=1")
		_, _ = w.Write([]byte(`{"result":"ok","debug":"upstream-secret"}`))
	}))
	defer upstream.Close()

	st, err := store.OpenFile(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	connectorRevision := "connr_test"
	sandboxID := "sbx_test"
	if err := st.Update(func(state *store.State) error {
		state.Connectors[store.ScopedKey("project-a", connectorRevision)] = domain.Connector{
			RevisionID: connectorRevision, ProjectID: "project-a", Name: "model", Destination: upstream.URL,
			AllowedMethods: []string{"POST"}, AllowedPaths: []string{"/chat/*"}, CredentialRef: "secret://file/model-api",
		}
		state.Sandboxes[store.ScopedKey("project-a", sandboxID)] = domain.Sandbox{ID: sandboxID, ProjectID: "project-a", State: domain.SandboxRunning, ConnectorRevisions: []string{connectorRevision}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	broker, err := NewBroker(st, private, resolver, "http://127.0.0.1/connector/v1", 5*time.Minute, WithClientFactory(func(domain.Connector) *http.Client { return upstream.Client() }))
	if err != nil {
		t.Fatal(err)
	}
	token, err := broker.Issue("project-a", sandboxID, []string{connectorRevision})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(broker)
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL+"/connector/v1/proxy/"+connectorRevision+"/chat/completions", strings.NewReader(`{"input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Cookie", "guest-cookie=1")
	req.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if receivedAuthorization != "Bearer upstream-secret" {
		t.Fatalf("authorization=%q", receivedAuthorization)
	}
	if bytes.Contains(body, []byte("upstream-secret")) || !bytes.Contains(body, []byte("[REDACTED]")) {
		t.Fatalf("response was not scrubbed: %s", body)
	}
	if response.Header.Get("Set-Cookie") != "" {
		t.Fatal("upstream cookie crossed connector boundary")
	}

	denied, _ := http.NewRequest(http.MethodPost, server.URL+"/connector/v1/proxy/"+connectorRevision+"/admin", nil)
	denied.Header.Set("Authorization", "Bearer "+token)
	deniedResponse, err := server.Client().Do(denied)
	if err != nil {
		t.Fatal(err)
	}
	deniedResponse.Body.Close()
	if deniedResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("disallowed path status=%d", deniedResponse.StatusCode)
	}
}

func TestLeaseExpiresAndCannotRenewAfterSandboxStops(t *testing.T) {
	st, _ := store.OpenFile(filepath.Join(t.TempDir(), "state.json"))
	connectorRevision, sandboxID := "connr_test", "sbx_test"
	_ = st.Update(func(state *store.State) error {
		state.Connectors[store.ScopedKey("project-a", connectorRevision)] = domain.Connector{RevisionID: connectorRevision, ProjectID: "project-a"}
		state.Sandboxes[store.ScopedKey("project-a", sandboxID)] = domain.Sandbox{ID: sandboxID, ProjectID: "project-a", State: domain.SandboxRunning, ConnectorRevisions: []string{connectorRevision}}
		return nil
	})
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1_800_000_000, 0).UTC()
	broker, err := NewBroker(st, private, staticResolver("secret"), "http://127.0.0.1/connector/v1", 30*time.Second, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	token, _ := broker.Issue("project-a", sandboxID, []string{connectorRevision})
	if _, err := broker.Verify(token, connectorRevision); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if _, err := broker.Verify(token, connectorRevision); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired token: %v", err)
	}

	now = time.Unix(1_800_000_000, 0).UTC()
	token, _ = broker.Issue("project-a", sandboxID, []string{connectorRevision})
	_ = st.Update(func(state *store.State) error {
		sandbox := state.Sandboxes[store.ScopedKey("project-a", sandboxID)]
		sandbox.State = domain.SandboxStandby
		state.Sandboxes[store.ScopedKey("project-a", sandboxID)] = sandbox
		return nil
	})
	if _, err := broker.Renew(token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("renewed stopped sandbox token: %v", err)
	}
}

func TestFileResolverRejectsLoosePermissionsAndSymlinks(t *testing.T) {
	directory := t.TempDir()
	resolver, _ := NewFileResolver(directory)
	loose := filepath.Join(directory, "loose")
	if err := os.WriteFile(loose, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), "secret://file/loose"); err == nil {
		t.Fatal("accepted loosely permissioned secret")
	}
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), "secret://file/link"); err == nil {
		t.Fatal("accepted secret symlink")
	}
}

func TestNetworkGuardBlocksMetadataAndPrivateAddressesByDefault(t *testing.T) {
	for _, value := range []string{"169.254.169.254", "100.100.100.200", "127.0.0.1", "10.0.0.1", "fd00:ec2::254"} {
		if !ipForbidden(net.ParseIP(value), false) {
			t.Fatalf("address %s was allowed", value)
		}
	}
	if ipForbidden(net.ParseIP("1.1.1.1"), false) {
		t.Fatal("public address was blocked")
	}
	if ipForbidden(net.ParseIP("10.0.0.1"), true) {
		t.Fatal("explicit private-network connector was blocked")
	}
	if !ipForbidden(net.ParseIP("169.254.169.254"), true) {
		t.Fatal("metadata address bypassed private-network opt-in")
	}
}

type staticResolver string

func (r staticResolver) Resolve(context.Context, string) ([]byte, error) { return []byte(r), nil }
