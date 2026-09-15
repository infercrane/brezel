package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/infercrane/brezel/internal/backend/e2b"
	"github.com/infercrane/brezel/internal/node"
	"github.com/infercrane/brezel/internal/nodeidentity"
	"github.com/infercrane/brezel/internal/nodeledger"
	"github.com/infercrane/brezel/internal/securefile"
	"github.com/infercrane/brezel/internal/telemetry"
)

const maxNodeKeyFileBytes = 16 << 10

type nodeConfig struct {
	listenAddress      string
	controlAddress     string
	nodeID             string
	apiID              string
	audience           string
	capabilityKeysFile string
	tlsFiles           nodeidentity.Files
	ledgerFile         string
	replayCapacity     int
	maxInFlight        int
	shutdownTimeout    time.Duration
	engineURL          string
	engineTokenFile    string
	guestURLTemplate   string
	durableWorkspaces  bool
}

type capabilityKeyPolicy struct {
	Version int                         `json:"version"`
	Issuer  string                      `json:"issuer"`
	Keys    []capabilityVerificationKey `json:"keys"`
}

type capabilityVerificationKey struct {
	ID              string `json:"id"`
	PublicKeyBase64 string `json:"public_key_base64"`
}

// errorPhaseObserver emits only closed-enum operation and phase names. It is
// deliberately unable to receive sandbox identifiers, commands, paths, or
// customer output, while still making node-local stream failures diagnosable.
type errorPhaseObserver struct {
	logger *log.Logger
}

