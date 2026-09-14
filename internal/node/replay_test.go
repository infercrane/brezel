package node

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReplayCacheConsumesCapabilityOnce(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	cache, err := NewReplayCache(2)
	if err != nil {
		t.Fatal(err)
	}
	cache.now = func() time.Time { return now }
	if err := cache.Use("jti-one", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := cache.Use("jti-one", now.Add(time.Minute)); !errors.Is(err, ErrCapabilityReplay) {
		t.Fatalf("replay error = %v", err)
	}
}

func TestReplayCacheFailsClosedAtCapacityAndReclaimsExpired(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	cache, err := NewReplayCache(1)
	if err != nil {
		t.Fatal(err)
	}
	cache.now = func() time.Time { return now }
	if err := cache.Use("jti-one", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := cache.Use("jti-two", now.Add(time.Minute)); !errors.Is(err, ErrReplayCapacity) {
		t.Fatalf("capacity error = %v", err)
	}
	now = now.Add(2 * time.Second)
	if err := cache.Use("jti-two", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if cache.Len() != 1 {
		t.Fatalf("live entries = %d", cache.Len())
	}
}

func TestReplayCacheConcurrentUseHasSingleWinner(t *testing.T) {
	cache, err := NewReplayCache(16)
	if err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			if cache.Use("same-jti", time.Now().Add(time.Minute)) == nil {
				winners.Add(1)
			}
		}()
	}
	group.Wait()
	if winners.Load() != 1 {
		t.Fatalf("successful uses = %d", winners.Load())
	}
}

func TestReplayCacheRejectsUnsafeInputs(t *testing.T) {
	if _, err := NewReplayCache(0); err == nil {
		t.Fatal("zero capacity accepted")
	}
	cache, _ := NewReplayCache(1)
	for _, jti := range []string{"", "bad\nvalue"} {
		if err := cache.Use(jti, time.Now().Add(time.Minute)); err == nil {
			t.Fatalf("unsafe jti %q accepted", jti)
		}
	}
	if err := cache.Use("already-expired", time.Now().Add(-time.Second)); err == nil {
		t.Fatal("expired capability accepted")
	}
}
