package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

var capabilityTestTime = time.Date(2026, 9, 14, 14, 0, 0, 0, time.UTC)

type capabilityFixture struct {
	private  ed25519.PrivateKey
	public   ed25519.PublicKey
	signer   *CapabilitySigner
	verifier *CapabilityVerifier
	issue    CapabilityIssue
	expected CapabilityExpected
}

func newCapabilityFixture(t *testing.T, operation CapabilityOperation, bounds CapabilityBounds) capabilityFixture {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewCapabilitySigner("brezel-control", "node-key-1", private)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewCapabilityVerifier("brezel-control", map[string]ed25519.PublicKey{"node-key-1": public})
	if err != nil {
		t.Fatal(err)
	}
	signer.now = func() time.Time { return capabilityTestTime }
	verifier.now = func() time.Time { return capabilityTestTime }
	digest, err := RequestDigest(operation, []byte(`{"canonical":"request"}`))
	if err != nil {
		t.Fatal(err)
	}
	issue := CapabilityIssue{
		Audience:      "brezel-node-relay",
		NodeID:        "node-par-01",
		BootEpoch:     "boot-0199d8f4",
		RouteID:       "route-01JY7QK8",
		Generation:    17,
		ProjectID:     "project-a",
		SandboxID:     "sandbox-a",
		Operation:     operation,
		RequestDigest: digest,
		Bounds:        bounds,
		TTL:           10 * time.Second,
	}
	return capabilityFixture{
		private:  private,
		public:   public,
		signer:   signer,
		verifier: verifier,
		issue:    issue,
		expected: CapabilityExpected{
			Audience:      issue.Audience,
			NodeID:        issue.NodeID,
			BootEpoch:     issue.BootEpoch,
			RouteID:       issue.RouteID,
			Generation:    issue.Generation,
			ProjectID:     issue.ProjectID,
			SandboxID:     issue.SandboxID,
			Operation:     issue.Operation,
			RequestDigest: issue.RequestDigest,
		},
	}
}

func commandBounds() CapabilityBounds {
	return CapabilityBounds{MaxDurationMillis: 30_000, MaxResponseBytes: 1 << 20}
}

func TestCapabilityRoundTripForEveryOperation(t *testing.T) {
	tests := []struct {
		name      string
		operation CapabilityOperation
		bounds    CapabilityBounds
	}{
		{"command", CapabilityRunCommand, commandBounds()},
		{"file-read", CapabilityReadFile, CapabilityBounds{MaxResponseBytes: 64 << 20}},
		{"file-write", CapabilityWriteFile, CapabilityBounds{MaxRequestBytes: 32 << 20}},
		{"port-proxy", CapabilityProxyPort, CapabilityBounds{Port: 3000, MaxDurationMillis: 30_000, MaxRequestBytes: 2 << 20, MaxResponseBytes: 8 << 20}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCapabilityFixture(t, test.operation, test.bounds)
			token, issued, err := fixture.signer.Issue(fixture.issue)
			if err != nil {
				t.Fatal(err)
			}
			if len(strings.Split(token, ".")) != 3 {
				t.Fatalf("token is not compact: %q", token)
			}
			verified, err := fixture.verifier.Verify(token, fixture.expected)
			if err != nil {
				t.Fatal(err)
			}
			if verified.JTI == "" || verified.JTI != issued.JTI || verified.KeyID != "node-key-1" || verified.Issuer != "brezel-control" {
				t.Fatalf("unexpected verified claims: %#v", verified)
			}
			if len(verified.Operations) != 1 || verified.Operations[0] != test.operation || verified.Bounds != test.bounds {
				t.Fatalf("operation or bounds changed: %#v", verified)
			}
		})
	}
}

func TestCapabilityAuthenticateAndMatchStages(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	token, issued, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	authenticated, err := fixture.verifier.Authenticate(token)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(authenticated, issued) {
		t.Fatalf("authenticated claims changed: got=%#v want=%#v", authenticated, issued)
	}
	if err := fixture.verifier.Match(authenticated, fixture.expected); err != nil {
		t.Fatalf("authenticated claims did not match their request context: %v", err)
	}
}

