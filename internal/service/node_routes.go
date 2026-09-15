package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/node"
	"github.com/infercrane/brezel/internal/nodeledger"
)

func (s *Service) attachNodeRoute(ctx context.Context, sandbox domain.Sandbox) (domain.Sandbox, error) {
	if s.routeAdmin == nil {
		return sandbox, nil
	}
	routeID := sandbox.NodeRouteID
	if routeID == "" {
		routeID = sandbox.ID
	}
	result, err := s.routeAdmin.Bind(ctx, node.RouteBindRequest{
		RouteID: routeID, ProjectID: sandbox.ProjectID, SandboxID: sandbox.ID, EngineID: sandbox.BackendID,
	})
	if err != nil {
		return sandbox, fmt.Errorf("bind node route: %w", err)
	}
	if err := applyNodeAssignment(&sandbox, result); err != nil {
		return sandbox, err
	}
	if result.Route.State == nodeledger.StateReady {
		return sandbox, nil
	}
	ready, err := s.routeAdmin.AttachReady(ctx, routeTransition(sandbox, nodeledger.StateAttaching))
	if err != nil {
		return sandbox, fmt.Errorf("activate node route: %w", err)
	}
	if err := applyNodeAssignment(&sandbox, ready); err != nil {
		return sandbox, err
	}
	return sandbox, nil
}

func (s *Service) reconcileNodeRoute(ctx context.Context, sandbox domain.Sandbox, remoteState domain.SandboxState) (domain.Sandbox, error) {
	if s.routeAdmin == nil {
		return sandbox, nil
	}
	if sandbox.NodeRouteID == "" || sandbox.NodeGeneration == 0 || sandbox.NodeID == "" {
		clearNodeAssignment(&sandbox)
		attached, err := s.attachNodeRoute(ctx, sandbox)
		if err != nil {
			return attached, err
		}
		sandbox = attached
	} else {
		resolved, err := s.resolveNodeRoute(ctx, sandbox)
		if err != nil {
			var remoteErr *node.RouteAdminError
			if errors.As(err, &remoteErr) && remoteErr.StatusCode == http.StatusNotFound {
				clearNodeAssignment(&sandbox)
				attached, attachErr := s.attachNodeRoute(ctx, sandbox)
				if attachErr != nil {
					return attached, attachErr
				}
				sandbox = attached
			} else {
				return sandbox, err
			}
		} else {
			var assignmentErr error
			sandbox, assignmentErr = assignmentFromResolved(sandbox, resolved)
			if assignmentErr != nil {
				return sandbox, assignmentErr
			}
			if resolved.Route.State == nodeledger.StateReleased {
				removed, removeErr := s.removeNodeRoute(ctx, sandbox)
				if removeErr != nil {
					return removed, removeErr
				}
				sandbox, err = s.attachNodeRoute(ctx, removed)
				if err != nil {
					return sandbox, err
				}
			}
		}
	}
	switch remoteState {
	case domain.SandboxRunning:
		return s.resumeNodeRoute(ctx, sandbox)
	case domain.SandboxStandby:
		drained, err := s.prepareNodeRoutePause(ctx, sandbox)
		if err != nil {
			return drained, err
		}
		return s.completeNodeRouteStandby(ctx, drained)
	default:
		return sandbox, nil
	}
}

// observeNodeRoute performs the read-only half of route reconciliation. A
// healthy route must remain available to guest operations while the node
// lookup is in flight; callers establish the backend-mutation fence only when
// this observation proves that a route transition or repair is required.
func (s *Service) observeNodeRoute(ctx context.Context, sandbox domain.Sandbox, remoteState domain.SandboxState) (domain.Sandbox, bool, error) {
	if s.routeAdmin == nil {
		return sandbox, false, nil
	}
	if sandbox.NodeRouteID == "" || sandbox.NodeGeneration == 0 || sandbox.NodeID == "" {
		clearNodeAssignment(&sandbox)
		return sandbox, true, nil
	}
	resolved, err := s.resolveNodeRoute(ctx, sandbox)
	if err != nil {
		var remoteErr *node.RouteAdminError
		if errors.As(err, &remoteErr) && remoteErr.StatusCode == http.StatusNotFound {
			clearNodeAssignment(&sandbox)
			return sandbox, true, nil
		}
		return sandbox, false, err
	}
	observed, err := assignmentFromResolved(sandbox, resolved)
	if err != nil {
		return sandbox, false, err
	}
	if resolved.Route.State == nodeledger.StateReleased {
		return observed, true, nil
	}
	switch remoteState {
	case domain.SandboxRunning:
		return observed, resolved.Route.State != nodeledger.StateReady, nil
	case domain.SandboxStandby:
		return observed, resolved.Route.State != nodeledger.StateStandby, nil
	default:
		return observed, false, nil
	}
}

