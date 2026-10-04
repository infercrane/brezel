package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const MaxWebSocketSeconds = 5 * 60

func (s *Service) ValidatePortAccess(ctx context.Context, projectID, sandboxID string, port uint16) error {
	operationID, err := randomID("portcheck")
	if err != nil {
		return err
	}
	_, _, release, err := s.acquireGuestOperation(ctx, projectID, sandboxID, operationID)
	if err != nil {
		return err
	}
	release()
	if port == 0 || !s.dataPlane.Capabilities().AuthenticatedPorts {
		return fmt.Errorf("%w: authenticated application ports are unavailable", ErrDenied)
	}
	if err := s.dataPlane.ValidatePort(port); err != nil {
		return fmt.Errorf("%w: invalid application port", ErrInvalid)
	}
	return nil
}

func (s *Service) ValidateWebSocketAccess(ctx context.Context, projectID, sandboxID string, port uint16) error {
	operationID, err := randomID("wscheck")
	if err != nil {
		return err
	}
	_, _, release, err := s.acquireGuestOperation(ctx, projectID, sandboxID, operationID)
	if err != nil {
		return err
	}
	release()
	if port == 0 || !s.dataPlane.Capabilities().AuthenticatedWebSockets {
		return fmt.Errorf("%w: authenticated WebSocket tunnels are unavailable", ErrDenied)
	}
	if err := s.dataPlane.ValidatePort(port); err != nil {
		return fmt.Errorf("%w: invalid application port", ErrInvalid)
	}
	return nil
}

func (s *Service) RoundTripPort(ctx context.Context, projectID, sandboxID string, port uint16, request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, fmt.Errorf("%w: port request is required", ErrInvalid)
	}
	operationID, err := randomID("portreq")
	if err != nil {
		return nil, err
	}
	sandbox, operationCtx, release, err := s.acquireGuestOperation(ctx, projectID, sandboxID, operationID)
	if err != nil {
		return nil, err
	}
	dataPlane, binding, err := s.authorizedNodeDataPlane(sandbox, operationID)
	if err != nil {
		release()
		return nil, err
	}
	if !dataPlane.Capabilities().AuthenticatedPorts {
		release()
		return nil, fmt.Errorf("%w: authenticated application ports are unavailable", ErrDenied)
	}
	response, err := dataPlane.RoundTripPort(operationCtx, binding, port, request)
	if err != nil {
		binding.Revoke()
		release()
		return nil, translateGuestError(err)
	}
	if response == nil || response.Body == nil {
		binding.Revoke()
		release()
		return nil, fmt.Errorf("%w: application port returned an invalid response", ErrBackend)
	}
	response.Body = &releaseReadCloser{ReadCloser: response.Body, release: func() {
		binding.Revoke()
		release()
	}}
	return response, nil
}

// OpenPortWebSocket opens one admitted, generation-bound WebSocket tunnel.
// The caller owns the returned connection and must call the release function
// even when the peer closes unexpectedly.
func (s *Service) OpenPortWebSocket(ctx context.Context, projectID, sandboxID string, port uint16, path string) (*websocket.Conn, func(), error) {
	if path == "" {
		return nil, nil, fmt.Errorf("%w: WebSocket path is required", ErrInvalid)
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > MaxWebSocketSeconds*time.Second {
		return nil, nil, fmt.Errorf("%w: WebSocket tunnel requires a deadline of at most %d seconds", ErrInvalid, MaxWebSocketSeconds)
	}
	operationID, err := randomID("wsop")
	if err != nil {
		return nil, nil, err
	}
	sandbox, operationCtx, release, err := s.acquireGuestOperation(ctx, projectID, sandboxID, operationID)
	if err != nil {
		return nil, nil, err
	}
	dataPlane, binding, err := s.authorizedNodeDataPlane(sandbox, operationID)
	if err != nil {
		release()
		return nil, nil, err
	}
	if !dataPlane.Capabilities().AuthenticatedWebSockets {
		binding.Revoke()
		release()
		return nil, nil, fmt.Errorf("%w: authenticated WebSocket tunnels are unavailable", ErrDenied)
	}
	conn, _, err := dataPlane.OpenPortWebSocket(operationCtx, binding, port, path)
	if err != nil {
		binding.Revoke()
		release()
		return nil, nil, translateGuestError(err)
	}
	var once sync.Once
	finish := func() {
		once.Do(func() {
			binding.Revoke()
			release()
		})
	}
	return conn, finish, nil
}

type releaseReadCloser struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (r *releaseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.release)
	return err
}