func TestCapabilityAuthenticateNeverReturnsUnauthenticatedClaims(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	token, claims, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")

	tamperedClaims := claims
	tamperedClaims.RouteID = "route-attacker"
	tamperedJSON, err := json.Marshal(tamperedClaims)
	if err != nil {
		t.Fatal(err)
	}
	tamperedPayload := parts[0] + "." + base64.RawURLEncoding.EncodeToString(tamperedJSON) + "." + parts[2]

	tamperedSignatureBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	tamperedSignatureBytes[0] ^= 0x80
	tamperedSignature := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(tamperedSignatureBytes)

	expiredCopy := *fixture.verifier
	expiredCopy.now = func() time.Time { return time.Unix(claims.ExpiresAt, 0) }
	expired := &expiredCopy

	tests := []struct {
		name     string
		verifier *CapabilityVerifier
		token    string
		wantErr  error
	}{
		{name: "malformed", verifier: fixture.verifier, token: "not-a-capability", wantErr: ErrMalformedCapability},
		{name: "tampered-payload", verifier: fixture.verifier, token: tamperedPayload, wantErr: ErrInvalidCapabilitySignature},
		{name: "tampered-signature", verifier: fixture.verifier, token: tamperedSignature, wantErr: ErrInvalidCapabilitySignature},
		{name: "expired", verifier: expired, token: token, wantErr: ErrCapabilityExpired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticated, err := test.verifier.Authenticate(test.token)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error=%v want=%v", err, test.wantErr)
			}
			if !reflect.DeepEqual(authenticated, CapabilityClaims{}) {
				t.Fatalf("unauthenticated claims escaped: %#v", authenticated)
			}
		})
	}
}

func TestAuthenticatedCapabilityCannotMatchDifferentRequest(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	token, _, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := fixture.verifier.Authenticate(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.verifier.Match(CapabilityClaims{}, fixture.expected); !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("unauthenticated claims mismatch error=%v", err)
	}
	otherDigest, err := RequestDigest(CapabilityRunCommand, []byte("another canonical request"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*CapabilityExpected)
	}{
		{name: "route", mutate: func(expected *CapabilityExpected) { expected.RouteID = "route-other" }},
		{name: "operation", mutate: func(expected *CapabilityExpected) { expected.Operation = CapabilityReadFile }},
		{name: "digest", mutate: func(expected *CapabilityExpected) { expected.RequestDigest = otherDigest }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expected := fixture.expected
			test.mutate(&expected)
			if err := fixture.verifier.Match(claims, expected); !errors.Is(err, ErrCapabilityMismatch) {
				t.Fatalf("mismatch error=%v", err)
			}
		})
	}
}

func TestCapabilityIssuanceUsesUniqueJTI(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	_, first, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	if first.JTI == second.JTI {
		t.Fatalf("reused capability id %q", first.JTI)
	}
}

func TestCapabilityRejectsPayloadAndSignatureTampering(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	token, claims, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	claims.SandboxID = "sandbox-attacker"
	claimsJSON, _ := json.Marshal(claims)
	tamperedPayload := parts[0] + "." + base64.RawURLEncoding.EncodeToString(claimsJSON) + "." + parts[2]
	if _, err := fixture.verifier.Verify(tamperedPayload, fixture.expected); !errors.Is(err, ErrInvalidCapabilitySignature) {
		t.Fatalf("tampered payload error=%v", err)
	}

	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	signature[0] ^= 0x80
	tamperedSignature := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(signature)
	if _, err := fixture.verifier.Verify(tamperedSignature, fixture.expected); !errors.Is(err, ErrInvalidCapabilitySignature) {
		t.Fatalf("tampered signature error=%v", err)
	}
}

