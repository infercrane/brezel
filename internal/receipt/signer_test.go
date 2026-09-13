package receipt

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestSignedEnvelopeDetectsTampering(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := NewSigner(private)
	envelope, err := signer.Sign(Statement{Type: "https://in-toto.io/Statement/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(envelope, signer.PublicKey()); err != nil {
		t.Fatal(err)
	}
	envelope.Payload += "A"
	if err := Verify(envelope, signer.PublicKey()); err == nil {
		t.Fatal("tampered envelope verified")
	}
}
