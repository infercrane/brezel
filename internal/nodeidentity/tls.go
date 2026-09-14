package nodeidentity

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const (
	maxCertificateBytes = 1 << 20
	maxPrivateKeyBytes  = 256 << 10
)

// Files contains the private on-host identity material used by one process.
// All three paths must name private regular files; symlinks are rejected.
type Files struct {
	CertificateFile string
	PrivateKeyFile  string
	CAFile          string
}

// ServerOptions defines a node control server identity and its one permitted
// API peer identity.
type ServerOptions struct {
	Files          Files
	LocalIdentity  Identity
	ClientIdentity Identity
	Now            func() time.Time
}

// ClientOptions defines an API client identity and the exact node it expects.
// ServerName is independently verified against the server certificate's DNS
// or IP SAN by crypto/tls; the node URI SAN is checked in addition.
type ClientOptions struct {
	Files          Files
	LocalIdentity  Identity
	ServerIdentity Identity
	ServerName     string
	Now            func() time.Time
}

// LoadServerTLSConfig returns a TLS 1.3 configuration that requires a trusted
// client certificate with exactly the configured API URI identity.
func LoadServerTLSConfig(options ServerOptions) (*tls.Config, error) {
	if options.LocalIdentity.Role != RoleNode {
		return nil, errors.New("server local identity must have node role")
	}
	if options.ClientIdentity.Role != RoleAPI {
		return nil, errors.New("server peer identity must have api role")
	}
	now := clock(options.Now)
	certificate, leaf, roots, err := loadMaterial(options.Files, options.LocalIdentity, x509.ExtKeyUsageServerAuth, now)
	if err != nil {
		return nil, err
	}
	certificate.Leaf = leaf
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    roots,
		Time:         now,
		VerifyConnection: peerVerifier(
			options.ClientIdentity,
			x509.ExtKeyUsageClientAuth,
			now,
		),
	}, nil
}

// LoadClientTLSConfig returns a TLS 1.3 configuration that performs normal CA
// and DNS SAN verification and additionally requires the exact node URI SAN.
func LoadClientTLSConfig(options ClientOptions) (*tls.Config, error) {
	if options.LocalIdentity.Role != RoleAPI {
		return nil, errors.New("client local identity must have api role")
	}
	if options.ServerIdentity.Role != RoleNode {
		return nil, errors.New("client peer identity must have node role")
	}
	if options.ServerName == "" {
		return nil, errors.New("server name is required for DNS or IP SAN verification")
	}
	now := clock(options.Now)
	certificate, leaf, roots, err := loadMaterial(options.Files, options.LocalIdentity, x509.ExtKeyUsageClientAuth, now)
	if err != nil {
		return nil, err
	}
	certificate.Leaf = leaf
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ServerName:   options.ServerName,
		RootCAs:      roots,
		Certificates: []tls.Certificate{certificate},
		Time:         now,
		VerifyConnection: peerVerifier(
			options.ServerIdentity,
			x509.ExtKeyUsageServerAuth,
			now,
		),
	}, nil
}

func clock(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

func loadMaterial(files Files, expected Identity, usage x509.ExtKeyUsage, now func() time.Time) (tls.Certificate, *x509.Certificate, *x509.CertPool, error) {
	certificatePEM, err := readPrivateRegular(files.CertificateFile, maxCertificateBytes)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("load certificate: %w", err)
	}
	privateKeyPEM, err := readPrivateRegular(files.PrivateKeyFile, maxPrivateKeyBytes)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("load private key: %w", err)
	}
	caPEM, err := readPrivateRegular(files.CAFile, maxCertificateBytes)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("load CA: %w", err)
	}
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("parse certificate and key: %w", err)
	}
	if len(certificate.Certificate) == 0 {
		return tls.Certificate{}, nil, nil, errors.New("certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("parse leaf certificate: %w", err)
	}
	roots, err := parseCAPool(caPEM)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	if err := verifyLeaf(leaf, certificate.Certificate[1:], roots, expected, usage, now()); err != nil {
		return tls.Certificate{}, nil, nil, fmt.Errorf("validate local certificate: %w", err)
	}
	return certificate, leaf, roots, nil
}

func parseCAPool(data []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	remaining := data
	count := 0
	for {
		block, rest := pem.Decode(remaining)
		if block == nil {
			break
		}
		remaining = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificates, err := x509.ParseCertificates(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse CA certificate: %w", err)
		}
		for _, certificate := range certificates {
			if !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
				return nil, errors.New("CA file contains a certificate that is not a signing authority")
			}
			pool.AddCert(certificate)
			count++
		}
	}
	if count == 0 {
		return nil, errors.New("CA file contains no certificates")
	}
	return pool, nil
}

func verifyLeaf(leaf *x509.Certificate, intermediatesDER [][]byte, roots *x509.CertPool, expected Identity, usage x509.ExtKeyUsage, now time.Time) error {
	if err := verifyIdentityAndUsage(leaf, expected, usage, now); err != nil {
		return err
	}
	intermediates := x509.NewCertPool()
	for _, encoded := range intermediatesDER {
		certificate, err := x509.ParseCertificate(encoded)
		if err != nil {
			return fmt.Errorf("parse intermediate certificate: %w", err)
		}
		intermediates.AddCert(certificate)
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{usage},
	})
	if err != nil {
		return fmt.Errorf("verify certificate chain: %w", err)
	}
	return nil
}

func peerVerifier(expected Identity, usage x509.ExtKeyUsage, now func() time.Time) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
			return errors.New("peer did not present a verified certificate chain")
		}
		return verifyIdentityAndUsage(state.PeerCertificates[0], expected, usage, now())
	}
}

func verifyIdentityAndUsage(certificate *x509.Certificate, expected Identity, usage x509.ExtKeyUsage, now time.Time) error {
	expectedURI, err := expected.URI()
	if err != nil {
		return fmt.Errorf("invalid expected identity: %w", err)
	}
	if now.Before(certificate.NotBefore) {
		return errors.New("certificate is not yet valid")
	}
	if !now.Before(certificate.NotAfter) {
		return errors.New("certificate is expired")
	}
	if certificate.KeyUsage != 0 && certificate.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return errors.New("leaf certificate does not permit digital signatures")
	}
	if len(certificate.URIs) != 1 || certificate.URIs[0].String() != expectedURI.String() {
		return fmt.Errorf("certificate URI identity must be exactly %q", expectedURI.String())
	}
	required := false
	for _, candidate := range certificate.ExtKeyUsage {
		if candidate == x509.ExtKeyUsageAny {
			return errors.New("leaf certificate cannot use the any extended key usage")
		}
		if candidate == usage {
			required = true
		}
		if usage == x509.ExtKeyUsageServerAuth && candidate == x509.ExtKeyUsageClientAuth {
			return errors.New("server leaf certificate cannot carry the client authentication usage")
		}
		if usage == x509.ExtKeyUsageClientAuth && candidate == x509.ExtKeyUsageServerAuth {
			return errors.New("client leaf certificate cannot carry the server authentication usage")
		}
	}
	if !required {
		return errors.New("leaf certificate is missing its required extended key usage")
	}
	return nil
}

func readPrivateRegular(path string, maximum int64) ([]byte, error) {
	if path == "" {
		return nil, errors.New("file path is required")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, errors.New("path must name a regular file, not a symlink")
	}
	if before.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("file permissions must not allow group or other access")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errors.New("file changed while it was being opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	if len(data) == 0 {
		return nil, errors.New("file is empty")
	}
	return data, nil
}
