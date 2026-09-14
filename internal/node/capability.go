package node

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// A node capability authorizes one narrowly bounded operation against one
// sandbox generation. Capabilities are bearer credentials and must never be
// logged, persisted with customer content, or returned from public resource
// APIs.
const (
	CapabilityVersion = 1
	MaxCapabilityTTL  = 30 * time.Second

	capabilityTokenType  = "BREZEL-NODE-CAPABILITY"
	capabilityAlgorithm  = "EdDSA"
	capabilitySignDomain = "brezel.node.capability.v1\x00"
	requestDigestDomain  = "brezel.node.request.v1\x00"
	maxCapabilitySize    = 8 << 10
	maxCapabilityBytes   = 1 << 30
	maxOperationMillis   = int64((24 * time.Hour) / time.Millisecond)
)

var (
	ErrMalformedCapability        = errors.New("malformed node capability")
	ErrInvalidCapability          = errors.New("invalid node capability")
	ErrUnknownCapabilityKey       = errors.New("unknown node capability key")
	ErrInvalidCapabilitySignature = errors.New("invalid node capability signature")
	ErrCapabilityNotYetValid      = errors.New("node capability is not yet valid")
	ErrCapabilityExpired          = errors.New("node capability has expired")
	ErrCapabilityMismatch         = errors.New("node capability does not match request")
)

// CapabilityOperation is deliberately closed. Adding an operation changes the
// node's authority surface and requires an explicit review of its bounds.
type CapabilityOperation string

const (
	CapabilityRunCommand CapabilityOperation = "command.run"
	CapabilityReadFile   CapabilityOperation = "file.read"
	CapabilityWriteFile  CapabilityOperation = "file.write"
	CapabilityProxyPort  CapabilityOperation = "port.proxy"
)

// CapabilityBounds are interpreted according to the capability's sole
// operation. Unused fields must be zero so adding a field cannot accidentally
// broaden an existing operation.
type CapabilityBounds struct {
	MaxDurationMillis int64  `json:"max_duration_ms,omitempty"`
	MaxRequestBytes   int64  `json:"max_request_bytes,omitempty"`
	MaxResponseBytes  int64  `json:"max_response_bytes,omitempty"`
	Port              uint32 `json:"port,omitempty"`
}

type capabilityHeader struct {
	Type      string `json:"typ"`
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
}

// CapabilityClaims contain only authorization metadata. They intentionally
// exclude commands, paths, request bodies, output, credentials, and other
// customer content; those inputs are bound by RequestDigest instead.
type CapabilityClaims struct {
	Version       int                   `json:"v"`
	KeyID         string                `json:"kid"`
	Issuer        string                `json:"iss"`
	Audience      string                `json:"aud"`
	NodeID        string                `json:"node"`
	BootEpoch     string                `json:"epoch"`
	RouteID       string                `json:"route"`
	Generation    uint64                `json:"generation"`
	ProjectID     string                `json:"project"`
	SandboxID     string                `json:"sandbox"`
	JTI           string                `json:"jti"`
	Operations    []CapabilityOperation `json:"ops"`
	RequestDigest string                `json:"request_digest"`
	IssuedAt      int64                 `json:"iat"`
	NotBefore     int64                 `json:"nbf"`
	ExpiresAt     int64                 `json:"exp"`
	Bounds        CapabilityBounds      `json:"bounds"`
}

// CapabilityIssue requests a single-operation capability. RequestDigest must
// be produced from the exact canonical operation request with RequestDigest.
type CapabilityIssue struct {
	Audience      string
	NodeID        string
	BootEpoch     string
	RouteID       string
	Generation    uint64
	ProjectID     string
	SandboxID     string
	Operation     CapabilityOperation
	RequestDigest string
	Bounds        CapabilityBounds
	TTL           time.Duration
	NotBefore     time.Time
}

// CapabilityExpected is the complete node-local context against which a token
// is verified. No routing or authorization identity is taken from the token
// without matching this independently established context.
type CapabilityExpected struct {
	Audience      string
	NodeID        string
	BootEpoch     string
	RouteID       string
	Generation    uint64
	ProjectID     string
	SandboxID     string
	Operation     CapabilityOperation
	RequestDigest string
}

// CapabilitySigner issues short-lived node capabilities under one issuer and
// key identity.
type CapabilitySigner struct {
	issuer  string
	keyID   string
	private ed25519.PrivateKey
	now     func() time.Time
	random  io.Reader
}