// prepareNodeRoutePause closes admission before the engine is paused. It also
// completes an interrupted attach deterministically so the route state machine
// never needs an unsafe force transition.
func (s *Service) prepareNodeRoutePause(ctx context.Context, sandbox domain.Sandbox) (domain.Sandbox, error) {
	if s.routeAdmin == nil {
		return sandbox, nil
	}
	resolved, err := s.resolveNodeRoute(ctx, sandbox)
	if err != nil {
		return sandbox, err
	}
	sandbox, err = assignmentFromResolved(sandbox, resolved)
	if err != nil {
		return sandbox, err
	}
	switch resolved.Route.State {
	case nodeledger.StateAttaching:
		ready, err := s.routeAdmin.AttachReady(ctx, routeTransition(sandbox, nodeledger.StateAttaching))
		if err != nil {
			return sandbox, fmt.Errorf("finish node route attach before drain: %w", err)
		}
		if err := applyNodeAssignment(&sandbox, ready); err != nil {
			return sandbox, err
		}
		fallthrough
	case nodeledger.StateReady:
		drained, err := s.routeAdmin.Drain(ctx, routeTransition(sandbox, nodeledger.StateReady))
		if err != nil {
			return sandbox, fmt.Errorf("drain node route: %w", err)
		}
		if err := applyNodeAssignment(&sandbox, drained); err != nil {
			return sandbox, err
		}
	case nodeledger.StateDraining, nodeledger.StateStandby:
		return sandbox, nil
	default:
		return sandbox, errors.New("node route is released")
	}
	return sandbox, nil
}

func (s *Service) completeNodeRouteStandby(ctx context.Context, sandbox domain.Sandbox) (domain.Sandbox, error) {
	if s.routeAdmin == nil {
		return sandbox, nil
	}
	resolved, err := s.resolveNodeRoute(ctx, sandbox)
	if err != nil {
		return sandbox, err
	}
	sandbox, err = assignmentFromResolved(sandbox, resolved)
	if err != nil {
		return sandbox, err
	}
	if resolved.Route.State == nodeledger.StateStandby {
		return sandbox, nil
	}
	if resolved.Route.State != nodeledger.StateDraining {
		return sandbox, fmt.Errorf("node route is %s, expected draining", resolved.Route.State)
	}
	standby, err := s.routeAdmin.Standby(ctx, routeTransition(sandbox, nodeledger.StateDraining))
	if err != nil {
		return sandbox, fmt.Errorf("standby node route: %w", err)
	}
	if err := applyNodeAssignment(&sandbox, standby); err != nil {
		return sandbox, err
	}
	return sandbox, nil
}

func (s *Service) resumeNodeRoute(ctx context.Context, sandbox domain.Sandbox) (domain.Sandbox, error) {
	if s.routeAdmin == nil {
		return sandbox, nil
	}
	resolved, err := s.resolveNodeRoute(ctx, sandbox)
	if err != nil {
		return sandbox, err
	}
	sandbox, err = assignmentFromResolved(sandbox, resolved)
	if err != nil {
		return sandbox, err
	}
	switch resolved.Route.State {
	case nodeledger.StateReady:
		return sandbox, nil
	case nodeledger.StateAttaching:
		// An interrupted resume already allocated a fresh generation. Replaying
		// attach-ready is safe and avoids allocating another generation.
	case nodeledger.StateStandby:
		rebound, err := s.routeAdmin.Rebind(ctx, node.RouteRebindRequest{
			RouteID: sandbox.NodeRouteID, ProjectID: sandbox.ProjectID, SandboxID: sandbox.ID,
			Generation: sandbox.NodeGeneration, ExpectedState: nodeledger.StateStandby, EngineID: sandbox.BackendID,
		})
		if err != nil {
			return sandbox, fmt.Errorf("rebind node route: %w", err)
		}
		if err := applyNodeAssignment(&sandbox, rebound); err != nil {
			return sandbox, err
		}
	case nodeledger.StateDraining:
		standby, err := s.routeAdmin.Standby(ctx, routeTransition(sandbox, nodeledger.StateDraining))
		if err != nil {
			return sandbox, fmt.Errorf("finish node drain before resume: %w", err)
		}
		if err := applyNodeAssignment(&sandbox, standby); err != nil {
			return sandbox, err
		}
		return s.resumeNodeRoute(ctx, sandbox)
	default:
		return sandbox, errors.New("node route is released")
	}
	ready, err := s.routeAdmin.AttachReady(ctx, routeTransition(sandbox, nodeledger.StateAttaching))
	if err != nil {
		return sandbox, fmt.Errorf("activate resumed node route: %w", err)
	}
	if err := applyNodeAssignment(&sandbox, ready); err != nil {
		return sandbox, err
	}
	return sandbox, nil
}