func TestCapabilityRejectsExpiredAndFutureTokens(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	token, claims, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	fixture.verifier.now = func() time.Time { return time.Unix(claims.ExpiresAt, 0) }
	if _, err := fixture.verifier.Verify(token, fixture.expected); !errors.Is(err, ErrCapabilityExpired) {
		t.Fatalf("expired token error=%v", err)
	}

	fixture = newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	fixture.signer.now = func() time.Time { return capabilityTestTime.Add(10 * time.Second) }
	token, _, err = fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.verifier.Verify(token, fixture.expected); !errors.Is(err, ErrCapabilityNotYetValid) {
		t.Fatalf("future token error=%v", err)
	}

	fixture = newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	fixture.issue.NotBefore = capabilityTestTime.Add(5 * time.Second)
	token, _, err = fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.verifier.Verify(token, fixture.expected); !errors.Is(err, ErrCapabilityNotYetValid) {
		t.Fatalf("future not-before error=%v", err)
	}
}

func TestCapabilityRejectsWrongRequestContext(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	token, _, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	otherDigest, err := RequestDigest(CapabilityRunCommand, []byte("another canonical request"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*CapabilityExpected)
	}{
		{"audience", func(value *CapabilityExpected) { value.Audience = "another-relay" }},
		{"node", func(value *CapabilityExpected) { value.NodeID = "node-par-02" }},
		{"boot-epoch", func(value *CapabilityExpected) { value.BootEpoch = "boot-new" }},
		{"route", func(value *CapabilityExpected) { value.RouteID = "route-other" }},
		{"generation", func(value *CapabilityExpected) { value.Generation++ }},
		{"project", func(value *CapabilityExpected) { value.ProjectID = "project-b" }},
		{"sandbox", func(value *CapabilityExpected) { value.SandboxID = "sandbox-b" }},
		{"operation", func(value *CapabilityExpected) { value.Operation = CapabilityReadFile }},
		{"digest", func(value *CapabilityExpected) { value.RequestDigest = otherDigest }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expected := fixture.expected
			test.mutate(&expected)
			if _, err := fixture.verifier.Verify(token, expected); !errors.Is(err, ErrCapabilityMismatch) {
				t.Fatalf("mismatch error=%v", err)
			}
		})
	}
}

func TestCapabilityRejectsWrongIssuerAndUnknownKey(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	token, claims, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	wrongIssuer, err := NewCapabilityVerifier("another-issuer", map[string]ed25519.PublicKey{"node-key-1": fixture.public})
	if err != nil {
		t.Fatal(err)
	}
	wrongIssuer.now = fixture.verifier.now
	if _, err := wrongIssuer.Verify(token, fixture.expected); !errors.Is(err, ErrCapabilityMismatch) {
		t.Fatalf("wrong issuer error=%v", err)
	}

	claims.KeyID = "retired-key"
	header := capabilityHeader{Type: capabilityTokenType, Algorithm: capabilityAlgorithm, KeyID: "retired-key"}
	token = signCapabilityForTest(t, fixture.private, header, claims)
	if _, err := fixture.verifier.Verify(token, fixture.expected); !errors.Is(err, ErrUnknownCapabilityKey) {
		t.Fatalf("unknown key error=%v", err)
	}
}

func TestCapabilityRejectsOverlongTTL(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	fixture.issue.TTL = MaxCapabilityTTL + time.Second
	if _, _, err := fixture.signer.Issue(fixture.issue); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("overlong issue error=%v", err)
	}

	fixture.issue.TTL = MaxCapabilityTTL
	_, claims, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	claims.ExpiresAt++
	token := signCapabilityForTest(t, fixture.private, capabilityHeader{Type: capabilityTokenType, Algorithm: capabilityAlgorithm, KeyID: claims.KeyID}, claims)
	if _, err := fixture.verifier.Verify(token, fixture.expected); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("overlong verified token error=%v", err)
	}
}

func TestCapabilityRequiresExactlyOneOperation(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	_, claims, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	header := capabilityHeader{Type: capabilityTokenType, Algorithm: capabilityAlgorithm, KeyID: claims.KeyID}
	for name, operations := range map[string][]CapabilityOperation{
		"none":    nil,
		"two":     {CapabilityRunCommand, CapabilityReadFile},
		"unknown": {"node.root"},
	} {
		t.Run(name, func(t *testing.T) {
			modified := claims
			modified.Operations = operations
			token := signCapabilityForTest(t, fixture.private, header, modified)
			if _, err := fixture.verifier.Verify(token, fixture.expected); !errors.Is(err, ErrInvalidCapability) {
				t.Fatalf("operations=%v error=%v", operations, err)
			}
		})
	}
}