// NewCapabilitySigner copies the private key so subsequent caller mutation
// cannot change the signing identity.
func NewCapabilitySigner(issuer, keyID string, private ed25519.PrivateKey) (*CapabilitySigner, error) {
	if err := validateIdentity("issuer", issuer); err != nil {
		return nil, err
	}
	if err := validateIdentity("key id", keyID); err != nil {
		return nil, err
	}
	if len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid node capability private key")
	}
	return &CapabilitySigner{
		issuer:  issuer,
		keyID:   keyID,
		private: append(ed25519.PrivateKey(nil), private...),
		now:     func() time.Time { return time.Now().UTC() },
		random:  rand.Reader,
	}, nil
}

// Issue returns a compact, self-contained, signed token. It accepts no caller-
// supplied JTI; every issuance receives 128 bits from crypto/rand.
func (s *CapabilitySigner) Issue(request CapabilityIssue) (string, CapabilityClaims, error) {
	if s == nil || len(s.private) != ed25519.PrivateKeySize || s.now == nil || s.random == nil {
		return "", CapabilityClaims{}, errors.New("node capability signer is not initialized")
	}
	if request.TTL < time.Second || request.TTL > MaxCapabilityTTL || request.TTL%time.Second != 0 {
		return "", CapabilityClaims{}, fmt.Errorf("%w: ttl must be whole seconds between 1s and %s", ErrInvalidCapability, MaxCapabilityTTL)
	}
	now := s.now().UTC().Truncate(time.Second)
	notBefore := request.NotBefore.UTC().Truncate(time.Second)
	if request.NotBefore.IsZero() {
		notBefore = now
	}
	expires := now.Add(request.TTL)
	if notBefore.Before(now) || !notBefore.Before(expires) {
		return "", CapabilityClaims{}, fmt.Errorf("%w: not-before must be within the capability lifetime", ErrInvalidCapability)
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(s.random, nonce); err != nil {
		return "", CapabilityClaims{}, fmt.Errorf("generate node capability id: %w", err)
	}
	claims := CapabilityClaims{
		Version:       CapabilityVersion,
		KeyID:         s.keyID,
		Issuer:        s.issuer,
		Audience:      request.Audience,
		NodeID:        request.NodeID,
		BootEpoch:     request.BootEpoch,
		RouteID:       request.RouteID,
		Generation:    request.Generation,
		ProjectID:     request.ProjectID,
		SandboxID:     request.SandboxID,
		JTI:           base64.RawURLEncoding.EncodeToString(nonce),
		Operations:    []CapabilityOperation{request.Operation},
		RequestDigest: request.RequestDigest,
		IssuedAt:      now.Unix(),
		NotBefore:     notBefore.Unix(),
		ExpiresAt:     expires.Unix(),
		Bounds:        request.Bounds,
	}
	if err := validateCapabilityClaims(claims); err != nil {
		return "", CapabilityClaims{}, err
	}
	header := capabilityHeader{Type: capabilityTokenType, Algorithm: capabilityAlgorithm, KeyID: s.keyID}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", CapabilityClaims{}, fmt.Errorf("encode node capability header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", CapabilityClaims{}, fmt.Errorf("encode node capability claims: %w", err)
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	encodedClaims := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := encodedHeader + "." + encodedClaims
	signature := ed25519.Sign(s.private, capabilityMessage(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), claims, nil
}

// CapabilityVerifier validates node capabilities using a fixed key set. The
// map and every public key are copied at construction.
type CapabilityVerifier struct {
	issuer string
	keys   map[string]ed25519.PublicKey
	now    func() time.Time
}

func NewCapabilityVerifier(issuer string, keys map[string]ed25519.PublicKey) (*CapabilityVerifier, error) {
	if err := validateIdentity("issuer", issuer); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, errors.New("at least one node capability verification key is required")
	}
	copied := make(map[string]ed25519.PublicKey, len(keys))
	for keyID, public := range keys {
		if err := validateIdentity("key id", keyID); err != nil {
			return nil, err
		}
		if len(public) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid node capability public key %q", keyID)
		}
		copied[keyID] = append(ed25519.PublicKey(nil), public...)
	}
	return &CapabilityVerifier{issuer: issuer, keys: copied, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Authenticate verifies a token's canonical envelope, signing key, signature,
// configured issuer, time window, and intrinsic claims. It deliberately does
// not decide whether those claims match a caller's route or request. Callers
// must pass the returned claims to Match before using them for authorization.
// Returned claims are safe authorization metadata, not proof that the
// operation was executed.
func (v *CapabilityVerifier) Authenticate(token string) (CapabilityClaims, error) {
	if v == nil || v.now == nil || len(v.keys) == 0 {
		return CapabilityClaims{}, errors.New("node capability verifier is not initialized")
	}
	if len(token) == 0 || len(token) > maxCapabilitySize {
		return CapabilityClaims{}, ErrMalformedCapability
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return CapabilityClaims{}, ErrMalformedCapability
	}
	var header capabilityHeader
	if err := decodeCanonicalSegment(parts[0], &header); err != nil {
		return CapabilityClaims{}, fmt.Errorf("%w: header", ErrMalformedCapability)
	}
	if header.Type != capabilityTokenType || header.Algorithm != capabilityAlgorithm {
		return CapabilityClaims{}, fmt.Errorf("%w: unsupported token header", ErrInvalidCapability)
	}
	public, ok := v.keys[header.KeyID]
	if !ok {
		return CapabilityClaims{}, ErrUnknownCapabilityKey
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return CapabilityClaims{}, ErrMalformedCapability
	}
	signingInput := parts[0] + "." + parts[1]
	if !ed25519.Verify(public, capabilityMessage(signingInput), signature) {
		return CapabilityClaims{}, ErrInvalidCapabilitySignature
	}
	var claims CapabilityClaims
	if err := decodeCanonicalSegment(parts[1], &claims); err != nil {
		return CapabilityClaims{}, fmt.Errorf("%w: claims", ErrMalformedCapability)
	}
	if header.KeyID != claims.KeyID {
		return CapabilityClaims{}, fmt.Errorf("%w: key id mismatch", ErrInvalidCapability)
	}
	if err := validateCapabilityClaims(claims); err != nil {
		return CapabilityClaims{}, err
	}
	if claims.Issuer != v.issuer {
		return CapabilityClaims{}, fmt.Errorf("%w: issuer", ErrCapabilityMismatch)
	}
	now := v.now().UTC().Unix()
	if now < claims.IssuedAt || now < claims.NotBefore {
		return CapabilityClaims{}, ErrCapabilityNotYetValid
	}
	if now >= claims.ExpiresAt {
		return CapabilityClaims{}, ErrCapabilityExpired
	}
	return claims, nil
}

// Match checks authenticated claims against the complete independently
// established node-local request context. Claims passed here must be the
// unchanged result of Authenticate; Match does not authenticate an arbitrary
// CapabilityClaims value.
func (v *CapabilityVerifier) Match(claims CapabilityClaims, expected CapabilityExpected) error {
	if v == nil || v.now == nil || len(v.keys) == 0 {
		return errors.New("node capability verifier is not initialized")
	}
	// Authenticate is the authority for signatures and time, but keep this
	// public stage total and fail closed if a caller supplies malformed or
	// issuer-inconsistent claims instead of its unchanged result.
	if err := validateCapabilityClaims(claims); err != nil || claims.Issuer != v.issuer {
		return ErrCapabilityMismatch
	}
	if err := validateExpected(expected); err != nil {
		return err
	}
	if claims.Audience != expected.Audience ||
		claims.NodeID != expected.NodeID ||
		claims.BootEpoch != expected.BootEpoch ||
		claims.RouteID != expected.RouteID ||
		claims.Generation != expected.Generation ||
		claims.ProjectID != expected.ProjectID ||
		claims.SandboxID != expected.SandboxID ||
		claims.Operations[0] != expected.Operation ||
		!equalDigest(claims.RequestDigest, expected.RequestDigest) {
		return ErrCapabilityMismatch
	}
	return nil
}

// Verify authenticates a token before checking its claims against the complete
// expected request context.
func (v *CapabilityVerifier) Verify(token string, expected CapabilityExpected) (CapabilityClaims, error) {
	claims, err := v.Authenticate(token)
	if err != nil {
		return CapabilityClaims{}, err
	}
	if err := v.Match(claims, expected); err != nil {
		return CapabilityClaims{}, err
	}
	return claims, nil
}

// RequestDigest binds a capability to caller-defined canonical request bytes
// and the operation namespace without placing request content in the token.
func RequestDigest(operation CapabilityOperation, canonicalRequest []byte) (string, error) {
	if !validOperation(operation) {
		return "", fmt.Errorf("%w: unsupported operation", ErrInvalidCapability)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(requestDigestDomain))
	_, _ = hash.Write([]byte(operation))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(canonicalRequest)
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil)), nil
}

