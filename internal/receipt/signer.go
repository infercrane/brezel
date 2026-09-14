package receipt

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
)

const PayloadType = "application/vnd.in-toto+json"

type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

type Statement struct {
	Type          string    `json:"_type"`
	Subject       []Subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     Predicate `json:"predicate"`
}

type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

type Predicate struct {
	ProjectID             string              `json:"project_id"`
	SandboxID             string              `json:"sandbox_id"`
	SandboxRevision       int64               `json:"sandbox_revision"`
	State                 domain.SandboxState `json:"state"`
	EnvironmentRevision   string              `json:"environment_revision"`
	SourceCheckpointID    string              `json:"source_checkpoint_id,omitempty"`
	ImageDigest           string              `json:"image_digest,omitempty"`
	PolicyRevision        string              `json:"policy_revision,omitempty"`
	Backend               string              `json:"backend"`
	BackendIDDigest       string              `json:"backend_id_digest,omitempty"`
	Lifecycle             domain.Lifecycle    `json:"lifecycle"`
	NetworkPolicyDigest   string              `json:"network_policy_digest"`
	WorkspaceMountsDigest string              `json:"workspace_mounts_digest,omitempty"`
	EventLogDigest        string              `json:"event_log_digest"`
	InputIdentity         string              `json:"input_identity"`
	OutputIdentity        string              `json:"output_identity"`
	CleanupIdentity       string              `json:"cleanup_identity"`
	Assurance             string              `json:"assurance"`
}

type Signer struct {
	private ed25519.PrivateKey
	public  ed25519.PublicKey
	keyID   string
}

func NewSigner(private ed25519.PrivateKey) (*Signer, error) {
	if len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key")
	}
	public := private.Public().(ed25519.PublicKey)
	digest := sha256.Sum256(public)
	return &Signer{private: private, public: public, keyID: hex.EncodeToString(digest[:])}, nil
}

func (s *Signer) PublicKey() ed25519.PublicKey { return append(ed25519.PublicKey(nil), s.public...) }
func (s *Signer) KeyID() string                { return s.keyID }

func (s *Signer) Sign(statement Statement) (Envelope, error) {
	payload, err := json.Marshal(statement)
	if err != nil {
		return Envelope{}, err
	}
	sig := ed25519.Sign(s.private, pae(PayloadType, payload))
	return Envelope{
		PayloadType: PayloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures:  []Signature{{KeyID: s.keyID, Sig: base64.StdEncoding.EncodeToString(sig)}},
	}, nil
}

func Verify(envelope Envelope, public ed25519.PublicKey) error {
	if envelope.PayloadType != PayloadType || len(envelope.Signatures) != 1 {
		return errors.New("invalid receipt envelope")
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		return fmt.Errorf("decode payload: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(envelope.Signatures[0].Sig)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(public, pae(envelope.PayloadType, payload), sig) {
		return errors.New("invalid receipt signature")
	}
	return nil
}

func DigestJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func DigestString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func pae(payloadType string, payload []byte) []byte {
	return []byte("DSSEv1 " + strconv.Itoa(len(payloadType)) + " " + payloadType + " " + strconv.Itoa(len(payload)) + " " + string(payload))
}
