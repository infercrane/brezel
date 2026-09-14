package node

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/nodeledger"
)

// BenchmarkRelayCommandAuthorization measures the in-process relay hot path:
// capability issuance, canonical request parsing, signature verification,
// replay consumption, generation fencing, and NDJSON dispatch. It excludes
// network and guest execution and therefore is not a sandbox startup claim.
func BenchmarkRelayCommandAuthorization(b *testing.B) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	signer, err := NewCapabilitySigner("brezel-api", "key-a", private)
	if err != nil {
		b.Fatal(err)
	}
	verifier, err := NewCapabilityVerifier("brezel-api", map[string]ed25519.PublicKey{"key-a": public})
	if err != nil {
		b.Fatal(err)
	}
	replay, err := NewReplayCache(b.N + 1)
	if err != nil {
		b.Fatal(err)
	}
	directory := filepath.Join(b.TempDir(), "ledger")
	if err := os.Mkdir(directory, 0o700); err != nil {
		b.Fatal(err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		b.Fatal(err)
	}
	ledger, err := nodeledger.Open(filepath.Join(directory, "routes.json"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ledger.Close() })
	now := time.Now().UTC()
	route, err := ledger.Bind("route-a", "project-a", "sandbox-a", "engine-a", now)
	if err != nil {
		b.Fatal(err)
	}
	route, err = ledger.Transition("route-a", route.Public().Generation, nodeledger.StateAttaching, nodeledger.StateReady)
	if err != nil {
		b.Fatal(err)
	}
	engine := &relayServerEngine{}
	server, err := NewRelayServer(RelayServerConfig{
		NodeID: "node-a", Audience: "brezel-node",
		Ledger: ledger, Verifier: verifier, Replay: replay, Engine: engine,
	})
	if err != nil {
		b.Fatal(err)
	}
	wire := relayCommandRequest{Argv: []string{"true"}}
	canonical, err := canonicalRelayJSON(wire)
	if err != nil {
		b.Fatal(err)
	}
	digest, err := RequestDigest(CapabilityRunCommand, canonical)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		token, _, err := signer.Issue(CapabilityIssue{
			Audience: "brezel-node", NodeID: "node-a", BootEpoch: server.bootEpoch,
			RouteID: "route-a", Generation: route.Public().Generation,
			ProjectID: "project-a", SandboxID: "sandbox-a",
			Operation: CapabilityRunCommand, RequestDigest: digest,
			Bounds: CapabilityBounds{MaxDurationMillis: 1_000, MaxResponseBytes: 1 << 20}, TTL: 10 * time.Second,
		})
		if err != nil {
			b.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(RelayRouteHeader, "route-a")
		request.Header.Set("Authorization", RelayCapabilityScheme+" "+token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			b.Fatalf("relay status=%d body=%q", response.Code, response.Body.String())
		}
	}
}