func TestCapabilityEnforcesOperationSpecificBounds(t *testing.T) {
	tests := []struct {
		name      string
		operation CapabilityOperation
		bounds    CapabilityBounds
	}{
		{"command-missing-duration", CapabilityRunCommand, CapabilityBounds{MaxResponseBytes: 1}},
		{"command-with-request", CapabilityRunCommand, CapabilityBounds{MaxDurationMillis: 1, MaxRequestBytes: 1, MaxResponseBytes: 1}},
		{"read-with-port", CapabilityReadFile, CapabilityBounds{MaxResponseBytes: 1, Port: 1}},
		{"write-with-response", CapabilityWriteFile, CapabilityBounds{MaxRequestBytes: 1, MaxResponseBytes: 1}},
		{"proxy-missing-request", CapabilityProxyPort, CapabilityBounds{Port: 3000, MaxDurationMillis: 1, MaxResponseBytes: 1}},
		{"port-out-of-range", CapabilityProxyPort, CapabilityBounds{Port: 65536, MaxDurationMillis: 1, MaxRequestBytes: 1, MaxResponseBytes: 1}},
		{"byte-bound-too-large", CapabilityWriteFile, CapabilityBounds{MaxRequestBytes: maxCapabilityBytes + 1}},
		{"duration-too-large", CapabilityRunCommand, CapabilityBounds{MaxDurationMillis: maxOperationMillis + 1, MaxResponseBytes: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCapabilityFixture(t, test.operation, test.bounds)
			if _, _, err := fixture.signer.Issue(fixture.issue); !errors.Is(err, ErrInvalidCapability) {
				t.Fatalf("invalid bounds accepted: %#v error=%v", test.bounds, err)
			}
		})
	}
}

func TestCapabilityRejectsMalformedTokens(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	valid, claims, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(valid, ".")
	header := capabilityHeader{Type: capabilityTokenType, Algorithm: capabilityAlgorithm, KeyID: claims.KeyID}

	badVersion := claims
	badVersion.Version++
	badJTI := claims
	badJTI.JTI = "short"
	badDigest := claims
	badDigest.RequestDigest = "short"
	zeroGeneration := claims
	zeroGeneration.Generation = 0
	unsafeIdentity := claims
	unsafeIdentity.RouteID = "route\nleak"
	wrongKeyClaim := claims
	wrongKeyClaim.KeyID = "different-key"
	wronTypeHeader := header
	wronTypeHeader.Type = "JWT"
	wrongAlgorithmHeader := header
	wrongAlgorithmHeader.Algorithm = "none"

	tests := map[string]string{
		"empty":              "",
		"one-segment":        "abc",
		"two-segments":       "abc.def",
		"four-segments":      "a.b.c.d",
		"bad-header-base64":  "!." + parts[1] + "." + parts[2],
		"bad-signature":      parts[0] + "." + parts[1] + ".!",
		"wrong-type":         signCapabilityForTest(t, fixture.private, wronTypeHeader, claims),
		"wrong-algorithm":    signCapabilityForTest(t, fixture.private, wrongAlgorithmHeader, claims),
		"bad-version":        signCapabilityForTest(t, fixture.private, header, badVersion),
		"bad-jti":            signCapabilityForTest(t, fixture.private, header, badJTI),
		"bad-digest":         signCapabilityForTest(t, fixture.private, header, badDigest),
		"zero-generation":    signCapabilityForTest(t, fixture.private, header, zeroGeneration),
		"unsafe-identity":    signCapabilityForTest(t, fixture.private, header, unsafeIdentity),
		"key-claim-mismatch": signCapabilityForTest(t, fixture.private, header, wrongKeyClaim),
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := fixture.verifier.Verify(token, fixture.expected); err == nil {
				t.Fatal("malformed token was accepted")
			}
		})
	}
}

