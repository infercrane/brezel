package access

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestPolicyBindsTokenToProjects(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	digest := sha256.Sum256([]byte(token))
	path := filepath.Join(t.TempDir(), "access.json")
	document := `{"version":1,"principals":[{"name":"runtime-client","token_sha256":"` + hex.EncodeToString(digest[:]) + `","projects":["project-a","project-b"]}]}`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated, authorized := policy.Authorize(token, "project-a"); !authenticated || !authorized {
		t.Fatalf("valid binding = %t, %t", authenticated, authorized)
	}
	if authenticated, authorized := policy.Authorize(token, "project-c"); !authenticated || authorized {
		t.Fatalf("unbound project = %t, %t", authenticated, authorized)
	}
	if authenticated, authorized := policy.Authorize("wrong-token", "project-a"); authenticated || authorized {
		t.Fatalf("invalid token = %t, %t", authenticated, authorized)
	}
}

func TestPolicyRejectsUnsafeAndAmbiguousDocuments(t *testing.T) {
	directory := t.TempDir()
	token := sha256.Sum256([]byte("0123456789abcdef0123456789abcdef"))
	digest := hex.EncodeToString(token[:])
	cases := map[string]string{
		"unknown field":     `{"version":1,"extra":true,"principals":[]}`,
		"missing principal": `{"version":1,"principals":[]}`,
		"duplicate digest":  `{"version":1,"principals":[{"name":"a","token_sha256":"` + digest + `","projects":["a"]},{"name":"b","token_sha256":"` + digest + `","projects":["b"]}]}`,
		"wildcard project":  `{"version":1,"principals":[{"name":"a","token_sha256":"` + digest + `","projects":["*"]}]}`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, name+".json")
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadFile(path); err == nil {
				t.Fatal("invalid access policy was accepted")
			}
		})
	}
	public := filepath.Join(directory, "public.json")
	if err := os.WriteFile(public, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(public); err == nil {
		t.Fatal("group-readable access policy was accepted")
	}
}