func validateCapabilityClaims(claims CapabilityClaims) error {
	if claims.Version != CapabilityVersion {
		return fmt.Errorf("%w: unsupported version", ErrInvalidCapability)
	}
	for name, value := range map[string]string{
		"key id": claims.KeyID, "issuer": claims.Issuer, "audience": claims.Audience,
		"node id": claims.NodeID, "boot epoch": claims.BootEpoch, "route id": claims.RouteID,
		"project id": claims.ProjectID, "sandbox id": claims.SandboxID,
	} {
		if err := validateIdentity(name, value); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidCapability, err)
		}
	}
	if claims.Generation == 0 {
		return fmt.Errorf("%w: generation must be positive", ErrInvalidCapability)
	}
	if len(claims.Operations) != 1 || !validOperation(claims.Operations[0]) {
		return fmt.Errorf("%w: exactly one supported operation is required", ErrInvalidCapability)
	}
	if !validEncodedSize(claims.JTI, 16) {
		return fmt.Errorf("%w: invalid capability id", ErrInvalidCapability)
	}
	if !validEncodedSize(claims.RequestDigest, sha256.Size) {
		return fmt.Errorf("%w: invalid request digest", ErrInvalidCapability)
	}
	if claims.IssuedAt <= 0 || claims.NotBefore < claims.IssuedAt || claims.NotBefore >= claims.ExpiresAt || claims.ExpiresAt-claims.IssuedAt > int64(MaxCapabilityTTL/time.Second) {
		return fmt.Errorf("%w: invalid capability lifetime", ErrInvalidCapability)
	}
	if err := validateBounds(claims.Operations[0], claims.Bounds); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCapability, err)
	}
	return nil
}

