package nodeidentity

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"
)

const (
	maxControlHeaderBytes = 1 << 20
	maxDataReadTime       = 5 * time.Minute
)

// NewControlHTTPClient creates the bounded, proxy-free HTTP client used by the
// API for mTLS node-control calls. Redirects are returned to the caller and are
// never followed with the controller certificate or authorization headers.
func NewControlHTTPClient(config *tls.Config) (*http.Client, error) {
	if err := validateControlClientTLS(config); err != nil {
		return nil, err
	}
	return &http.Client{
		Transport:     newHTTPTransport(config),
		Timeout:       30 * time.Second,
		CheckRedirect: rejectRedirect,
	}, nil
}

// NewDataHTTPClient creates the proxy-free mTLS client for bounded relay data
// operations. The caller's operation context owns the total deadline so a
// streamed command is not truncated by an unrelated client-wide timeout.
func NewDataHTTPClient(config *tls.Config) (*http.Client, error) {
	if err := validateControlClientTLS(config); err != nil {
		return nil, err
	}
	return &http.Client{
		Transport:     newHTTPTransport(config),
		CheckRedirect: rejectRedirect,
	}, nil
}

func newHTTPTransport(config *tls.Config) *http.Transport {
	return &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           32,
		MaxIdleConnsPerHost:    16,
		MaxConnsPerHost:        64,
		IdleConnTimeout:        60 * time.Second,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxControlHeaderBytes,
		TLSClientConfig:        config.Clone(),
	}
}

func rejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// NewControlHTTPServer applies conservative limits to the node's mTLS control
// endpoint. Streaming guest data belongs on a separate server with its own
// explicit limits.
func NewControlHTTPServer(address string, handler http.Handler, config *tls.Config) (*http.Server, error) {
	if address == "" {
		return nil, errors.New("listen address is required")
	}
	if handler == nil {
		return nil, errors.New("HTTP handler is required")
	}
	if config == nil {
		return nil, errors.New("TLS config is required")
	}
	if err := validateControlServerTLS(config); err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		TLSConfig:         config.Clone(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxControlHeaderBytes,
	}, nil
}

// NewDataHTTPServer applies the same mTLS identity boundary to relay traffic,
// while allowing bounded long-running command streams. ReadTimeout limits slow
// request bodies; each relay handler supplies the stricter operation deadline.
func NewDataHTTPServer(address string, handler http.Handler, config *tls.Config) (*http.Server, error) {
	if address == "" {
		return nil, errors.New("listen address is required")
	}
	if handler == nil {
		return nil, errors.New("HTTP handler is required")
	}
	if err := validateControlServerTLS(config); err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		TLSConfig:         config.Clone(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       maxDataReadTime,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxControlHeaderBytes,
	}, nil
}

func validateControlClientTLS(config *tls.Config) error {
	if config == nil {
		return errors.New("TLS config is required")
	}
	if config.InsecureSkipVerify || config.MinVersion < tls.VersionTLS13 || config.ServerName == "" || config.RootCAs == nil || len(config.Certificates) == 0 || config.VerifyConnection == nil {
		return errors.New("control client requires TLS 1.3, CA and server-name verification, a client certificate, and exact peer identity verification")
	}
	return nil
}

func validateControlServerTLS(config *tls.Config) error {
	if config == nil {
		return errors.New("TLS config is required")
	}
	if config.InsecureSkipVerify || config.MinVersion < tls.VersionTLS13 || config.ClientAuth != tls.RequireAndVerifyClientCert || config.ClientCAs == nil || len(config.Certificates) == 0 || config.VerifyConnection == nil {
		return errors.New("control server requires TLS 1.3, a server certificate, mandatory client certificate verification, and exact peer identity verification")
	}
	return nil
}
