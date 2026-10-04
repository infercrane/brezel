// Package websocketbridge copies bounded WebSocket messages between two
// authenticated endpoints without inspecting or retaining their contents.
package websocketbridge

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/coder/websocket"
)

const maxMessageBytes = 16 << 20

// Bridge forwards messages until either peer closes, the context expires, or
// one direction exceeds its cumulative byte budget. The first budget covers
// left-to-right traffic and the second covers right-to-left traffic.
func Bridge(ctx context.Context, left, right *websocket.Conn, leftToRightBytes, rightToLeftBytes int64) error {
	if left == nil || right == nil {
		return errors.New("both WebSocket peers are required")
	}
	if leftToRightBytes < 1 || rightToLeftBytes < 1 {
		return errors.New("WebSocket byte budgets must be positive")
	}
	left.SetReadLimit(min64(maxMessageBytes, leftToRightBytes))
	right.SetReadLimit(min64(maxMessageBytes, rightToLeftBytes))

	bridgeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsFound := make(chan error, 2)
	var closeOnce sync.Once
	closePeers := func() {
		closeOnce.Do(func() {
			_ = left.CloseNow()
			_ = right.CloseNow()
		})
	}
	copyDirection := func(source, destination *websocket.Conn, budget int64) {
		var transferred int64
		for {
			messageType, payload, err := source.Read(bridgeCtx)
			if err != nil {
				errorsFound <- err
				return
			}
			transferred += int64(len(payload))
			if transferred > budget {
				errorsFound <- fmt.Errorf("WebSocket byte budget exceeded")
				return
			}
			if err := destination.Write(bridgeCtx, messageType, payload); err != nil {
				errorsFound <- err
				return
			}
		}
	}
	go copyDirection(left, right, leftToRightBytes)
	go copyDirection(right, left, rightToLeftBytes)
	first := <-errorsFound
	cancel()
	closePeers()
	<-errorsFound
	if ctx.Err() != nil {
		return ctx.Err()
	}
	status := websocket.CloseStatus(first)
	if status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway || status == websocket.StatusNoStatusRcvd {
		return nil
	}
	return first
}

func min64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}