func validateExpected(expected CapabilityExpected) error {
	for name, value := range map[string]string{
		"audience": expected.Audience, "node id": expected.NodeID, "boot epoch": expected.BootEpoch,
		"route id": expected.RouteID, "project id": expected.ProjectID, "sandbox id": expected.SandboxID,
	} {
		if err := validateIdentity(name, value); err != nil {
			return fmt.Errorf("%w: invalid expected %s", ErrCapabilityMismatch, name)
		}
	}
	if expected.Generation == 0 || !validOperation(expected.Operation) || !validEncodedSize(expected.RequestDigest, sha256.Size) {
		return fmt.Errorf("%w: invalid expected operation context", ErrCapabilityMismatch)
	}
	return nil
}

func validateBounds(operation CapabilityOperation, bounds CapabilityBounds) error {
	if bounds.MaxDurationMillis < 0 || bounds.MaxDurationMillis > maxOperationMillis ||
		bounds.MaxRequestBytes < 0 || bounds.MaxRequestBytes > maxCapabilityBytes ||
		bounds.MaxResponseBytes < 0 || bounds.MaxResponseBytes > maxCapabilityBytes ||
		bounds.Port > 65535 {
		return errors.New("operation bound is outside the supported range")
	}
	switch operation {
	case CapabilityRunCommand:
		if bounds.MaxDurationMillis == 0 || bounds.MaxResponseBytes == 0 || bounds.MaxRequestBytes != 0 || bounds.Port != 0 {
			return errors.New("command requires duration and response bounds only")
		}
	case CapabilityReadFile:
		if bounds.MaxResponseBytes == 0 || bounds.MaxDurationMillis != 0 || bounds.MaxRequestBytes != 0 || bounds.Port != 0 {
			return errors.New("file read requires a response bound only")
		}
	case CapabilityWriteFile:
		if bounds.MaxRequestBytes == 0 || bounds.MaxDurationMillis != 0 || bounds.MaxResponseBytes != 0 || bounds.Port != 0 {
			return errors.New("file write requires a request bound only")
		}
	case CapabilityProxyPort:
		if bounds.Port == 0 || bounds.MaxDurationMillis == 0 || bounds.MaxRequestBytes == 0 || bounds.MaxResponseBytes == 0 {
			return errors.New("port proxy requires port, duration, request, and response bounds")
		}
	default:
		return errors.New("unsupported operation")
	}
	return nil
}

func validOperation(operation CapabilityOperation) bool {
	switch operation {
	case CapabilityRunCommand, CapabilityReadFile, CapabilityWriteFile, CapabilityProxyPort:
		return true
	default:
		return false
	}
}

func validateIdentity(name, value string) error {
	if value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
		return fmt.Errorf("invalid %s", name)
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("invalid %s", name)
		}
	}
	return nil
}

func validEncodedSize(value string, size int) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == size && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func equalDigest(left, right string) bool {
	leftBytes, leftErr := base64.RawURLEncoding.Strict().DecodeString(left)
	rightBytes, rightErr := base64.RawURLEncoding.Strict().DecodeString(right)
	return leftErr == nil && rightErr == nil && len(leftBytes) == sha256.Size && len(rightBytes) == sha256.Size && subtle.ConstantTimeCompare(leftBytes, rightBytes) == 1
}

func decodeCanonicalSegment(segment string, destination any) error {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(segment)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	canonical, err := json.Marshal(destination)
	if err != nil || !bytes.Equal(canonical, decoded) {
		return errors.New("non-canonical JSON")
	}
	return nil
}

func capabilityMessage(signingInput string) []byte {
	message := make([]byte, 0, len(capabilitySignDomain)+len(signingInput))
	message = append(message, capabilitySignDomain...)
	message = append(message, signingInput...)
	return message
}
