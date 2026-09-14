package nodeidentity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var certificateSerial atomic.Int64

type testAuthority struct {
	certificate    *x509.Certificate
	certificatePEM []byte
	privateKey     *ecdsa.PrivateKey
}

type testMaterial struct {
	certificate []byte
	privateKey  []byte
	ca          []byte
}

func TestMutuallyAuthenticatedControlConnection(t *testing.T) {
	now := time.Now().UTC()
	authority := newTestAuthority(t, now)
	node := mustIdentity(t, RoleNode, "node-a")
	api := mustIdentity(t, RoleAPI, "api-a")
	nodeMaterial := authority.issue(t, node, []string{"node-a.internal"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil)
	apiMaterial := authority.issue(t, api, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil)

	serverConfig, err := LoadServerTLSConfig(ServerOptions{Files: writeMaterial(t, nodeMaterial), LocalIdentity: node, ClientIdentity: api, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := LoadClientTLSConfig(ClientOptions{Files: writeMaterial(t, apiMaterial), LocalIdentity: api, ServerIdentity: node, ServerName: "node-a.internal", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewControlHTTPClient(clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	dataClient, err := NewDataHTTPClient(clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	if dataClient.Timeout != 0 {
		t.Fatalf("data client has an unexpected global timeout: %s", dataClient.Timeout)
	}
	dataServer, err := NewDataHTTPServer("127.0.0.1:0", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), serverConfig)
	if err != nil {
		t.Fatal(err)
	}
	if dataServer.ReadTimeout != maxDataReadTime || dataServer.WriteTimeout != 0 {
		t.Fatalf("data server timeouts read=%s write=%s", dataServer.ReadTimeout, dataServer.WriteTimeout)
	}
	address, shutdown := serveTLS(t, serverConfig, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer shutdown()
	response, err := client.Get("https://" + address)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d", response.StatusCode)
	}
	if serverConfig.MinVersion != tls.VersionTLS13 || clientConfig.MinVersion != tls.VersionTLS13 || clientConfig.InsecureSkipVerify {
		t.Fatalf("unsafe TLS config: server=%d client=%d insecure=%v", serverConfig.MinVersion, clientConfig.MinVersion, clientConfig.InsecureSkipVerify)
	}
}

func TestUntrustedAuthorityIsRejected(t *testing.T) {
	now := time.Now().UTC()
	serverAuthority := newTestAuthority(t, now)
	clientAuthority := newTestAuthority(t, now)
	node := mustIdentity(t, RoleNode, "node-a")
	api := mustIdentity(t, RoleAPI, "api-a")
	nodeFiles := writeMaterial(t, serverAuthority.issue(t, node, []string{"node-a.internal"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil))
	apiFiles := writeMaterial(t, clientAuthority.issue(t, api, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil))
	serverConfig, err := LoadServerTLSConfig(ServerOptions{Files: nodeFiles, LocalIdentity: node, ClientIdentity: api})
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := LoadClientTLSConfig(ClientOptions{Files: apiFiles, LocalIdentity: api, ServerIdentity: node, ServerName: "node-a.internal"})
	if err != nil {
		t.Fatal(err)
	}
	address, shutdown := serveTLS(t, serverConfig, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer shutdown()
	client, _ := NewControlHTTPClient(clientConfig)
	if response, err := client.Get("https://" + address); err == nil {
		response.Body.Close()
		t.Fatal("connection with an untrusted authority succeeded")
	}
}

func TestLocalCertificateRoleAndUsageAreEnforced(t *testing.T) {
	now := time.Now().UTC()
	authority := newTestAuthority(t, now)
	node := mustIdentity(t, RoleNode, "node-a")
	api := mustIdentity(t, RoleAPI, "api-a")

	wrongUsage := authority.issue(t, api, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil)
	if _, err := LoadClientTLSConfig(ClientOptions{Files: writeMaterial(t, wrongUsage), LocalIdentity: api, ServerIdentity: node, ServerName: "node-a.internal"}); err == nil || !strings.Contains(err.Error(), "authentication usage") {
		t.Fatalf("wrong client EKU error=%v", err)
	}
	validNode := authority.issue(t, node, []string{"node-a.internal"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil)
	if _, err := LoadServerTLSConfig(ServerOptions{Files: writeMaterial(t, validNode), LocalIdentity: api, ClientIdentity: api}); err == nil || !strings.Contains(err.Error(), "node role") {
		t.Fatalf("wrong server role error=%v", err)
	}
	if _, err := LoadClientTLSConfig(ClientOptions{Files: writeMaterial(t, validNode), LocalIdentity: node, ServerIdentity: node, ServerName: "node-a.internal"}); err == nil || !strings.Contains(err.Error(), "api role") {
		t.Fatalf("wrong client role error=%v", err)
	}
}

func TestWrongNodeIdentityAndServerNameAreRejected(t *testing.T) {
	now := time.Now().UTC()
	authority := newTestAuthority(t, now)
	nodeA := mustIdentity(t, RoleNode, "node-a")
	nodeB := mustIdentity(t, RoleNode, "node-b")
	api := mustIdentity(t, RoleAPI, "api-a")
	nodeFiles := writeMaterial(t, authority.issue(t, nodeA, []string{"node-a.internal"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil))
	apiFiles := writeMaterial(t, authority.issue(t, api, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil))
	serverConfig, err := LoadServerTLSConfig(ServerOptions{Files: nodeFiles, LocalIdentity: nodeA, ClientIdentity: api})
	if err != nil {
		t.Fatal(err)
	}
	address, shutdown := serveTLS(t, serverConfig, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer shutdown()
	for name, expected := range map[string]struct {
		identity Identity
		name     string
	}{
		"wrong URI identity": {identity: nodeB, name: "node-a.internal"},
		"wrong DNS identity": {identity: nodeA, name: "node-b.internal"},
	} {
		t.Run(name, func(t *testing.T) {
			config, err := LoadClientTLSConfig(ClientOptions{Files: apiFiles, LocalIdentity: api, ServerIdentity: expected.identity, ServerName: expected.name})
			if err != nil {
				t.Fatal(err)
			}
			client, _ := NewControlHTTPClient(config)
			if response, err := client.Get("https://" + address); err == nil {
				response.Body.Close()
				t.Fatal("connection with wrong server identity succeeded")
			}
		})
	}
}

func TestInvalidCertificateValidityIsRejectedAtLoad(t *testing.T) {
	now := time.Now().UTC()
	authority := newTestAuthority(t, now)
	node := mustIdentity(t, RoleNode, "node-a")
	api := mustIdentity(t, RoleAPI, "api-a")
	for name, validity := range map[string]struct {
		notBefore time.Time
		notAfter  time.Time
		errorText string
	}{
		"expired":       {notBefore: now.Add(-2 * time.Hour), notAfter: now.Add(-time.Minute), errorText: "expired"},
		"not yet valid": {notBefore: now.Add(time.Minute), notAfter: now.Add(2 * time.Hour), errorText: "not yet valid"},
	} {
		t.Run(name, func(t *testing.T) {
			material := authority.issue(t, api, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, validity.notBefore, validity.notAfter, nil)
			_, err := LoadClientTLSConfig(ClientOptions{Files: writeMaterial(t, material), LocalIdentity: api, ServerIdentity: node, ServerName: "node-a.internal", Now: func() time.Time { return now }})
			if err == nil || !strings.Contains(err.Error(), validity.errorText) {
				t.Fatalf("validity error=%v", err)
			}
		})
	}
}

func TestMissingClientCertificateIsRejected(t *testing.T) {
	now := time.Now().UTC()
	authority := newTestAuthority(t, now)
	node := mustIdentity(t, RoleNode, "node-a")
	api := mustIdentity(t, RoleAPI, "api-a")
	nodeFiles := writeMaterial(t, authority.issue(t, node, []string{"node-a.internal"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil))
	serverConfig, err := LoadServerTLSConfig(ServerOptions{Files: nodeFiles, LocalIdentity: node, ClientIdentity: api})
	if err != nil {
		t.Fatal(err)
	}
	address, shutdown := serveTLS(t, serverConfig, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer shutdown()
	roots, err := parseCAPool(authority.certificatePEM)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "node-a.internal"}}, Timeout: 3 * time.Second}
	if response, err := client.Get("https://" + address); err == nil {
		response.Body.Close()
		t.Fatal("connection without a client certificate succeeded")
	}
}

func TestServerRejectsWrongClientIdentityAndRole(t *testing.T) {
	now := time.Now().UTC()
	authority := newTestAuthority(t, now)
	node := mustIdentity(t, RoleNode, "node-a")
	apiA := mustIdentity(t, RoleAPI, "api-a")
	apiB := mustIdentity(t, RoleAPI, "api-b")
	nodeB := mustIdentity(t, RoleNode, "node-b")
	nodeFiles := writeMaterial(t, authority.issue(t, node, []string{"node-a.internal"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil))
	serverConfig, err := LoadServerTLSConfig(ServerOptions{Files: nodeFiles, LocalIdentity: node, ClientIdentity: apiA, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	address, shutdown := serveTLS(t, serverConfig, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer shutdown()

	wrongIdentity := authority.issue(t, apiB, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil)
	wrongIdentityConfig, err := LoadClientTLSConfig(ClientOptions{Files: writeMaterial(t, wrongIdentity), LocalIdentity: apiB, ServerIdentity: node, ServerName: "node-a.internal", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	wrongIdentityClient, _ := NewControlHTTPClient(wrongIdentityConfig)
	if response, err := wrongIdentityClient.Get("https://" + address); err == nil {
		response.Body.Close()
		t.Fatal("server accepted a different API identity")
	}

	wrongRole := authority.issue(t, nodeB, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil)
	wrongRoleClient := rawMTLSClient(t, wrongRole, "node-a.internal")
	if response, err := wrongRoleClient.Get("https://" + address); err == nil {
		response.Body.Close()
		t.Fatal("server accepted a node identity as an API identity")
	}
}

func TestServerRejectsInvalidClientCertificateValidity(t *testing.T) {
	now := time.Now().UTC()
	authority := newTestAuthority(t, now)
	node := mustIdentity(t, RoleNode, "node-a")
	api := mustIdentity(t, RoleAPI, "api-a")
	nodeFiles := writeMaterial(t, authority.issue(t, node, []string{"node-a.internal"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil))
	serverConfig, err := LoadServerTLSConfig(ServerOptions{Files: nodeFiles, LocalIdentity: node, ClientIdentity: api, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	address, shutdown := serveTLS(t, serverConfig, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer shutdown()

	for name, validity := range map[string]struct {
		notBefore time.Time
		notAfter  time.Time
	}{
		"expired":       {notBefore: now.Add(-2 * time.Hour), notAfter: now.Add(-time.Minute)},
		"not yet valid": {notBefore: now.Add(time.Minute), notAfter: now.Add(time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			material := authority.issue(t, api, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, validity.notBefore, validity.notAfter, nil)
			client := rawMTLSClient(t, material, "node-a.internal")
			if response, err := client.Get("https://" + address); err == nil {
				response.Body.Close()
				t.Fatalf("server accepted %s client certificate", name)
			}
		})
	}
}

func TestPrivateRegularFileRequirements(t *testing.T) {
	now := time.Now().UTC()
	authority := newTestAuthority(t, now)
	node := mustIdentity(t, RoleNode, "node-a")
	api := mustIdentity(t, RoleAPI, "api-a")
	material := authority.issue(t, node, []string{"node-a.internal"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil)

	for _, field := range []string{"certificate", "private key", "CA"} {
		t.Run("world-readable "+field, func(t *testing.T) {
			files := writeMaterial(t, material)
			path := materialPath(files, field)
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadServerTLSConfig(ServerOptions{Files: files, LocalIdentity: node, ClientIdentity: api})
			if err == nil || !strings.Contains(err.Error(), "permissions") {
				t.Fatalf("world-readable %s error=%v", field, err)
			}
		})
		t.Run("symlink "+field, func(t *testing.T) {
			files := writeMaterial(t, material)
			target := materialPath(files, field)
			link := filepath.Join(t.TempDir(), strings.ReplaceAll(field, " ", "-")+".link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			setMaterialPath(&files, field, link)
			_, err := LoadServerTLSConfig(ServerOptions{Files: files, LocalIdentity: node, ClientIdentity: api})
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("symlink %s error=%v", field, err)
			}
		})
	}
}

func TestIdentityMustBeTheOnlyURISAN(t *testing.T) {
	now := time.Now().UTC()
	authority := newTestAuthority(t, now)
	node := mustIdentity(t, RoleNode, "node-a")
	api := mustIdentity(t, RoleAPI, "api-a")
	extra, _ := url.Parse("spiffe://brezel/node/other")
	material := authority.issue(t, node, []string{"node-a.internal"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now.Add(-time.Minute), now.Add(time.Hour), []*url.URL{extra})
	if _, err := LoadServerTLSConfig(ServerOptions{Files: writeMaterial(t, material), LocalIdentity: node, ClientIdentity: api}); err == nil || !strings.Contains(err.Error(), "exactly") {
		t.Fatalf("multiple URI SAN error=%v", err)
	}
}

func TestControlHTTPClientDoesNotFollowRedirects(t *testing.T) {
	now := time.Now().UTC()
	authority := newTestAuthority(t, now)
	node := mustIdentity(t, RoleNode, "node-a")
	api := mustIdentity(t, RoleAPI, "api-a")
	nodeFiles := writeMaterial(t, authority.issue(t, node, []string{"node-a.internal"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil))
	apiFiles := writeMaterial(t, authority.issue(t, api, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, now.Add(-time.Minute), now.Add(time.Hour), nil))
	serverConfig, _ := LoadServerTLSConfig(ServerOptions{Files: nodeFiles, LocalIdentity: node, ClientIdentity: api})
	clientConfig, _ := LoadClientTLSConfig(ClientOptions{Files: apiFiles, LocalIdentity: api, ServerIdentity: node, ServerName: "node-a.internal"})
	var followed atomic.Bool
	address, shutdown := serveTLS(t, serverConfig, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/destination" {
			followed.Store(true)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, "/destination", http.StatusTemporaryRedirect)
	}))
	defer shutdown()
	client, _ := NewControlHTTPClient(clientConfig)
	response, err := client.Get("https://" + address)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || followed.Load() {
		t.Fatalf("status=%d followed=%v", response.StatusCode, followed.Load())
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.MaxResponseHeaderBytes != maxControlHeaderBytes || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("unsafe control transport: %#v", client.Transport)
	}
}

func TestIdentityValidation(t *testing.T) {
	for _, test := range []struct {
		role Role
		id   string
	}{
		{role: "operator", id: "api-a"},
		{role: RoleAPI, id: ""},
		{role: RoleNode, id: "Uppercase"},
		{role: RoleNode, id: "-leading"},
		{role: RoleNode, id: strings.Repeat("a", 64)},
	} {
		if _, err := NewIdentity(test.role, test.id); err == nil {
			t.Fatalf("NewIdentity(%q, %q) succeeded", test.role, test.id)
		}
	}
}

func newTestAuthority(t *testing.T, now time.Time) *testAuthority {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(certificateSerial.Add(1)),
		Subject:               pkix.Name{CommonName: "Brezel test authority"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return &testAuthority{certificate: certificate, certificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: encoded}), privateKey: privateKey}
}

func (authority *testAuthority) issue(t *testing.T, identity Identity, dnsNames []string, usages []x509.ExtKeyUsage, notBefore, notAfter time.Time, extraURIs []*url.URL) testMaterial {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identityURI, err := identity.URI()
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(certificateSerial.Add(1)),
		Subject:               pkix.Name{CommonName: identity.String()},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           usages,
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		URIs:                  append([]*url.URL{identityURI}, extraURIs...),
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, authority.certificate, &privateKey.PublicKey, authority.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return testMaterial{
		certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: encoded}),
		privateKey:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}),
		ca:          authority.certificatePEM,
	}
}

func writeMaterial(t *testing.T, material testMaterial) Files {
	t.Helper()
	directory := t.TempDir()
	files := Files{
		CertificateFile: filepath.Join(directory, "identity.crt"),
		PrivateKeyFile:  filepath.Join(directory, "identity.key"),
		CAFile:          filepath.Join(directory, "ca.crt"),
	}
	for path, data := range map[string][]byte{
		files.CertificateFile: material.certificate,
		files.PrivateKeyFile:  material.privateKey,
		files.CAFile:          material.ca,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func materialPath(files Files, field string) string {
	switch field {
	case "certificate":
		return files.CertificateFile
	case "private key":
		return files.PrivateKeyFile
	case "CA":
		return files.CAFile
	default:
		panic("unknown material field")
	}
}

func setMaterialPath(files *Files, field, path string) {
	switch field {
	case "certificate":
		files.CertificateFile = path
	case "private key":
		files.PrivateKeyFile = path
	case "CA":
		files.CAFile = path
	default:
		panic("unknown material field")
	}
}

func mustIdentity(t *testing.T, role Role, id string) Identity {
	t.Helper()
	identity, err := NewIdentity(role, id)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func rawMTLSClient(t *testing.T, material testMaterial, serverName string) *http.Client {
	t.Helper()
	certificate, err := tls.X509KeyPair(material.certificate, material.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := parseCAPool(material.ca)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			ServerName:   serverName,
			RootCAs:      roots,
			Certificates: []tls.Certificate{certificate},
		}},
		Timeout: 3 * time.Second,
	}
}

func serveTLS(t *testing.T, config *tls.Config, handler http.Handler) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewControlHTTPServer(listener.Addr().String(), handler, config)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(tls.NewListener(listener, server.TLSConfig))
	}()
	return listener.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		err := <-serveDone
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve TLS: %v", err)
		}
	}
}

func TestHTTPConstructorsRejectIncompleteConfiguration(t *testing.T) {
	if _, err := NewControlHTTPClient(nil); err == nil {
		t.Fatal("nil client TLS config succeeded")
	}
	if _, err := NewDataHTTPClient(nil); err == nil {
		t.Fatal("nil data client TLS config succeeded")
	}
	if _, err := NewControlHTTPServer("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &tls.Config{}); err == nil {
		t.Fatal("empty listen address succeeded")
	}
	if _, err := NewControlHTTPServer("127.0.0.1:0", nil, &tls.Config{}); err == nil {
		t.Fatal("nil server handler succeeded")
	}
	if _, err := NewControlHTTPServer("127.0.0.1:0", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil); err == nil {
		t.Fatal("nil server TLS config succeeded")
	}
	if _, err := NewDataHTTPServer("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &tls.Config{}); err == nil {
		t.Fatal("empty data listen address succeeded")
	}
	if _, err := NewDataHTTPServer("127.0.0.1:0", nil, &tls.Config{}); err == nil {
		t.Fatal("nil data server handler succeeded")
	}
	if _, err := NewDataHTTPServer("127.0.0.1:0", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), nil); err == nil {
		t.Fatal("nil data server TLS config succeeded")
	}
}

func ExampleIdentity() {
	identity, _ := NewIdentity(RoleNode, "node-a")
	fmt.Println(identity)
	// Output: spiffe://brezel/node/node-a
}
