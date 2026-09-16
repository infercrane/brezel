package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/infercrane/brezel/internal/access"
	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/backend/e2b"
	"github.com/infercrane/brezel/internal/backend/warm"
	"github.com/infercrane/brezel/internal/connector"
	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/httpapi"
	"github.com/infercrane/brezel/internal/node"
	"github.com/infercrane/brezel/internal/nodeidentity"
	"github.com/infercrane/brezel/internal/receipt"
	"github.com/infercrane/brezel/internal/securefile"
	"github.com/infercrane/brezel/internal/service"
	"github.com/infercrane/brezel/internal/store"
	"github.com/infercrane/brezel/internal/telemetry"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		if err := keygen(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func keygen(args []string) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := flags.String("out", "", "private key output path")
	publicOut := flags.String("public-out", "", "optional raw public key output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("keygen requires -out")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create key file: %w", err)
	}
	if _, err := file.WriteString(base64.StdEncoding.EncodeToString(private) + "\n"); err != nil {
		file.Close()
		return fmt.Errorf("write key: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if *publicOut == "" {
		return nil
	}
	publicFile, err := os.OpenFile(*publicOut, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create public key file: %w", err)
	}
	if _, err := publicFile.WriteString(base64.StdEncoding.EncodeToString(public) + "\n"); err != nil {
		publicFile.Close()
		return fmt.Errorf("write public key: %w", err)
	}
	if err := publicFile.Sync(); err != nil {
		publicFile.Close()
		return err
	}
	return publicFile.Close()
}

