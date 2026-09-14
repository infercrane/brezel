// Package nodeidentity establishes the mutually authenticated TLS boundary
// between the Brezel API and a Brezel execution node.
package nodeidentity

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

const trustDomain = "brezel"

var safeID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Role is a workload identity role in the private node-control network.
type Role string

const (
	RoleNode Role = "node"
	RoleAPI  Role = "api"
)

// Identity is an exact URI SAN identity. Its wire representation is
// spiffe://brezel/<role>/<id>.
type Identity struct {
	Role Role
	ID   string
}

func NewIdentity(role Role, id string) (Identity, error) {
	if role != RoleNode && role != RoleAPI {
		return Identity{}, errors.New("identity role must be node or api")
	}
	if !safeID.MatchString(id) {
		return Identity{}, errors.New("identity id must contain 1-63 lowercase alphanumeric, dot, underscore, or hyphen characters and start with an alphanumeric character")
	}
	return Identity{Role: role, ID: id}, nil
}

func (i Identity) URI() (*url.URL, error) {
	valid, err := NewIdentity(i.Role, i.ID)
	if err != nil {
		return nil, err
	}
	return url.Parse(fmt.Sprintf("spiffe://%s/%s/%s", trustDomain, valid.Role, valid.ID))
}

func (i Identity) String() string {
	u, err := i.URI()
	if err != nil {
		return "invalid Brezel identity"
	}
	return u.String()
}
