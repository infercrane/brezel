package e2b

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/backend/e2b/wire"
	"github.com/infercrane/brezel/internal/backend/e2b/wire/wireconnect"
	"github.com/infercrane/brezel/internal/telemetry"
)

const envdPort = 49983

const (
	processReplayVersionHeader  = "E2b-Process-Replay-Version"
	processJournalIDHeader      = "E2b-Process-Journal-Id"
	processAfterSequenceHeader  = "E2b-Process-After-Sequence"
	processReplayVersion        = "1"
	processReconnectMaxAttempts = 3
)

var safeSandboxID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,127}$`)
var safeProcessJournalID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

var errGuestProcessOutputIncomplete = errors.New("guest process output continuity was lost")

type guestConnection struct {
	baseURL      *url.URL
	accessToken  string
	trafficToken string
	sandboxID    string
	port         uint16
}

// guestCredentialTTL deliberately matches the bounded guest liveness proof.
// This is an in-process latency optimization, not durable authorization: an
// API restart, lifecycle mutation, or expiry always forces a fresh engine read.
const guestCredentialTTL = guestLiveTTL

type guestCredential struct {
	baseURL      url.URL
	accessToken  string
	trafficToken string
	expiresAt    time.Time
}

func (c *Client) Run(ctx context.Context, sandboxID string, in backend.CommandRequest, emit func(backend.CommandEvent) error) (resultErr error) {
	if emit == nil {
		return errors.New("command event callback is required")
	}
	if len(in.Argv) == 0 || strings.TrimSpace(in.Argv[0]) == "" {
		return errors.New("command argv is required")
	}
	connection, err := c.guestConnectionFor(ctx, sandboxID, telemetry.OperationCommand)
	if err != nil {
		return err
	}
	client := wireconnect.NewProcessClient(c.guestHTTPClient, connection.baseURL.String())
	executionTag, err := guestExecutionTag()
	if err != nil {
		return err
	}
	stdin := false
	request := connect.NewRequest(&wire.StartRequest{
		Process: &wire.ProcessConfig{Cmd: in.Argv[0], Args: in.Argv[1:], Envs: in.Env, Cwd: optionalString(in.Cwd)},
		Tag:     &executionTag,
		Stdin:   &stdin,
	})
	connection.setHeaders(request.Header())
	started := time.Now()
	stream, err := client.Start(ctx, request)
	telemetry.Observe(c.observer, telemetry.OperationCommand, telemetry.PhaseGuestProcessStart, started, err)
	if err != nil {
		return fmt.Errorf("start guest process: %w", err)
	}
	defer stream.Close()
	processStarted := time.Now()
	defer func() {
		telemetry.Observe(c.observer, telemetry.OperationCommand, telemetry.PhaseGuestProcessRun, processStarted, resultErr)
	}()

	ended := false
	var processPID uint32
	var journalID string
	var committedSequence uint64
	firstEventStarted := time.Now()
	firstEventObserved := false
	defer func() {
		if !firstEventObserved && c.observer != nil {
			c.observer.ObservePhase(telemetry.OperationCommand, telemetry.PhaseGuestFirstEvent, telemetry.OutcomeError, time.Since(firstEventStarted))
		}
	}()
	for stream.Receive() {
		if journalID == "" {
			journalID = processJournalID(stream.ResponseHeader())
		}
		message := stream.Msg()
		if message == nil || message.GetEvent() == nil {
			return errors.New("guest process returned an empty event")
		}
		if !firstEventObserved {
			if c.observer != nil {
				c.observer.ObservePhase(telemetry.OperationCommand, telemetry.PhaseGuestFirstEvent, telemetry.OutcomeSuccess, time.Since(firstEventStarted))
			}
			firstEventObserved = true
		}
		event := message.GetEvent()
		var output backend.CommandEvent
		switch {
		case event.GetStart() != nil:
			processPID = event.GetStart().GetPid()
			if processPID == 0 {
				return errors.New("guest process returned an invalid pid")
			}
			output = backend.CommandEvent{Type: backend.CommandStarted, PID: processPID}
		case event.GetData() != nil:
			data := event.GetData()
			switch {
			case len(data.GetStdout()) > 0:
				output = backend.CommandEvent{Type: backend.CommandStdout, Data: append([]byte(nil), data.GetStdout()...)}
			case len(data.GetStderr()) > 0:
				output = backend.CommandEvent{Type: backend.CommandStderr, Data: append([]byte(nil), data.GetStderr()...)}
			case len(data.GetPty()) > 0:
				return errors.New("guest returned PTY data for a non-PTY process")
			default:
				committedSequence++
				continue
			}
		case event.GetEnd() != nil:
			end := event.GetEnd()
			output = backend.CommandEvent{Type: backend.CommandExited, ExitCode: end.GetExitCode(), Exited: end.GetExited(), Status: end.GetStatus(), Error: end.GetError()}
			ended = true
		default:
			continue
		}
		if err := emit(output); err != nil {
			return err
		}
		if event.GetData() != nil || event.GetEnd() != nil {
			committedSequence++
		}
	}
	if journalID == "" {
		journalID = processJournalID(stream.ResponseHeader())
	}
	streamErr := stream.Err()
	if ended {
		return nil
	}
	if streamErr == nil && !ended {
		streamErr = errors.New("guest process stream closed without an exit event")
	}
	if streamErr != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("guest process stream: %w", streamErr)
		}
		// Start deliberately detaches the guest process from the request
		// context. Once the RPC has been accepted, replaying Start could execute
		// a side effect twice. A patched envd advertises a generation-scoped,
		// cursorized output journal. Reconnect to the exact process and consume
		// only the suffix after the last event accepted by emit. Unsupported or
		// incomplete journals fail closed.
		selector := &wire.ProcessSelector{Selector: &wire.ProcessSelector_Tag{Tag: executionTag}}
		identity := executionTag
		if processPID != 0 {
			selector.Selector = &wire.ProcessSelector_Pid{Pid: processPID}
			identity = fmt.Sprint(processPID)
		}
		if journalID != "" {
			recoverErr := c.recoverGuestProcess(ctx, client, connection, selector, &processRecoveryState{
				journalID:         journalID,
				committedSequence: committedSequence,
				pid:               processPID,
				started:           processPID != 0,
			}, emit)
			if recoverErr == nil {
				return nil
			}
			return errors.Join(errGuestProcessOutputIncomplete, fmt.Errorf("guest process stream: %w", streamErr), fmt.Errorf("recover guest process %s: %w", identity, recoverErr))
		}
		if recoverErr := c.awaitGuestProcessTerminal(ctx, client, connection, selector, processPID); recoverErr != nil {
			return errors.Join(fmt.Errorf("guest process stream: %w", streamErr), fmt.Errorf("recover guest process %s: %w", identity, recoverErr))
		}
		return errors.Join(errGuestProcessOutputIncomplete, fmt.Errorf("guest process stream: %w", streamErr))
	}
	return nil
}

type processRecoveryState struct {
	journalID         string
	committedSequence uint64
	pid               uint32
	started           bool
	ended             bool
}

func processJournalID(header http.Header) string {
	if header.Get(processReplayVersionHeader) != processReplayVersion {
		return ""
	}
	id := strings.TrimSpace(header.Get(processJournalIDHeader))
	if !safeProcessJournalID.MatchString(id) {
		return ""
	}
	return id
}

func (c *Client) recoverGuestProcess(ctx context.Context, client wireconnect.ProcessClient, connection guestConnection, selector *wire.ProcessSelector, state *processRecoveryState, emit func(backend.CommandEvent) error) error {
	var lastErr error
	for attempt := 0; attempt < processReconnectMaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		request := connect.NewRequest(&wire.ConnectRequest{Process: selector})
		connection.setHeaders(request.Header())
		request.Header().Set(processJournalIDHeader, state.journalID)
		request.Header().Set(processAfterSequenceHeader, strconv.FormatUint(state.committedSequence, 10))
		stream, err := client.Connect(ctx, request)
		if err != nil {
			lastErr = err
			if !retryableGuestStreamError(err) {
				return err
			}
			continue
		}

		headerChecked := false
		for stream.Receive() {
			if !headerChecked {
				headerChecked = true
				if got := processJournalID(stream.ResponseHeader()); got != state.journalID {
					_ = stream.Close()
					return fmt.Errorf("guest process reconnect journal %q does not match %q", got, state.journalID)
				}
			}
			message := stream.Msg()
			if message == nil || message.GetEvent() == nil {
				_ = stream.Close()
				return errors.New("guest process reconnect returned an empty event")
			}
			if err := acceptRecoveredProcessEvent(message.GetEvent(), state, emit); err != nil {
				_ = stream.Close()
				return err
			}
			if state.ended {
				_ = stream.Close()
				return nil
			}
		}
		if !headerChecked {
			lastErr = stream.Err()
			if lastErr != nil {
				_ = stream.Close()
				if !retryableGuestStreamError(lastErr) {
					return lastErr
				}
				continue
			}
			if got := processJournalID(stream.ResponseHeader()); got != state.journalID {
				_ = stream.Close()
				return fmt.Errorf("guest process reconnect journal %q does not match %q", got, state.journalID)
			}
		}
		lastErr = stream.Err()
		_ = stream.Close()
		if lastErr == nil {
			lastErr = errors.New("guest process reconnect closed without an exit event")
		}
		if !retryableGuestStreamError(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("guest process reconnect exhausted after %d attempts: %w", processReconnectMaxAttempts, lastErr)
}

func acceptRecoveredProcessEvent(event *wire.ProcessEvent, state *processRecoveryState, emit func(backend.CommandEvent) error) error {
	switch {
	case event.GetStart() != nil:
		pid := event.GetStart().GetPid()
		if pid == 0 {
			return errors.New("guest process reconnect returned an invalid pid")
		}
		if state.pid != 0 && pid != state.pid {
			return fmt.Errorf("guest process reconnect returned pid %d, want %d", pid, state.pid)
		}
		if state.started {
			return nil
		}
		if err := emit(backend.CommandEvent{Type: backend.CommandStarted, PID: pid}); err != nil {
			return err
		}
		state.pid = pid
		state.started = true
		return nil
	case event.GetData() != nil:
		if !state.started {
			return errors.New("guest process reconnect returned data before start")
		}
		data := event.GetData()
		var output backend.CommandEvent
		switch {
		case len(data.GetStdout()) > 0:
			output = backend.CommandEvent{Type: backend.CommandStdout, Data: append([]byte(nil), data.GetStdout()...)}
		case len(data.GetStderr()) > 0:
			output = backend.CommandEvent{Type: backend.CommandStderr, Data: append([]byte(nil), data.GetStderr()...)}
		case len(data.GetPty()) > 0:
			return errors.New("guest returned PTY data for a non-PTY process")
		default:
			state.committedSequence++
			return nil
		}
		if err := emit(output); err != nil {
			return err
		}
		state.committedSequence++
		return nil
	case event.GetEnd() != nil:
		if !state.started {
			return errors.New("guest process reconnect returned an exit before start")
		}
		end := event.GetEnd()
		if err := emit(backend.CommandEvent{Type: backend.CommandExited, ExitCode: end.GetExitCode(), Exited: end.GetExited(), Status: end.GetStatus(), Error: end.GetError()}); err != nil {
			return err
		}
		state.committedSequence++
		state.ended = true
		return nil
	default:
		return nil
	}
}

func retryableGuestStreamError(err error) bool {
	if err == nil {
		return false
	}
	switch connect.CodeOf(err) {
	case connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodeOutOfRange, connect.CodeFailedPrecondition, connect.CodeUnimplemented, connect.CodePermissionDenied, connect.CodeUnauthenticated:
		return false
	default:
		return true
	}
}

func (c *Client) awaitGuestProcessTerminal(ctx context.Context, client wireconnect.ProcessClient, connection guestConnection, selector *wire.ProcessSelector, expectedPID uint32) error {
	request := connect.NewRequest(&wire.ConnectRequest{Process: selector})
	connection.setHeaders(request.Header())
	stream, err := client.Connect(ctx, request)
	if err != nil {
		return err
	}
	defer stream.Close()
	started := false
	ended := false
	for stream.Receive() {
		message := stream.Msg()
		if message == nil || message.GetEvent() == nil {
			return errors.New("guest process reconnect returned an empty event")
		}
		event := message.GetEvent()
		switch {
		case event.GetStart() != nil:
			pid := event.GetStart().GetPid()
			if pid == 0 {
				return errors.New("guest process reconnect returned an invalid pid")
			}
			if expectedPID != 0 && pid != expectedPID {
				return fmt.Errorf("guest process reconnect returned pid %d, want %d", pid, expectedPID)
			}
			started = true
		case event.GetEnd() != nil:
			if !started {
				return errors.New("guest process reconnect returned an exit before start")
			}
			ended = true
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	if !started || !ended {
		return errors.New("guest process reconnect closed without a complete terminal state")
	}
	return nil
}

func guestExecutionTag() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("create guest execution identity: %w", err)
	}
	return "brezel-" + hex.EncodeToString(entropy[:]), nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (c *Client) WriteFile(ctx context.Context, sandboxID, path string, source io.Reader) (result backend.FileInfo, resultErr error) {
	if source == nil {
		return backend.FileInfo{}, errors.New("file source is required")
	}
	connection, err := c.guestConnectionFor(ctx, sandboxID, telemetry.OperationFileWrite)
	if err != nil {
		return backend.FileInfo{}, err
	}
	u := *connection.baseURL
	u.Path = "/files"
	query := u.Query()
	query.Set("path", path)
	u.RawQuery = query.Encode()
	counted := &countingReader{reader: source}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), counted)
	if err != nil {
		return backend.FileInfo{}, err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	connection.setHeaders(request.Header)
	started := time.Now()
	defer func() {
		telemetry.Observe(c.observer, telemetry.OperationFileWrite, telemetry.PhaseGuestTransfer, started, resultErr)
	}()
	response, err := c.guestHTTPClient.Do(request)
	if err != nil {
		return backend.FileInfo{}, fmt.Errorf("upload guest file: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 32<<10))
		if response.StatusCode == http.StatusNotFound {
			return backend.FileInfo{}, backend.ErrNotFound
		}
		return backend.FileInfo{}, fmt.Errorf("guest file upload returned %s", response.Status)
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return backend.FileInfo{Path: path, ContentType: "application/octet-stream", Size: counted.size}, nil
}

func (c *Client) ReadFile(ctx context.Context, sandboxID, path string, destination io.Writer) (result backend.FileInfo, resultErr error) {
	connection, err := c.guestConnectionFor(ctx, sandboxID, telemetry.OperationFileRead)
	if err != nil {
		return backend.FileInfo{}, err
	}
	u := *connection.baseURL
	u.Path = "/files"
	query := u.Query()
	query.Set("path", path)
	u.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return backend.FileInfo{}, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	connection.setHeaders(request.Header)
	started := time.Now()
	defer func() {
		telemetry.Observe(c.observer, telemetry.OperationFileRead, telemetry.PhaseGuestTransfer, started, resultErr)
	}()
	response, err := c.guestHTTPClient.Do(request)
	if err != nil {
		return backend.FileInfo{}, fmt.Errorf("download guest file: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 32<<10))
		if response.StatusCode == http.StatusNotFound {
			return backend.FileInfo{}, backend.ErrNotFound
		}
		return backend.FileInfo{}, fmt.Errorf("guest file download returned %s", response.Status)
	}
	size, err := io.Copy(destination, response.Body)
	if err != nil {
		return backend.FileInfo{}, fmt.Errorf("stream guest file: %w", err)
	}
	return backend.FileInfo{Path: path, ContentType: response.Header.Get("Content-Type"), Size: size}, nil
}

func (c *Client) guestConnection(ctx context.Context, sandboxID string) (guestConnection, error) {
	return c.guestConnectionFor(ctx, sandboxID, telemetry.OperationCommand)
}

func (c *Client) guestConnectionFor(ctx context.Context, sandboxID string, operation telemetry.Operation) (result guestConnection, resultErr error) {
	started := time.Now()
	if !safeSandboxID.MatchString(sandboxID) {
		return guestConnection{}, errors.New("invalid backend sandbox id")
	}
	if connection, ok := c.cachedGuestConnection(sandboxID); ok {
		if c.observer != nil {
			c.observer.ObservePhase(operation, telemetry.PhaseGuestConnection, telemetry.OutcomeHit, time.Since(started))
		}
		return connection, nil
	}
	defer func() {
		if c.observer == nil {
			return
		}
		outcome := telemetry.OutcomeMiss
		if resultErr != nil {
			outcome = telemetry.OutcomeError
		}
		c.observer.ObservePhase(operation, telemetry.PhaseGuestConnection, outcome, time.Since(started))
	}()
	var detail sandboxResponse
	if err := c.do(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(sandboxID), nil, &detail, http.StatusOK); err != nil {
		return guestConnection{}, err
	}
	if detail.SandboxID != "" && detail.SandboxID != sandboxID {
		c.forgetGuestState(sandboxID)
		return guestConnection{}, errors.New("backend returned a mismatched sandbox id")
	}
	connection, err := c.guestConnectionFromResponse(sandboxID, detail)
	if err != nil {
		c.forgetGuestState(sandboxID)
		return guestConnection{}, err
	}
	c.rememberGuestConnection(connection)
	return connection, nil
}

func (c *Client) guestConnectionFromResponse(sandboxID string, detail sandboxResponse) (guestConnection, error) {
	if !safeSandboxID.MatchString(sandboxID) {
		return guestConnection{}, errors.New("invalid backend sandbox id")
	}
	if detail.SandboxID != "" && detail.SandboxID != sandboxID {
		return guestConnection{}, errors.New("backend returned a mismatched sandbox id")
	}
	if err := requireSecureGuestAccess(detail.EnvdAccessToken); err != nil {
		return guestConnection{}, err
	}
	baseURL, err := c.resolveGuestURL(sandboxID, detail.Domain, envdPort)
	if err != nil {
		return guestConnection{}, err
	}
	connection := guestConnection{
		baseURL:     baseURL,
		accessToken: strings.TrimSpace(*detail.EnvdAccessToken),
		sandboxID:   sandboxID,
		port:        envdPort,
	}
	if detail.TrafficAccessToken != nil {
		connection.trafficToken = strings.TrimSpace(*detail.TrafficAccessToken)
	}
	return connection, nil
}

func (c *Client) rememberGuestCredential(detail sandboxResponse) {
	connection, err := c.guestConnectionFromResponse(detail.SandboxID, detail)
	if err != nil {
		return
	}
	c.rememberGuestConnection(connection)
}

func (c *Client) rememberGuestConnection(connection guestConnection) {
	if !safeSandboxID.MatchString(connection.sandboxID) || connection.baseURL == nil || strings.TrimSpace(connection.accessToken) == "" {
		return
	}
	now := time.Now()
	c.guestMu.Lock()
	defer c.guestMu.Unlock()
	for id, credential := range c.guestCredentials {
		if !now.Before(credential.expiresAt) {
			delete(c.guestCredentials, id)
		}
	}
	c.guestCredentials[connection.sandboxID] = guestCredential{
		baseURL:      *connection.baseURL,
		accessToken:  connection.accessToken,
		trafficToken: connection.trafficToken,
		expiresAt:    now.Add(guestCredentialTTL),
	}
}

func (c *Client) cachedGuestConnection(sandboxID string) (guestConnection, bool) {
	now := time.Now()
	c.guestMu.Lock()
	defer c.guestMu.Unlock()
	credential, ok := c.guestCredentials[sandboxID]
	if !ok || !now.Before(credential.expiresAt) {
		delete(c.guestCredentials, sandboxID)
		return guestConnection{}, false
	}
	baseURL := credential.baseURL
	return guestConnection{
		baseURL:      &baseURL,
		accessToken:  credential.accessToken,
		trafficToken: credential.trafficToken,
		sandboxID:    sandboxID,
		port:         envdPort,
	}, true
}

func (c *Client) forgetGuestCredential(sandboxID string) {
	c.guestMu.Lock()
	delete(c.guestCredentials, sandboxID)
	c.guestMu.Unlock()
}

func (c *Client) forgetGuestState(sandboxID string) {
	c.forgetLive(sandboxID)
	c.forgetGuestCredential(sandboxID)
	c.forgetPortCredential(sandboxID)
}

func (c *Client) resolveGuestURL(sandboxID string, responseDomain *string, port uint16) (*url.URL, error) {
	if c.guestURLTemplate != "" {
		value := strings.NewReplacer("{sandbox_id}", sandboxID, "{port}", fmt.Sprint(port)).Replace(c.guestURLTemplate)
		u, err := url.Parse(value)
		if err != nil {
			return nil, errors.New("invalid configured guest URL")
		}
		return u, nil
	}
	domain := ""
	if responseDomain != nil {
		domain = strings.TrimSpace(*responseDomain)
	}
	if domain == "" {
		domain = strings.TrimPrefix(c.baseURL.Hostname(), "api.")
	}
	if strings.ContainsAny(domain, "/:@?#[] \r\n\t") || domain == "" {
		return nil, errors.New("backend returned an invalid sandbox domain")
	}
	u, err := url.Parse(fmt.Sprintf("https://%d-%s.%s", port, sandboxID, domain))
	if err != nil || u.Host == "" {
		return nil, errors.New("could not construct guest URL")
	}
	return u, nil
}

type countingReader struct {
	reader io.Reader
	size   int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.size += int64(n)
	return n, err
}

func (c guestConnection) setHeaders(header http.Header) {
	header.Set("X-Access-Token", c.accessToken)
	c.setRoutingHeaders(header)
}

func (c guestConnection) setRoutingHeaders(header http.Header) {
	header.Set("E2b-Sandbox-Id", c.sandboxID)
	header.Set("E2b-Sandbox-Port", fmt.Sprint(c.port))
	if c.trafficToken != "" {
		header.Set("E2B-Traffic-Access-Token", c.trafficToken)
	}
}

var _ backend.GuestRuntime = (*Client)(nil)