func TestCapabilityRejectsNonCanonicalOrUnknownJSON(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	_, claims, err := fixture.signer.Issue(fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	header := capabilityHeader{Type: capabilityTokenType, Algorithm: capabilityAlgorithm, KeyID: claims.KeyID}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)

	nonCanonicalClaims := append([]byte(" "), claimsJSON...)
	unknownHeader := []byte(`{"typ":"BREZEL-NODE-CAPABILITY","alg":"EdDSA","kid":"node-key-1","extra":true}`)
	for name, token := range map[string]string{
		"non-canonical-claims": signCapabilityJSONForTest(fixture.private, headerJSON, nonCanonicalClaims),
		"unknown-header-field": signCapabilityJSONForTest(fixture.private, unknownHeader, claimsJSON),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fixture.verifier.Verify(token, fixture.expected); !errors.Is(err, ErrMalformedCapability) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRequestDigestIsDeterministicAndOperationBound(t *testing.T) {
	first, err := RequestDigest(CapabilityRunCommand, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	second, _ := RequestDigest(CapabilityRunCommand, []byte("same"))
	otherOperation, _ := RequestDigest(CapabilityWriteFile, []byte("same"))
	if first != second || first == otherOperation {
		t.Fatalf("digest binding failed: first=%q second=%q other=%q", first, second, otherOperation)
	}
	if _, err := RequestDigest("node.root", nil); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("unknown operation error=%v", err)
	}
}

func TestCapabilityConstructorsCopyKeysAndRejectInvalidConfiguration(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewCapabilitySigner("issuer", "key", private)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewCapabilityVerifier("issuer", map[string]ed25519.PublicKey{"key": public})
	if err != nil {
		t.Fatal(err)
	}
	private[0] ^= 1
	public[0] ^= 1
	signer.now = func() time.Time { return capabilityTestTime }
	verifier.now = signer.now
	digest, _ := RequestDigest(CapabilityRunCommand, []byte("request"))
	issue := CapabilityIssue{Audience: "aud", NodeID: "node", BootEpoch: "epoch", RouteID: "route", Generation: 1, ProjectID: "project", SandboxID: "sandbox", Operation: CapabilityRunCommand, RequestDigest: digest, Bounds: commandBounds(), TTL: time.Second}
	token, _, err := signer.Issue(issue)
	if err != nil {
		t.Fatal(err)
	}
	expected := CapabilityExpected{Audience: issue.Audience, NodeID: issue.NodeID, BootEpoch: issue.BootEpoch, RouteID: issue.RouteID, Generation: issue.Generation, ProjectID: issue.ProjectID, SandboxID: issue.SandboxID, Operation: issue.Operation, RequestDigest: issue.RequestDigest}
	if _, err := verifier.Verify(token, expected); err != nil {
		t.Fatalf("key mutation affected signer or verifier: %v", err)
	}

	if _, err := NewCapabilitySigner("", "key", make([]byte, ed25519.PrivateKeySize)); err == nil {
		t.Fatal("empty issuer accepted")
	}
	if _, err := NewCapabilitySigner("issuer", "key", []byte("short")); err == nil {
		t.Fatal("short private key accepted")
	}
	if _, err := NewCapabilityVerifier("issuer", nil); err == nil {
		t.Fatal("empty key set accepted")
	}
	if _, err := NewCapabilityVerifier("issuer", map[string]ed25519.PublicKey{"key": []byte("short")}); err == nil {
		t.Fatal("short public key accepted")
	}
}

func TestCapabilityFailsClosedWhenRandomSourceFails(t *testing.T) {
	fixture := newCapabilityFixture(t, CapabilityRunCommand, commandBounds())
	fixture.signer.random = errorReader{}
	if _, _, err := fixture.signer.Issue(fixture.issue); err == nil {
		t.Fatal("random source failure was accepted")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func signCapabilityForTest(t *testing.T, private ed25519.PrivateKey, header capabilityHeader, claims CapabilityClaims) string {
	t.Helper()
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return signCapabilityJSONForTest(private, headerJSON, claimsJSON)
}

func signCapabilityJSONForTest(private ed25519.PrivateKey, headerJSON, claimsJSON []byte) string {
	header := base64.RawURLEncoding.EncodeToString(headerJSON)
	claims := base64.RawURLEncoding.EncodeToString(claimsJSON)
	input := header + "." + claims
	signature := ed25519.Sign(private, capabilityMessage(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}
