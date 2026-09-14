package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/nodeledger"
)

const (
	RelayCapabilityScheme = "BrezelCapability"
	RelayRouteHeader      = "X-Brezel-Route-ID"
	RelayContentSHAHeader = "X-Brezel-Content-SHA256"
	RelayFilePathHeader   = "X-Brezel-File-Path"
	RelayFileSizeHeader   = "X-Brezel-File-Size"
)

type relayRouteResponse struct {
	NodeID    string           `json:"node_id"`
	BootEpoch string           `json:"boot_epoch"`
	Route     nodeledger.Route `json:"route"`
}

type relayCommandRequest struct {
	Argv []string          `json:"argv"`
	Cwd  string            `json:"cwd,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
}

func commandProtocolRequest(request backend.CommandRequest) relayCommandRequest {
	return relayCommandRequest{Argv: append([]string(nil), request.Argv...), Cwd: request.Cwd, Env: cloneStringMap(request.Env)}
}

type relayFileDescriptor struct {
	Path   string `json:"path"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

func fileWriteDescriptor(path string, data []byte) relayFileDescriptor {
	digest := sha256.Sum256(data)
	return relayFileDescriptor{Path: path, Size: int64(len(data)), SHA256: base64.RawURLEncoding.EncodeToString(digest[:])}
}

type relayPortRequest struct {
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	RawQuery   string              `json:"raw_query,omitempty"`
	Header     map[string][]string `json:"header,omitempty"`
	BodySize   int64               `json:"body_size,omitempty"`
	BodySHA256 string              `json:"body_sha256,omitempty"`
}

func portProtocolRequest(method, path, rawQuery string, header http.Header, body []byte) relayPortRequest {
	request := relayPortRequest{Method: method, Path: path, RawQuery: rawQuery, Header: canonicalPortHeaders(header), BodySize: int64(len(body))}
	if len(body) > 0 {
		digest := sha256.Sum256(body)
		request.BodySHA256 = base64.RawURLEncoding.EncodeToString(digest[:])
	}
	return request
}

func canonicalRelayJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}

func decodeRelayJSON(source io.Reader, maximum int64, destination any) error {
	if source == nil || maximum < 1 {
		return errors.New("invalid relay request body")
	}
	data, err := io.ReadAll(io.LimitReader(source, maximum+1))
	if err != nil {
		return err
	}
	if len(data) == 0 || int64(len(data)) > maximum {
		return errors.New("relay request body is empty or exceeds its limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("relay request contains trailing JSON")
	}
	return nil
}

func canonicalPortHeaders(header http.Header) map[string][]string {
	if len(header) == 0 {
		return nil
	}
	names := make([]string, 0, len(header))
	for name := range header {
		name = http.CanonicalHeaderKey(name)
		if relayHeaderAllowed(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	result := make(map[string][]string, len(names))
	for _, name := range names {
		result[name] = append([]string(nil), header.Values(name)...)
	}
	return result
}

func relayHeaderAllowed(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Connection", "Cookie", "Forwarded", "Keep-Alive", "Origin", "Proxy-Authenticate", "Proxy-Authorization", "Referer", "Te", "Trailer", "Transfer-Encoding", "Upgrade", RelayRouteHeader, RelayContentSHAHeader:
		return false
	default:
		return !strings.HasPrefix(strings.ToLower(name), "x-brezel-")
	}
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