func (s *Service) removeNodeRoute(ctx context.Context, sandbox domain.Sandbox) (domain.Sandbox, error) {
	if s.routeAdmin == nil || sandbox.NodeRouteID == "" {
		return sandbox, nil
	}
	resolved, err := s.resolveNodeRoute(ctx, sandbox)
	if err != nil {
		var remoteErr *node.RouteAdminError
		if errors.As(err, &remoteErr) && remoteErr.StatusCode == http.StatusNotFound {
			clearNodeAssignment(&sandbox)
			return sandbox, nil
		}
		return sandbox, err
	}
	sandbox, err = assignmentFromResolved(sandbox, resolved)
	if err != nil {
		return sandbox, err
	}
	state := resolved.Route.State
	if state == nodeledger.StateReady || state == nodeledger.StateAttaching {
		if state == nodeledger.StateAttaching {
			ready, transitionErr := s.routeAdmin.AttachReady(ctx, routeTransition(sandbox, nodeledger.StateAttaching))
			if transitionErr != nil {
				return sandbox, transitionErr
			}
			if err := applyNodeAssignment(&sandbox, ready); err != nil {
				return sandbox, err
			}
		}
		drained, transitionErr := s.routeAdmin.Drain(ctx, routeTransition(sandbox, nodeledger.StateReady))
		if transitionErr != nil {
			return sandbox, transitionErr
		}
		if err := applyNodeAssignment(&sandbox, drained); err != nil {
			return sandbox, err
		}
		state = nodeledger.StateDraining
	}
	if state != nodeledger.StateReleased {
		if state != nodeledger.StateDraining && state != nodeledger.StateStandby {
			return sandbox, fmt.Errorf("cannot release node route from %s", state)
		}
		released, transitionErr := s.routeAdmin.Release(ctx, routeTransition(sandbox, state))
		if transitionErr != nil {
			return sandbox, transitionErr
		}
		if err := applyNodeAssignment(&sandbox, released); err != nil {
			return sandbox, err
		}
	}
	if _, err := s.routeAdmin.Remove(ctx, routeTransition(sandbox, nodeledger.StateReleased)); err != nil {
		return sandbox, fmt.Errorf("remove node route: %w", err)
	}
	clearNodeAssignment(&sandbox)
	return sandbox, nil
}

func (s *Service) resolveNodeRoute(ctx context.Context, sandbox domain.Sandbox) (node.RouteAdminResult, error) {
	if sandbox.NodeRouteID == "" || sandbox.NodeGeneration == 0 || sandbox.NodeID == "" {
		return node.RouteAdminResult{}, errors.New("sandbox has no durable node assignment")
	}
	result, err := s.routeAdmin.Resolve(ctx, sandbox.NodeRouteID)
	if err != nil {
		return node.RouteAdminResult{}, fmt.Errorf("resolve node route: %w", err)
	}
	if result.NodeID != sandbox.NodeID || result.Route.ProjectID != sandbox.ProjectID || result.Route.SandboxID != sandbox.ID || result.Route.Generation < sandbox.NodeGeneration {
		return node.RouteAdminResult{}, errors.New("node route does not match durable sandbox assignment")
	}
	return result, nil
}

func assignmentFromResolved(sandbox domain.Sandbox, result node.RouteAdminResult) (domain.Sandbox, error) {
	if result.Route.ProjectID != sandbox.ProjectID || result.Route.SandboxID != sandbox.ID || result.Route.RouteID != sandbox.NodeRouteID || result.Route.Generation < sandbox.NodeGeneration {
		return sandbox, errors.New("resolved node assignment does not match sandbox")
	}
	if sandbox.NodeID != "" && sandbox.NodeID != result.NodeID {
		return sandbox, errors.New("resolved node identity changed")
	}
	if err := applyNodeAssignment(&sandbox, result); err != nil {
		return sandbox, err
	}
	return sandbox, nil
}

func applyNodeAssignment(sandbox *domain.Sandbox, result node.RouteAdminResult) error {
	if sandbox == nil || result.NodeID == "" || result.Route.RouteID == "" || result.Route.Generation == 0 || result.Route.ProjectID != sandbox.ProjectID || result.Route.SandboxID != sandbox.ID {
		return errors.New("invalid node route assignment")
	}
	sandbox.NodeID = result.NodeID
	sandbox.NodeRouteID = result.Route.RouteID
	sandbox.NodeGeneration = result.Route.Generation
	return nil
}

func clearNodeAssignment(sandbox *domain.Sandbox) {
	sandbox.NodeID = ""
	sandbox.NodeRouteID = ""
	sandbox.NodeGeneration = 0
}

func routeTransition(sandbox domain.Sandbox, expected nodeledger.State) node.RouteTransitionRequest {
	return node.RouteTransitionRequest{
		RouteID: sandbox.NodeRouteID, ProjectID: sandbox.ProjectID, SandboxID: sandbox.ID,
		Generation: sandbox.NodeGeneration, ExpectedState: expected,
	}
}
