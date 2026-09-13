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
	"strings"
	"syscall"
	"time"

	"github.com/infercrane/sandbox-runtime-lab/internal/backend/e2b"
	"github.com/infercrane/sandbox-runtime-lab/internal/connector"
	"github.com/infercrane/sandbox-runtime-lab/internal/httpapi"
	"github.com/infercrane/sandbox-runtime-lab/internal/receipt"
	"github.com/infercrane/sandbox-runtime-lab/internal/service"
	"github.com/infercrane/sandbox-runtime-lab/internal/store"
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
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("keygen requires -out")
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create key file: %w", err)
	}
	defer file.Close()
	if _, err := file.WriteString(base64.StdEncoding.EncodeToString(private) + "\n"); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	return file.Sync()
}

func run() error {
	backendName := env("RUNTIME_BACKEND", "")
	if backendName != "e2b" {
		return fmt.Errorf("RUNTIME_BACKEND must be explicitly set to e2b; got %q", backendName)
	}
	apiKey := os.Getenv("E2B_API_KEY")
	if apiKey == "" {
		return errors.New("E2B_API_KEY is required")
	}
	client, err := e2b.New(env("E2B_API_URL", "https://api.e2b.app"), apiKey, nil)
	if err != nil {
		return err
	}
	state, err := store.OpenFile(env("RUNTIME_DATA_FILE", "./runtime-state/state.json"))
	if err != nil {
		return err
	}
	private, err := loadPrivateKey(os.Getenv("RUNTIME_RECEIPT_PRIVATE_KEY_FILE"))
	if err != nil {
		return err
	}
	signer, err := receipt.NewSigner(private)
	if err != nil {
		return err
	}
	var serviceOptions []service.Option
	var apiOptions []httpapi.Option
	gatewayURL := os.Getenv("RUNTIME_CONNECTOR_GATEWAY_URL")
	secretDirectory := os.Getenv("RUNTIME_SECRET_FILE_DIR")
	if (gatewayURL == "") != (secretDirectory == "") {
		return errors.New("RUNTIME_CONNECTOR_GATEWAY_URL and RUNTIME_SECRET_FILE_DIR must be configured together")
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
	svc, err := service.New(state, client, signer, serviceOptions...)
	if err != nil {
		return err
	}
	token := os.Getenv("RUNTIME_SERVICE_TOKEN")
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

	httpServer := &http.Server{
		Addr:              env("RUNTIME_LISTEN_ADDR", "127.0.0.1:8080"),
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("runtime API listening on %s with backend %s", httpServer.Addr, backendName)
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

func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("RUNTIME_RECEIPT_PRIVATE_KEY_FILE is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read receipt key: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
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
