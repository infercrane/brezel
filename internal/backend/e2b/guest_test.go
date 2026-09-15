package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/backend/e2b/wire"
	"github.com/infercrane/brezel/internal/backend/e2b/wire/wireconnect"
	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/telemetry"
)

type testProcessService struct {
	t             *testing.T
	omitExit      bool
	seen          backend.CommandRequest
	requiredToken string
}

func (s *testProcessService) Start(_ context.Context, request *connect.Request[wire.StartRequest], stream *connect.ServerStream[wire.StartResponse]) error {
	s.t.Helper()
	if request.Header().Get("X-Access-Token") != s.requiredToken {
		s.t.Errorf("guest access token = %q", request.Header().Get("X-Access-Token"))
	}
	if request.Header().Get("E2b-Sandbox-Id") != "upstream-1" || request.Header().Get("E2b-Sandbox-Port") != "49983" {
		s.t.Errorf("sandbox routing headers missing: %#v", request.Header())
	}
	process := request.Msg.GetProcess()
	s.seen = backend.CommandRequest{Argv: append([]string{process.GetCmd()}, process.GetArgs()...), Cwd: process.GetCwd(), Env: process.GetEnvs()}
	if err := stream.Send(&wire.StartResponse{Event: &wire.ProcessEvent{Event: &wire.ProcessEvent_Start{Start: &wire.ProcessEvent_StartEvent{Pid: 42}}}}); err != nil {
		return err
	}
	if err := stream.Send(&wire.StartResponse{Event: &wire.ProcessEvent{Event: &wire.ProcessEvent_Data{Data: &wire.ProcessEvent_DataEvent{Output: &wire.ProcessEvent_DataEvent_Stdout{Stdout: []byte("hello\n")}}}}}); err != nil {
		return err
	}
	if s.omitExit {
		return nil
	}
	return stream.Send(&wire.StartResponse{Event: &wire.ProcessEvent{Event: &wire.ProcessEvent_End{End: &wire.ProcessEvent_EndEvent{Exited: true, ExitCode: 7, Status: "exited"}}}})
}

