package domain

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func ValidateProjectID(id string) error {
	if !safeName.MatchString(id) {
		return errors.New("project id must contain 1-128 letters, numbers, dots, underscores, or hyphens")
	}
	return nil
}

func ValidateEnvironment(e Environment) error {
	if err := ValidateProjectID(e.ProjectID); err != nil {
		return err
	}
	if !safeName.MatchString(e.Name) {
		return errors.New("environment name is invalid")
	}
	if e.Backend != "e2b" {
		return fmt.Errorf("backend %q is unsupported", e.Backend)
	}
	if strings.TrimSpace(e.BackendTemplate) == "" {
		return errors.New("backend_template is required")
	}
	if e.ImageDigest != "" && !strings.HasPrefix(e.ImageDigest, "sha256:") {
		return errors.New("image_digest must be immutable and start with sha256:")
	}
	return nil
}

func ValidateLifecycle(l Lifecycle) error {
	if l.ExpiresAfterSeconds < 30 || l.ExpiresAfterSeconds > 30*24*60*60 {
		return errors.New("expires_after_seconds must be between 30 and 2592000")
	}
	if l.StandbyAfterSeconds < 0 {
		return errors.New("standby_after_seconds cannot be negative")
	}
	if l.StandbyAfterSeconds > 0 && l.StandbyAfterSeconds >= l.ExpiresAfterSeconds {
		return errors.New("standby_after_seconds must be less than expires_after_seconds")
	}
	if l.StandbyAfterSeconds > 0 {
		if l.StandbyCheckpoint != CheckpointFilesystem && l.StandbyCheckpoint != CheckpointFullState {
			return errors.New("standby_checkpoint_kind must be filesystem or full_state")
		}
		if l.AutoResume && l.StandbyCheckpoint == CheckpointFilesystem {
			return errors.New("auto_resume requires a full_state standby checkpoint")
		}
	}
	if l.AutoResume && l.StandbyAfterSeconds == 0 {
		return errors.New("auto_resume requires standby_after_seconds")
	}
	return nil
}

func ValidateNetwork(n NetworkPolicy) error {
	for _, destination := range append(append([]string{}, n.AllowOut...), n.DenyOut...) {
		if strings.TrimSpace(destination) == "" || strings.ContainsAny(destination, "\r\n") {
			return errors.New("network destinations cannot be empty or contain newlines")
		}
	}
	return nil
}

func ValidateConnector(c Connector) error {
	if err := ValidateProjectID(c.ProjectID); err != nil {
		return err
	}
	if !safeName.MatchString(c.Name) {
		return errors.New("connector name is invalid")
	}
	u, err := url.Parse(c.Destination)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("destination must be an https origin or path without credentials, query, or fragment")
	}
	if len(c.AllowedMethods) == 0 || len(c.AllowedPaths) == 0 {
		return errors.New("allowed_methods and allowed_paths are required")
	}
	for _, method := range c.AllowedMethods {
		switch strings.ToUpper(method) {
		case "GET", "POST", "PUT", "PATCH", "DELETE":
		default:
			return fmt.Errorf("method %q is not allowed", method)
		}
	}
	for _, path := range c.AllowedPaths {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n?#") {
			return fmt.Errorf("path %q must be an absolute clean path pattern", path)
		}
		if strings.Contains(path, "*") && (!strings.HasSuffix(path, "/*") || strings.Count(path, "*") != 1) {
			return fmt.Errorf("path %q may only use one trailing /* wildcard", path)
		}
	}
	if !strings.HasPrefix(c.CredentialRef, "secret://") {
		return errors.New("credential_ref must be an opaque secret:// handle")
	}
	return nil
}

func CanTransition(from, to SandboxState) bool {
	allowed := map[SandboxState]map[SandboxState]bool{
		SandboxRequested: {SandboxPreparing: true, SandboxFailed: true},
		SandboxPreparing: {SandboxRunning: true, SandboxFailed: true, SandboxUnknown: true},
		SandboxRunning:   {SandboxPausing: true, SandboxStandby: true, SandboxDeleting: true, SandboxExpired: true, SandboxUnknown: true},
		SandboxPausing:   {SandboxStandby: true, SandboxUnknown: true, SandboxFailed: true},
		SandboxStandby:   {SandboxResuming: true, SandboxRunning: true, SandboxDeleting: true, SandboxExpired: true, SandboxUnknown: true},
		SandboxResuming:  {SandboxRunning: true, SandboxUnknown: true, SandboxFailed: true},
		SandboxDeleting:  {SandboxDeleted: true, SandboxExpired: true, SandboxUnknown: true, SandboxFailed: true},
		SandboxUnknown:   {SandboxRunning: true, SandboxStandby: true, SandboxDeleting: true, SandboxDeleted: true, SandboxExpired: true, SandboxFailed: true},
	}
	return allowed[from][to]
}