func run() error {
	engineToken, err := loadSecretFile(env("BREZEL_ENGINE_TOKEN_FILE", "./.brezel/secrets/engine.token"))
	if err != nil {
		return fmt.Errorf("load microVM engine token: %w", err)
	}
	guestURL := env("BREZEL_GUEST_URL_TEMPLATE", "http://127.0.0.1:5007")
	durableWorkspaces, err := parseBoolEnv("BREZEL_DURABLE_WORKSPACES", false)
	if err != nil {
		return err
	}
	phaseMetrics := telemetry.NewRegistry()
	diagnosticsEnabled, err := parseBoolEnv("BREZEL_BENCHMARK_DIAGNOSTICS", false)
	if err != nil {
		return err
	}
	var commandDiagnostics *telemetry.CommandDiagnosticLogger
	if diagnosticsEnabled {
		commandDiagnostics = telemetry.NewCommandDiagnosticLogger(os.Stderr)
	}
	client, err := e2b.New(
		env("BREZEL_ENGINE_API_URL", "http://127.0.0.1:3000"),
		engineToken,
		nil,
		e2b.WithGuestURLTemplate(guestURL),
		e2b.WithDurableWorkspaces(durableWorkspaces),
		e2b.WithPhaseObserver(telemetry.JoinObservers(phaseMetrics, commandDiagnostics)),
		e2b.WithCommandDiagnostics(commandDiagnostics),
	)
	if err != nil {
		return err
	}
	state, err := store.OpenSQLite(
		env("BREZEL_DATA_DB", "./.brezel/state/brezel.db"),
		env("BREZEL_LEGACY_DATA_FILE", env("BREZEL_DATA_FILE", "./.brezel/state/state.json")),
	)
	if err != nil {
		return err
	}
	defer state.Close()
	var runtimeBackend backend.Backend = client
	warmPool, err := loadWarmPool(runtimeBackend, phaseMetrics)
	if err != nil {
		return err
	}
	if warmPool != nil {
		defer warmPool.Close()
		primeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		err = warmPool.Prime(primeCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("prime single-use warm capacity: %w", err)
		}
		runtimeBackend = warmPool
	}
	private, err := loadPrivateKey(os.Getenv("BREZEL_RECEIPT_PRIVATE_KEY_FILE"))
	if err != nil {
		return err
	}
	signer, err := receipt.NewSigner(private)
	if err != nil {
		return err
	}
	var serviceOptions []service.Option
	var apiOptions []httpapi.Option
	limits, err := loadLimits()
	if err != nil {
		return err
	}
	serviceOptions = append(serviceOptions, service.WithLimits(limits), service.WithPhaseObserver(phaseMetrics))
	nodeOptions, err := loadNodeOptions(commandDiagnostics)
	if err != nil {
		return err
	}
	serviceOptions = append(serviceOptions, nodeOptions...)
	gatewayURL := os.Getenv("BREZEL_CONNECTOR_GATEWAY_URL")
	secretDirectory := os.Getenv("BREZEL_SECRET_FILE_DIR")
	if (gatewayURL == "") != (secretDirectory == "") {
		return errors.New("BREZEL_CONNECTOR_GATEWAY_URL and BREZEL_SECRET_FILE_DIR must be configured together")
	}
	if gatewayURL != "" {
		resolver, resolverErr := connector.NewFileResolver(secretDirectory)
		if resolverErr != nil {
			return resolverErr
		}
		broker, brokerErr := connector.NewBroker(state, private, resolver, gatewayURL, 5*time.Minute)
		if brokerErr != nil {
			return brokerErr
		}
		serviceOptions = append(serviceOptions, service.WithConnectorBroker(broker))
		apiOptions = append(apiOptions, httpapi.WithConnectorHandler(broker))
	}
	svc, err := service.New(state, runtimeBackend, signer, serviceOptions...)
	if err != nil {
		return err
	}
	accessPolicyFile := strings.TrimSpace(os.Getenv("BREZEL_ACCESS_POLICY_FILE"))
	trustedOperatorMode, err := parseBoolEnv("BREZEL_TRUSTED_OPERATOR_MODE", false)
	if err != nil {
		return err
	}
	if accessPolicyFile != "" && trustedOperatorMode {
		return errors.New("BREZEL_ACCESS_POLICY_FILE and BREZEL_TRUSTED_OPERATOR_MODE are mutually exclusive")
	}
	var token string
	if accessPolicyFile != "" {
		policy, policyErr := access.LoadFile(accessPolicyFile)
		if policyErr != nil {
			return fmt.Errorf("load Brezel access policy: %w", policyErr)
		}
		apiOptions = append(apiOptions, httpapi.WithAuthorizer(policy))
	} else {
		if !trustedOperatorMode {
			return errors.New("BREZEL_ACCESS_POLICY_FILE is required; set BREZEL_TRUSTED_OPERATOR_MODE=true only for an isolated development host")
		}
		token, err = loadSecretFile(env("BREZEL_SERVICE_TOKEN_FILE", "./.brezel/secrets/service.token"))
		if err != nil {
			return fmt.Errorf("load Brezel service token: %w", err)
		}
	}
	maxInFlight, err := parsePositiveIntEnv("BREZEL_MAX_IN_FLIGHT_REQUESTS", 512)
	if err != nil {
		return err
	}
	apiOptions = append(apiOptions, httpapi.WithMaxInFlightRequests(maxInFlight), httpapi.WithRequestLogger(log.Default()), httpapi.WithPhaseMetrics(phaseMetrics), httpapi.WithCommandDiagnostics(commandDiagnostics))
	api, err := httpapi.New(svc, token, apiOptions...)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := svc.Reconcile(ctx); err != nil {
		return fmt.Errorf("initial reconciliation: %w", err)
	}
	go reconcileLoop(ctx, svc)
	if warmPool != nil {
		maintenanceCtx, cancelMaintenance := context.WithCancel(ctx)
		maintenanceDone := make(chan struct{})
		go func() {
			defer close(maintenanceDone)
			warmPoolLoop(maintenanceCtx, warmPool)
		}()
		defer func() {
			cancelMaintenance()
			select {
			case <-maintenanceDone:
			case <-time.After(10 * time.Second):
				log.Printf("warm capacity maintenance did not stop before shutdown deadline")
			}
		}()
	}

	httpServer := &http.Server{
		Addr:              env("BREZEL_LISTEN_ADDR", "127.0.0.1:8080"),
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// Command and file responses are bounded by handler-level limits and
		// contexts. A server-wide write deadline would corrupt long streams.
		WriteTimeout:   0,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("Brezel API listening on %s with bundled microVM engine", httpServer.Addr)
		serveErr <- httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func loadWarmPool(inner backend.Backend, observer telemetry.Observer) (*warm.Pool, error) {
	target, err := parseNonNegativeIntEnv("BREZEL_WARM_POOL_SIZE", 0)
	if err != nil || target == 0 {
		return nil, err
	}
	primeConcurrency, err := parsePositiveIntEnv("BREZEL_WARM_POOL_PRIME_CONCURRENCY", target)
	if err != nil {
		return nil, err
	}
	slotTTL, err := parsePositiveIntEnv("BREZEL_WARM_POOL_SLOT_TTL_SECONDS", 4*60*60)
	if err != nil {
		return nil, err
	}
	maxClaimTTL, err := parsePositiveIntEnv("BREZEL_WARM_POOL_MAX_CLAIM_TTL_SECONDS", 60*60)
	if err != nil {
		return nil, err
	}
	allowInternet, err := parseBoolEnv("BREZEL_WARM_POOL_ALLOW_INTERNET", false)
	if err != nil {
		return nil, err
	}
	strict, err := parseBoolEnv("BREZEL_WARM_POOL_STRICT", true)
	if err != nil {
		return nil, err
	}
	configured, err := warm.New(inner, warm.Config{
		Path:             env("BREZEL_WARM_POOL_DB", "./.brezel/state/warm-pool.db"),
		Target:           target,
		TemplateID:       env("BREZEL_WARM_POOL_TEMPLATE", "base"),
		SlotTTL:          time.Duration(slotTTL) * time.Second,
		MaxClaimTTL:      time.Duration(maxClaimTTL) * time.Second,
		PrimeConcurrency: primeConcurrency,
		Network:          domain.NetworkPolicy{AllowInternet: allowInternet},
		Strict:           strict,
		Observer:         observer,
	})
	if err != nil {
		return nil, fmt.Errorf("configure single-use warm capacity: %w", err)
	}
	return configured, nil
}

func loadNodeOptions(commandDiagnostics telemetry.CommandDiagnosticObserver) ([]service.Option, error) {
	dataURL := strings.TrimSpace(os.Getenv("BREZEL_NODE_DATA_URL"))
	controlURL := strings.TrimSpace(os.Getenv("BREZEL_NODE_CONTROL_URL"))
	if dataURL == "" && controlURL == "" {
		return nil, nil
	}
	if dataURL == "" || controlURL == "" {
		return nil, errors.New("BREZEL_NODE_DATA_URL and BREZEL_NODE_CONTROL_URL must be configured together")
	}
	required := func(name string) (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("%s is required when the node relay is enabled", name)
		}
		return value, nil
	}
	apiID, err := required("BREZEL_API_ID")
	if err != nil {
		return nil, err
	}
	nodeID, err := required("BREZEL_NODE_ID")
	if err != nil {
		return nil, err
	}
	serverName, err := required("BREZEL_NODE_SERVER_NAME")
	if err != nil {
		return nil, err
	}
	certificateFile, err := required("BREZEL_API_TLS_CERT_FILE")
	if err != nil {
		return nil, err
	}
	privateKeyFile, err := required("BREZEL_API_TLS_KEY_FILE")
	if err != nil {
		return nil, err
	}
	caFile, err := required("BREZEL_NODE_TLS_CA_FILE")
	if err != nil {
		return nil, err
	}
	capabilityIssuer, err := required("BREZEL_NODE_CAPABILITY_ISSUER")
	if err != nil {
		return nil, err
	}
	capabilityKeyID, err := required("BREZEL_NODE_CAPABILITY_KEY_ID")
	if err != nil {
		return nil, err
	}
	capabilityKeyFile, err := required("BREZEL_NODE_CAPABILITY_PRIVATE_KEY_FILE")
	if err != nil {
		return nil, err
	}
	apiIdentity, err := nodeidentity.NewIdentity(nodeidentity.RoleAPI, apiID)
	if err != nil {
		return nil, fmt.Errorf("configure API node identity: %w", err)
	}
	nodeIdentity, err := nodeidentity.NewIdentity(nodeidentity.RoleNode, nodeID)
	if err != nil {
		return nil, fmt.Errorf("configure execution node identity: %w", err)
	}
	tlsConfig, err := nodeidentity.LoadClientTLSConfig(nodeidentity.ClientOptions{
		Files:         nodeidentity.Files{CertificateFile: certificateFile, PrivateKeyFile: privateKeyFile, CAFile: caFile},
		LocalIdentity: apiIdentity, ServerIdentity: nodeIdentity, ServerName: serverName,
	})
	if err != nil {
		return nil, fmt.Errorf("load API to node mTLS identity: %w", err)
	}
	capabilityPrivate, err := loadPrivateKey(capabilityKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load node capability signing key: %w", err)
	}
	signer, err := node.NewCapabilitySigner(capabilityIssuer, capabilityKeyID, capabilityPrivate)
	if err != nil {
		return nil, fmt.Errorf("configure node capability signer: %w", err)
	}
	audience := env("BREZEL_NODE_CAPABILITY_AUDIENCE", "brezel-node")
	dataPlane, err := node.NewRelayDataPlane(dataURL, tlsConfig, signer, audience, nodeID, node.WithRelayCommandDiagnostics(commandDiagnostics))
	if err != nil {
		return nil, fmt.Errorf("configure node data relay: %w", err)
	}
	administrator, err := node.NewRouteAdminClient(controlURL, tlsConfig, nodeID)
	if err != nil {
		return nil, fmt.Errorf("configure node route administrator: %w", err)
	}
	return []service.Option{service.WithNodeDataPlane(dataPlane), service.WithNodeRouteAdministrator(administrator)}, nil
}

func parseBoolEnv(name string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return value, nil
}

func parsePositiveIntEnv(name string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func parseNonNegativeIntEnv(name string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return value, nil
}

func loadLimits() (service.Limits, error) {
	limits := service.DefaultLimits
	var err error
	limits.MaxActiveSandboxesPerProject, err = parsePositiveIntEnv("BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT", limits.MaxActiveSandboxesPerProject)
	if err != nil {
		return limits, err
	}
	limits.MaxActiveSandboxesTotal, err = parsePositiveIntEnv("BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL", limits.MaxActiveSandboxesTotal)
	if err != nil {
		return limits, err
	}
	limits.MaxWorkspacesPerProject, err = parsePositiveIntEnv("BREZEL_MAX_WORKSPACES_PER_PROJECT", limits.MaxWorkspacesPerProject)
	if err != nil {
		return limits, err
	}
	limits.MaxConcurrentGuestOpsPerProject, err = parsePositiveIntEnv("BREZEL_MAX_CONCURRENT_GUEST_OPS_PER_PROJECT", limits.MaxConcurrentGuestOpsPerProject)
	if err != nil {
		return limits, err
	}
	limits.MaxEnvironmentsPerProject, err = parsePositiveIntEnv("BREZEL_MAX_ENVIRONMENTS_PER_PROJECT", limits.MaxEnvironmentsPerProject)
	if err != nil {
		return limits, err
	}
	limits.MaxConnectorsPerProject, err = parsePositiveIntEnv("BREZEL_MAX_CONNECTORS_PER_PROJECT", limits.MaxConnectorsPerProject)
	return limits, err
}

func loadSecretFile(path string) (string, error) {
	return securefile.ReadText(path, 16<<10)
}

func reconcileLoop(ctx context.Context, svc *service.Service) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := svc.Reconcile(reconcileCtx); err != nil {
				log.Printf("reconciliation failed: %v", err)
			}
			cancel()
		}
	}
}

func warmPoolLoop(ctx context.Context, pool *warm.Pool) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			maintenanceCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			if err := pool.Maintain(maintenanceCtx); err != nil {
				log.Printf("warm capacity maintenance failed: %v", err)
			}
			cancel()
		}
	}
}

func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("BREZEL_RECEIPT_PRIVATE_KEY_FILE is required")
	}
	value, err := loadSecretFile(path)
	if err != nil {
		return nil, fmt.Errorf("read receipt key: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, errors.New("receipt key file must contain base64")
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, errors.New("receipt key file contains an invalid Ed25519 key")
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