func TestGuestRunStreamsRealProtocolEvents(t *testing.T) {
	processService := &testProcessService{t: t, requiredToken: "guest-secret"}
	path, handler := wireconnect.NewProcessHandler(processService)
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("E2B-Traffic-Access-Token") != "traffic-secret" {
			t.Errorf("traffic token = %q", r.Header.Get("E2B-Traffic-Access-Token"))
		}
		handler.ServeHTTP(w, r)
	}))
	defer guest.Close()
	if path != "/process.Process/" {
		t.Fatalf("unexpected process path %q", path)
	}
	api := sandboxDetailServer(t, `{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-secret","trafficAccessToken":"traffic-secret"}`)
	defer api.Close()
	client, err := New(api.URL, "api-secret", api.Client(), WithGuestURLTemplate(guest.URL))
	if err != nil {
		t.Fatal(err)
	}
	var events []backend.CommandEvent
	err = client.Run(context.Background(), "upstream-1", backend.CommandRequest{Argv: []string{"python3", "-V"}, Cwd: "/workspace", Env: map[string]string{"A": "b"}}, func(event backend.CommandEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []backend.CommandEventType{backend.CommandStarted, backend.CommandStdout, backend.CommandExited}
	var gotTypes []backend.CommandEventType
	for _, event := range events {
		gotTypes = append(gotTypes, event.Type)
	}
	if !reflect.DeepEqual(gotTypes, wantTypes) || events[2].ExitCode != 7 {
		t.Fatalf("events = %#v", events)
	}
	if !reflect.DeepEqual(processService.seen.Argv, []string{"python3", "-V"}) || processService.seen.Cwd != "/workspace" {
		t.Fatalf("process request = %#v", processService.seen)
	}
}

func TestGuestRunExportsOnlyFixedPhaseDimensions(t *testing.T) {
	processService := &testProcessService{t: t, requiredToken: "guest-secret"}
	_, handler := wireconnect.NewProcessHandler(processService)
	guest := httptest.NewServer(handler)
	defer guest.Close()
	api := sandboxDetailServer(t, `{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-secret"}`)
	defer api.Close()
	registry := telemetry.NewRegistry()
	client, err := New(api.URL, "api-secret", api.Client(), WithGuestURLTemplate(guest.URL), WithPhaseObserver(registry))
	if err != nil {
		t.Fatal(err)
	}
	secretCommand := "customer-command-must-not-be-a-label"
	if err := client.Run(context.Background(), "upstream-1", backend.CommandRequest{Argv: []string{secretCommand}}, func(backend.CommandEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := registry.WritePrometheus(&output); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"guest_connection", "guest_process_start", "guest_first_event", "guest_process_run"} {
		if !strings.Contains(output.String(), `operation="command",phase="`+phase+`"`) {
			t.Fatalf("guest metrics omitted %s: %s", phase, output.String())
		}
	}
	if strings.Contains(output.String(), secretCommand) || strings.Contains(output.String(), "upstream-1") || strings.Contains(output.String(), "guest-secret") {
		t.Fatalf("guest identity or content reached metrics: %s", output.String())
	}
}

func TestGuestRunRejectsStreamWithoutExit(t *testing.T) {
	processService := &testProcessService{t: t, omitExit: true, requiredToken: "guest-secret"}
	_, handler := wireconnect.NewProcessHandler(processService)
	guest := httptest.NewServer(handler)
	defer guest.Close()
	api := sandboxDetailServer(t, `{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-secret"}`)
	defer api.Close()
	client, _ := New(api.URL, "api-secret", api.Client(), WithGuestURLTemplate(guest.URL))
	err := client.Run(context.Background(), "upstream-1", backend.CommandRequest{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil })
	if err == nil {
		t.Fatal("stream without exit event was accepted")
	}
}

func TestGuestFilesUseSecuredEnvdEndpoint(t *testing.T) {
	var stored []byte
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") != "guest-secret" {
			t.Fatalf("guest request was not authenticated")
		}
		if r.URL.Path != "/files" || r.URL.Query().Get("path") != "/workspace/state.json" {
			t.Fatalf("guest file request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		switch r.Method {
		case http.MethodPost:
			if r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Fatalf("upload content type = %q", r.Header.Get("Content-Type"))
			}
			stored, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"path":"/workspace/state.json"}]`))
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(stored)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer guest.Close()
	api := sandboxDetailServer(t, `{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-secret"}`)
	defer api.Close()
	client, _ := New(api.URL, "api-secret", api.Client(), WithGuestURLTemplate(guest.URL))
	info, err := client.WriteFile(context.Background(), "upstream-1", "/workspace/state.json", strings.NewReader(`{"step":1}`))
	if err != nil || info.Path != "/workspace/state.json" || info.Size != int64(len(`{"step":1}`)) {
		t.Fatalf("WriteFile() = %#v, %v", info, err)
	}
	var output []byte
	buffer := sliceWriter{value: &output}
	info, err = client.ReadFile(context.Background(), "upstream-1", "/workspace/state.json", buffer)
	if err != nil || string(output) != `{"step":1}` || info.Size != int64(len(output)) {
		t.Fatalf("ReadFile() = %#v %q, %v", info, output, err)
	}
}

func TestApplicationPortUsesTrafficCredentialWithoutGuestCredential(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("E2B-Traffic-Access-Token") != "traffic-secret" || r.Header.Get("E2b-Sandbox-Port") != "8080" {
			t.Fatalf("port routing headers = %#v", r.Header)
		}
		if r.Header.Get("X-Access-Token") != "" {
			t.Fatal("guest-management credential was sent to application port")
		}
		if r.Header.Get("Authorization") != "Bearer application-token" {
			t.Fatal("application authorization header was not preserved")
		}
		if r.URL.RequestURI() != "/health?ready=1" {
			t.Fatalf("application path = %q", r.URL.RequestURI())
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	}))
	defer guest.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sandboxes/upstream-1/connect" || r.Header.Get("X-API-Key") != "api-secret" {
			t.Fatalf("connect request = %s %s headers=%#v", r.Method, r.URL.Path, r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["timeout"] != float64(60) {
			t.Fatalf("connect body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-secret","trafficAccessToken":"traffic-secret"}`))
	}))
	defer api.Close()
	client, _ := New(api.URL, "api-secret", api.Client(), WithGuestURLTemplate(guest.URL))
	request := httptest.NewRequest(http.MethodGet, "http://runtime.invalid/health?ready=1", nil)
	request.Header.Set("Authorization", "Bearer application-token")
	response, err := client.RoundTripPort(context.Background(), "upstream-1", 8080, request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("port response = %d %q", response.StatusCode, body)
	}
}

func TestApplicationPortReusesShortLivedCredentialFromCreate(t *testing.T) {
	guest := newHealthyGuestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("E2B-Traffic-Access-Token") != "traffic-from-create" {
			t.Fatalf("traffic credential = %q", r.Header.Get("E2B-Traffic-Access-Token"))
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer guest.Close()
	apiCalls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		if r.Method != http.MethodPost || r.URL.Path != "/sandboxes" {
			t.Fatalf("unexpected engine request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-secret","trafficAccessToken":"traffic-from-create"}`))
	}))
	defer api.Close()
	client, _ := New(api.URL, "api-secret", api.Client(), WithGuestURLTemplate(guest.URL))
	if _, err := client.Create(context.Background(), backend.CreateRequest{
		LocalSandboxID: "local-1", ProjectID: "project-a", TemplateID: "base",
		Lifecycle: domain.Lifecycle{ExpiresAfterSeconds: 600},
	}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://runtime.invalid/", nil)
	response, err := client.RoundTripPort(context.Background(), "upstream-1", 8080, request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if apiCalls != 1 {
		t.Fatalf("engine API calls = %d, want create only", apiCalls)
	}
}

func TestGuestRunReusesShortLivedCredentialFromCreate(t *testing.T) {
	processService := &testProcessService{t: t, requiredToken: "guest-from-create"}
	_, handler := wireconnect.NewProcessHandler(processService)
	guest := newHealthyGuestServer(t, handler)
	defer guest.Close()
	apiCalls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		if r.Method != http.MethodPost || r.URL.Path != "/sandboxes" {
			t.Fatalf("unexpected engine request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-from-create"}`))
	}))
	defer api.Close()
	client, err := New(api.URL, "api-secret", api.Client(), WithGuestURLTemplate(guest.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Create(context.Background(), backend.CreateRequest{
		LocalSandboxID: "local-1", ProjectID: "project-a", TemplateID: "base",
		Lifecycle: domain.Lifecycle{ExpiresAfterSeconds: 600},
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.Run(context.Background(), "upstream-1", backend.CommandRequest{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if apiCalls != 1 {
		t.Fatalf("engine API calls = %d, want create only", apiCalls)
	}
}

func TestGuestRunReusesShortLivedCredentialFromResume(t *testing.T) {
	processService := &testProcessService{t: t, requiredToken: "guest-from-resume"}
	_, handler := wireconnect.NewProcessHandler(processService)
	guest := newHealthyGuestServer(t, handler)
	defer guest.Close()
	apiCalls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		if r.Method != http.MethodPost || r.URL.Path != "/sandboxes/upstream-1/resume" {
			t.Fatalf("unexpected engine request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-from-resume"}`))
	}))
	defer api.Close()
	client, err := New(api.URL, "api-secret", api.Client(), WithGuestURLTemplate(guest.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resume(context.Background(), "upstream-1", domain.CheckpointFullState, 600); err != nil {
		t.Fatal(err)
	}
	if err := client.Run(context.Background(), "upstream-1", backend.CommandRequest{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if apiCalls != 1 {
		t.Fatalf("engine API calls = %d, want resume only", apiCalls)
	}
}

func TestGuestCredentialExpiryFallsBackToEngineDetail(t *testing.T) {
	processService := &testProcessService{t: t, requiredToken: "fresh-guest-secret"}
	_, handler := wireconnect.NewProcessHandler(processService)
	guest := newHealthyGuestServer(t, handler)
	defer guest.Close()
	detailCalls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"old-guest-secret"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/sandboxes/upstream-1":
			detailCalls++
			_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"fresh-guest-secret"}`))
		default:
			t.Fatalf("unexpected engine request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()
	client, err := New(api.URL, "api-secret", api.Client(), WithGuestURLTemplate(guest.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Create(context.Background(), backend.CreateRequest{
		LocalSandboxID: "local-1", ProjectID: "project-a", TemplateID: "base",
		Lifecycle: domain.Lifecycle{ExpiresAfterSeconds: 600},
	}); err != nil {
		t.Fatal(err)
	}
	client.guestMu.Lock()
	credential := client.guestCredentials["upstream-1"]
	credential.expiresAt = time.Now().Add(-time.Second)
	client.guestCredentials["upstream-1"] = credential
	client.guestMu.Unlock()
	if err := client.Run(context.Background(), "upstream-1", backend.CommandRequest{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if detailCalls != 1 {
		t.Fatalf("detail calls = %d, want 1 after credential expiry", detailCalls)
	}
}

func TestGuestCredentialMismatchInvalidatesCachedState(t *testing.T) {
	guest := newHealthyGuestServer(t, nil)
	defer guest.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"old-guest-secret"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/sandboxes/upstream-1":
			_, _ = w.Write([]byte(`{"sandboxID":"different-sandbox","state":"running","envdAccessToken":"wrong-guest-secret"}`))
		default:
			t.Fatalf("unexpected engine request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()
	client, err := New(api.URL, "api-secret", api.Client(), WithGuestURLTemplate(guest.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Create(context.Background(), backend.CreateRequest{
		LocalSandboxID: "local-1", ProjectID: "project-a", TemplateID: "base",
		Lifecycle: domain.Lifecycle{ExpiresAfterSeconds: 600},
	}); err != nil {
		t.Fatal(err)
	}
	client.guestMu.Lock()
	credential := client.guestCredentials["upstream-1"]
	credential.expiresAt = time.Now().Add(-time.Second)
	client.guestCredentials["upstream-1"] = credential
	client.guestMu.Unlock()
	err = client.Run(context.Background(), "upstream-1", backend.CommandRequest{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "mismatched sandbox id") {
		t.Fatalf("Run() error = %v, want mismatched sandbox id", err)
	}
	if _, ok := client.cachedGuestConnection("upstream-1"); ok {
		t.Fatal("mismatched engine response left a guest credential cached")
	}
	if client.recentlyLive("upstream-1") {
		t.Fatal("mismatched engine response left a liveness proof cached")
	}
}

func TestLifecycleMutationInvalidatesGuestCredentialBeforeEngineResult(t *testing.T) {
	for _, test := range []struct {
		name       string
		method     string
		path       string
		statusCode int
		mutate     func(*Client) error
	}{
		{
			name: "pause success", method: http.MethodPost, path: "/sandboxes/upstream-1/pause", statusCode: http.StatusNoContent,
			mutate: func(client *Client) error {
				return client.Pause(context.Background(), "upstream-1", domain.CheckpointFullState)
			},
		},
		{
			name: "pause ambiguous failure", method: http.MethodPost, path: "/sandboxes/upstream-1/pause", statusCode: http.StatusBadGateway,
			mutate: func(client *Client) error {
				return client.Pause(context.Background(), "upstream-1", domain.CheckpointFullState)
			},
		},
		{
			name: "delete success", method: http.MethodDelete, path: "/sandboxes/upstream-1", statusCode: http.StatusNoContent,
			mutate: func(client *Client) error { return client.Delete(context.Background(), "upstream-1") },
		},
		{
			name: "delete ambiguous failure", method: http.MethodDelete, path: "/sandboxes/upstream-1", statusCode: http.StatusBadGateway,
			mutate: func(client *Client) error { return client.Delete(context.Background(), "upstream-1") },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			guest := newHealthyGuestServer(t, nil)
			defer guest.Close()
			mutationStarted := make(chan struct{})
			releaseMutation := make(chan struct{})
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-secret","trafficAccessToken":"traffic-secret"}`))
				case r.Method == test.method && r.URL.Path == test.path:
					close(mutationStarted)
					<-releaseMutation
					w.WriteHeader(test.statusCode)
					if test.statusCode != http.StatusNoContent {
						_, _ = w.Write([]byte(`{"error":"uncertain engine state"}`))
					}
				case r.Method == http.MethodGet && r.URL.Path == "/sandboxes/upstream-1":
					_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"racing-guest-secret"}`))
				default:
					t.Fatalf("unexpected engine request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer api.Close()
			client, err := New(api.URL, "api-secret", api.Client(), WithGuestURLTemplate(guest.URL))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Create(context.Background(), backend.CreateRequest{
				LocalSandboxID: "local-1", ProjectID: "project-a", TemplateID: "base",
				Lifecycle: domain.Lifecycle{ExpiresAfterSeconds: 600},
			}); err != nil {
				t.Fatal(err)
			}
			if _, ok := client.cachedGuestConnection("upstream-1"); !ok {
				t.Fatal("create did not retain a guest credential")
			}
			mutationResult := make(chan error, 1)
			go func() {
				mutationResult <- test.mutate(client)
			}()
			<-mutationStarted
			_, guestCached := client.cachedGuestConnection("upstream-1")
			liveCached := client.recentlyLive("upstream-1")
			_, portCached := client.cachedPortCredential("upstream-1")
			if _, err := client.guestConnection(context.Background(), "upstream-1"); err != nil {
				close(releaseMutation)
				<-mutationResult
				t.Fatalf("concurrent guest credential refresh failed: %v", err)
			}
			_, refreshedDuringMutation := client.cachedGuestConnection("upstream-1")
			close(releaseMutation)
			err = <-mutationResult
			if guestCached || liveCached || portCached {
				t.Fatalf("credential state remained reachable in flight: guest=%t live=%t port=%t", guestCached, liveCached, portCached)
			}
			if !refreshedDuringMutation {
				t.Fatal("test did not reproduce an in-flight credential refresh")
			}
			if test.statusCode == http.StatusNoContent && err != nil {
				t.Fatalf("mutation failed: %v", err)
			}
			if test.statusCode != http.StatusNoContent && err == nil {
				t.Fatal("ambiguous engine failure was accepted")
			}
			if _, ok := client.cachedGuestConnection("upstream-1"); ok {
				t.Fatal("lifecycle mutation left a guest credential cached")
			}
			if client.recentlyLive("upstream-1") {
				t.Fatal("lifecycle mutation left a liveness proof cached")
			}
			if _, ok := client.cachedPortCredential("upstream-1"); ok {
				t.Fatal("lifecycle mutation left a port credential cached")
			}
		})
	}
}

func sandboxDetailServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandboxes/upstream-1" || r.Header.Get("X-API-Key") != "api-secret" {
			t.Fatalf("detail request = %s headers=%#v", r.URL.Path, r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

type sliceWriter struct{ value *[]byte }

func (w sliceWriter) Write(data []byte) (int, error) {
	*w.value = append(*w.value, data...)
	return len(data), nil
}
