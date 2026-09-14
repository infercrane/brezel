package domain

import "testing"

func TestLifecycleRejectsFilesystemAutoResume(t *testing.T) {
	err := ValidateLifecycle(Lifecycle{
		StandbyAfterSeconds: 30,
		ExpiresAfterSeconds: 3600,
		StandbyCheckpoint:   CheckpointFilesystem,
		AutoResume:          true,
	})
	if err == nil {
		t.Fatal("expected filesystem auto-resume to be rejected")
	}
}

func TestLifecycleValidatesStandbyGrace(t *testing.T) {
	for _, lifecycle := range []Lifecycle{
		{StandbyGraceSeconds: 1, ExpiresAfterSeconds: 3600},
		{StandbyAfterSeconds: 30, StandbyGraceSeconds: 301, ExpiresAfterSeconds: 3600, StandbyCheckpoint: CheckpointFullState},
		{StandbyAfterSeconds: 30, StandbyGraceSeconds: 30, ExpiresAfterSeconds: 60, StandbyCheckpoint: CheckpointFullState},
	} {
		if err := ValidateLifecycle(lifecycle); err == nil {
			t.Fatalf("expected lifecycle %#v to be rejected", lifecycle)
		}
	}
	if err := ValidateLifecycle(Lifecycle{StandbyAfterSeconds: 30, StandbyGraceSeconds: 15, ExpiresAfterSeconds: 3600, StandbyCheckpoint: CheckpointFullState}); err != nil {
		t.Fatalf("valid standby grace rejected: %v", err)
	}
}

func TestTransitionGraphKeepsStandbyAndDeletionDistinct(t *testing.T) {
	if !CanTransition(SandboxRunning, SandboxPausing) || !CanTransition(SandboxPausing, SandboxStandby) {
		t.Fatal("expected running -> pausing -> standby")
	}
	if CanTransition(SandboxStandby, SandboxDeleted) {
		t.Fatal("standby must pass through deleting before deleted")
	}
}

func TestConnectorRequiresOpaqueSecretHandle(t *testing.T) {
	err := ValidateConnector(Connector{
		ProjectID: "project-a", Name: "model", Destination: "https://models.internal/v1",
		AllowedMethods: []string{"POST"}, AllowedPaths: []string{"/chat/completions"}, CredentialRef: "sk-live",
	})
	if err == nil {
		t.Fatal("expected raw credential to be rejected")
	}
}