func (o errorPhaseObserver) ObservePhase(operation telemetry.Operation, phase telemetry.Phase, outcome telemetry.Outcome, duration time.Duration) {
	if o.logger == nil || outcome != telemetry.OutcomeError {
		return
	}
	o.logger.Printf("Brezel node backend phase failed operation=%s phase=%s duration_ms=%d", operation, phase, duration.Milliseconds())
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() (resultErr error) {
	config, err := loadNodeConfig()
	if err != nil {
		return err
	}
	tlsConfig, err := loadNodeTLSConfig(config)
	if err != nil {
		return err
	}
	issuer, publicKeys, err := loadCapabilityKeys(config.capabilityKeysFile)
	if err != nil {
		return err
	}
	verifier, err := node.NewCapabilityVerifier(issuer, publicKeys)
	if err != nil {
		return fmt.Errorf("configure capability verifier: %w", err)
	}
	engineToken, err := securefile.ReadText(config.engineTokenFile, maxNodeKeyFileBytes)
	if err != nil {
		return fmt.Errorf("load microVM engine token: %w", err)
	}
	engineTransport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	defer engineTransport.CloseIdleConnections()
	engine, err := e2b.New(
		config.engineURL,
		engineToken,
		&http.Client{Transport: engineTransport, Timeout: 30 * time.Second},
		e2b.WithGuestURLTemplate(config.guestURLTemplate),
		e2b.WithDurableWorkspaces(config.durableWorkspaces),
		e2b.WithPhaseObserver(errorPhaseObserver{logger: log.Default()}),
	)
	if err != nil {
		return fmt.Errorf("configure microVM engine: %w", err)
	}
	capabilities := engine.Capabilities()
	if !capabilities.HostileCodeIsolation || !capabilities.DenyByDefaultEgress || !capabilities.CommandStreaming || !capabilities.FileReadWrite || !capabilities.AuthenticatedPorts {
		return errors.New("microVM engine does not provide the required isolation, network, command, file, and port capabilities")
	}
	readyContext, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
	readyErr := engine.Ready(readyContext)
	cancelReady()
	if readyErr != nil {
		return fmt.Errorf("microVM engine readiness: %w", readyErr)
	}
	replay, err := node.NewReplayCache(config.replayCapacity)
	if err != nil {
		return fmt.Errorf("configure capability replay cache: %w", err)
	}
	ledger, err := nodeledger.Open(config.ledgerFile)
	if err != nil {
		return fmt.Errorf("open node generation ledger: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, ledger.Close()) }()
	relay, err := node.NewRelayServer(node.RelayServerConfig{
		NodeID: config.nodeID, Audience: config.audience, Ledger: ledger,
		Verifier: verifier, Replay: replay, Engine: engine, MaxInFlight: config.maxInFlight,
	})
	if err != nil {
		return fmt.Errorf("configure node relay: %w", err)
	}
	admin, err := node.NewRouteAdminHandler(node.RouteAdminHandlerConfig{NodeID: config.nodeID, Ledger: ledger})
	if err != nil {
		return fmt.Errorf("configure node route administration: %w", err)
	}
	dataServer, err := nodeidentity.NewDataHTTPServer(config.listenAddress, relay, tlsConfig)
	if err != nil {
		return fmt.Errorf("configure node HTTP server: %w", err)
	}
	controlServer, err := nodeidentity.NewControlHTTPServer(config.controlAddress, admin, tlsConfig)
	if err != nil {
		return fmt.Errorf("configure node control HTTP server: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	handlerContext, cancelHandlers := context.WithCancel(context.Background())
	defer cancelHandlers()
	dataServer.BaseContext = func(net.Listener) context.Context { return handlerContext }
	controlServer.BaseContext = func(net.Listener) context.Context { return handlerContext }
	type serveResult struct {
		name string
		err  error
	}
	serveErr := make(chan serveResult, 2)
	go func() {
		log.Printf("Brezel node %s data listener on %s with mandatory mTLS", config.nodeID, dataServer.Addr)
		serveErr <- serveResult{name: "data", err: dataServer.ListenAndServeTLS("", "")}
	}()
	go func() {
		log.Printf("Brezel node %s control listener on %s with mandatory mTLS", config.nodeID, controlServer.Addr)
		serveErr <- serveResult{name: "control", err: controlServer.ListenAndServeTLS("", "")}
	}()

	var first serveResult
	served := 0
	select {
	case first = <-serveErr:
		served = 1
	case <-ctx.Done():
	}
	relay.BeginDrain()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), config.shutdownTimeout)
	shutdownErr := errors.Join(controlServer.Shutdown(shutdownCtx), dataServer.Shutdown(shutdownCtx))
	cancel()
	if shutdownErr != nil {
		cancelHandlers()
		shutdownErr = errors.Join(shutdownErr, controlServer.Close(), dataServer.Close())
	}
	serveErrors := []error{shutdownErr}
	if served == 1 && !errors.Is(first.err, http.ErrServerClosed) {
		serveErrors = append(serveErrors, fmt.Errorf("%s listener: %w", first.name, first.err))
	}
	for served < 2 {
		result := <-serveErr
		served++
		if !errors.Is(result.err, http.ErrServerClosed) {
			serveErrors = append(serveErrors, fmt.Errorf("%s listener: %w", result.name, result.err))
		}
	}
	return errors.Join(serveErrors...)
}

func loadNodeConfig() (nodeConfig, error) {
	var config nodeConfig
	var err error
	config.listenAddress = env("BREZEL_NODE_LISTEN_ADDR", "127.0.0.1:8443")
	config.controlAddress = env("BREZEL_NODE_CONTROL_LISTEN_ADDR", "127.0.0.1:8444")
	if config.listenAddress == config.controlAddress {
		return config, errors.New("BREZEL_NODE_LISTEN_ADDR and BREZEL_NODE_CONTROL_LISTEN_ADDR must differ")
	}
	config.nodeID, err = requiredEnv("BREZEL_NODE_ID")
	if err != nil {
		return config, err
	}
	config.apiID, err = requiredEnv("BREZEL_NODE_API_ID")
	if err != nil {
		return config, err
	}
	config.capabilityKeysFile, err = requiredAbsolutePathEnv("BREZEL_NODE_CAPABILITY_KEYS_FILE")
	if err != nil {
		return config, err
	}
	config.tlsFiles.CertificateFile, err = requiredAbsolutePathEnv("BREZEL_NODE_TLS_CERT_FILE")
	if err != nil {
		return config, err
	}
	config.tlsFiles.PrivateKeyFile, err = requiredAbsolutePathEnv("BREZEL_NODE_TLS_KEY_FILE")
	if err != nil {
		return config, err
	}
	config.tlsFiles.CAFile, err = requiredAbsolutePathEnv("BREZEL_NODE_TLS_CA_FILE")
	if err != nil {
		return config, err
	}
	config.audience = env("BREZEL_NODE_CAPABILITY_AUDIENCE", "brezel-node")
	config.ledgerFile, err = requiredAbsolutePathEnv("BREZEL_NODE_LEDGER_FILE")
	if err != nil {
		return config, err
	}
	config.engineURL = env("BREZEL_ENGINE_API_URL", "http://127.0.0.1:3000")
	config.engineTokenFile, err = requiredAbsolutePathEnv("BREZEL_ENGINE_TOKEN_FILE")
	if err != nil {
		return config, err
	}
	config.guestURLTemplate = env("BREZEL_GUEST_URL_TEMPLATE", "http://127.0.0.1:5007")
	config.replayCapacity, err = positiveIntEnv("BREZEL_NODE_REPLAY_CAPACITY", 65_536)
	if err != nil {
		return config, err
	}
	config.maxInFlight, err = boundedIntEnv("BREZEL_NODE_MAX_IN_FLIGHT", 64, 1, 1_024)
	if err != nil {
		return config, err
	}
	config.shutdownTimeout, err = durationEnv("BREZEL_NODE_DRAIN_TIMEOUT", 5*time.Minute, time.Second, time.Hour)
	if err != nil {
		return config, err
	}
	config.durableWorkspaces, err = boolEnv("BREZEL_DURABLE_WORKSPACES", false)
	if err != nil {
		return config, err
	}
	return config, nil
}

func loadNodeTLSConfig(config nodeConfig) (*tls.Config, error) {
	nodeIdentity, err := nodeidentity.NewIdentity(nodeidentity.RoleNode, config.nodeID)
	if err != nil {
		return nil, fmt.Errorf("configure node identity: %w", err)
	}
	apiIdentity, err := nodeidentity.NewIdentity(nodeidentity.RoleAPI, config.apiID)
	if err != nil {
		return nil, fmt.Errorf("configure API identity: %w", err)
	}
	tlsConfig, err := nodeidentity.LoadServerTLSConfig(nodeidentity.ServerOptions{
		Files: config.tlsFiles, LocalIdentity: nodeIdentity, ClientIdentity: apiIdentity,
	})
	if err != nil {
		return nil, fmt.Errorf("load node mTLS identity: %w", err)
	}
	return tlsConfig, nil
}

func loadCapabilityKeys(path string) (string, map[string]ed25519.PublicKey, error) {
	data, err := securefile.Read(path, maxNodeKeyFileBytes)
	if err != nil {
		return "", nil, fmt.Errorf("read capability key policy: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var policy capabilityKeyPolicy
	if err := decoder.Decode(&policy); err != nil {
		return "", nil, fmt.Errorf("decode capability key policy: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return "", nil, err
	}
	policy.Issuer = strings.TrimSpace(policy.Issuer)
	if policy.Version != 1 || policy.Issuer == "" || len(policy.Keys) < 1 || len(policy.Keys) > 16 {
		return "", nil, errors.New("capability key policy requires version 1, one issuer, and 1 to 16 keys")
	}
	keys := make(map[string]ed25519.PublicKey, len(policy.Keys))
	for _, entry := range policy.Keys {
		entry.ID = strings.TrimSpace(entry.ID)
		value := strings.TrimSpace(entry.PublicKeyBase64)
		if entry.ID == "" || value == "" || strings.ContainsAny(entry.ID+value, "\r\n") {
			return "", nil, errors.New("capability key policy contains an invalid key entry")
		}
		if _, exists := keys[entry.ID]; exists {
			return "", nil, fmt.Errorf("capability key policy contains duplicate key id %q", entry.ID)
		}
		decoded, decodeErr := base64.StdEncoding.Strict().DecodeString(value)
		if decodeErr != nil || len(decoded) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(decoded) != value {
			return "", nil, fmt.Errorf("capability key %q must be one canonical base64 Ed25519 public key", entry.ID)
		}
		keys[entry.ID] = ed25519.PublicKey(append([]byte(nil), decoded...))
	}
	if _, err := node.NewCapabilityVerifier(policy.Issuer, keys); err != nil {
		return "", nil, fmt.Errorf("validate capability key policy: %w", err)
	}
	return policy.Issuer, keys, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("capability key policy must contain exactly one JSON value")
		}
		return fmt.Errorf("decode capability key policy trailing data: %w", err)
	}
	return nil
}

func requiredEnv(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func requiredAbsolutePathEnv(name string) (string, error) {
	value, err := requiredEnv(name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return "", fmt.Errorf("%s must be a clean absolute path", name)
	}
	return value, nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func positiveIntEnv(name string, fallback int) (int, error) {
	return boundedIntEnv(name, fallback, 1, 1_000_000)
}

func boundedIntEnv(name string, fallback, minimum, maximum int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

func boolEnv(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return value, nil
}

func durationEnv(name string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be a duration between %s and %s", name, minimum, maximum)
	}
	return value, nil
}
