package access

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/securefile"
)

const maxPolicyBytes = 1 << 20

// Policy binds opaque API credentials to an explicit set of projects. Token
// material is never stored in the policy: operators provision SHA-256 digests
// of independently generated high-entropy tokens.
type Policy struct {
	principals []principal
}

type principal struct {
	name     string
	digest   [sha256.Size]byte
	projects map[string]struct{}
}

type policyDocument struct {
	Version    int                 `json:"version"`
	Principals []principalDocument `json:"principals"`
}

type principalDocument struct {
	Name        string   `json:"name"`
	TokenSHA256 string   `json:"token_sha256"`
	Projects    []string `json:"projects"`
}

// LoadFile reads a protected, non-symlink policy file and validates the whole
// document before returning it. Invalid or ambiguous policies fail closed.
func LoadFile(path string) (*Policy, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("access policy file path is required")
	}
	data, err := securefile.Read(path, maxPolicyBytes)
	if err != nil {
		return nil, fmt.Errorf("read access policy: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var document policyDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode access policy: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("access policy must contain one JSON document")
	}
	return parse(document)
}

func parse(document policyDocument) (*Policy, error) {
	if document.Version != 1 {
		return nil, fmt.Errorf("unsupported access policy version %d", document.Version)
	}
	if len(document.Principals) == 0 {
		return nil, errors.New("access policy must define at least one principal")
	}
	names := make(map[string]struct{}, len(document.Principals))
	digests := make(map[string]struct{}, len(document.Principals))
	policy := &Policy{principals: make([]principal, 0, len(document.Principals))}
	for _, item := range document.Principals {
		if err := domain.ValidateProjectID(item.Name); err != nil {
			return nil, fmt.Errorf("principal name %q is invalid: %w", item.Name, err)
		}
		if _, exists := names[item.Name]; exists {
			return nil, fmt.Errorf("principal name %q is duplicated", item.Name)
		}
		names[item.Name] = struct{}{}
		digestText := strings.ToLower(strings.TrimSpace(item.TokenSHA256))
		raw, err := hex.DecodeString(digestText)
		if err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("principal %q token_sha256 must be 64 hexadecimal characters", item.Name)
		}
		if _, exists := digests[digestText]; exists {
			return nil, errors.New("access policy reuses a token digest")
		}
		digests[digestText] = struct{}{}
		if len(item.Projects) == 0 {
			return nil, fmt.Errorf("principal %q must be bound to at least one project", item.Name)
		}
		projects := make(map[string]struct{}, len(item.Projects))
		for _, projectID := range item.Projects {
			if err := domain.ValidateProjectID(projectID); err != nil {
				return nil, fmt.Errorf("principal %q has invalid project %q: %w", item.Name, projectID, err)
			}
			if _, exists := projects[projectID]; exists {
				return nil, fmt.Errorf("principal %q repeats project %q", item.Name, projectID)
			}
			projects[projectID] = struct{}{}
		}
		var digest [sha256.Size]byte
		copy(digest[:], raw)
		policy.principals = append(policy.principals, principal{name: item.Name, digest: digest, projects: projects})
	}
	sort.Slice(policy.principals, func(i, j int) bool { return policy.principals[i].name < policy.principals[j].name })
	return policy, nil
}

// Authorize performs a constant-time comparison against every configured
// digest. The first result says whether the token exists; the second says
// whether that principal may access the requested project.
func (p *Policy) Authorize(token, projectID string) (bool, bool) {
	if p == nil || token == "" {
		return false, false
	}
	digest := sha256.Sum256([]byte(token))
	authenticated := 0
	authorized := 0
	for _, item := range p.principals {
		match := subtle.ConstantTimeCompare(digest[:], item.digest[:])
		authenticated |= match
		if _, ok := item.projects[projectID]; ok {
			authorized |= match
		}
	}
	return authenticated == 1, authorized == 1
}
