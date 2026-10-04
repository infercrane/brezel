package websocketbridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestBridgePreservesMessageTypesInBothDirections(t *testing.T) {
	leftClient, leftServer := websocketPair(t)
	rightClient, rightServer := websocketPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Bridge(ctx, leftServer, rightServer, 1024, 1024) }()

	if err := leftClient.Write(ctx, websocket.MessageText, []byte("from-left")); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := rightClient.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(payload) != "from-left" {
		t.Fatalf("right read = type %v payload %q err %v", typ, payload, err)
	}
	if err := rightClient.Write(ctx, websocket.MessageBinary, []byte{0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	typ, payload, err = leftClient.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(payload) != string([]byte{0, 1, 2}) {
		t.Fatalf("left read = type %v payload %v err %v", typ, payload, err)
	}
	_ = leftClient.Close(websocket.StatusNormalClosure, "done")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bridge returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal("bridge did not stop")
	}
}

func TestBridgeRejectsCumulativeBudgetOverrun(t *testing.T) {
	leftClient, leftServer := websocketPair(t)
	rightClient, rightServer := websocketPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Bridge(ctx, leftServer, rightServer, 5, 1024) }()

	if err := leftClient.Write(ctx, websocket.MessageText, []byte("1234")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rightClient.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err := leftClient.Write(ctx, websocket.MessageText, []byte("56")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "budget") {
			t.Fatalf("bridge error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("bridge did not enforce the byte budget")
	}
}

func websocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		accepted <- conn
	}))
	t.Cleanup(server.Close)
	client, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })
	var peer *websocket.Conn
	select {
	case peer = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not accept WebSocket")
	}
	t.Cleanup(func() { _ = peer.CloseNow() })
	return client, peer
}
