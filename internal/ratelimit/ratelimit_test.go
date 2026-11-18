package ratelimit

import (
	"testing"
	"time"
)

func TestWindow(t *testing.T) {
	w := New(2, 30*time.Second)
	start := time.Date(2026, 11, 18, 20, 0, 0, 0, time.UTC)
	for i := range 2 {
		if ok, _ := w.Allow(start.Add(time.Duration(i) * time.Second)); !ok {
			t.Fatalf("event %d denied", i)
		}
	}
	if ok, first := w.Allow(start.Add(2 * time.Second)); ok || !first {
		t.Fatalf("third: ok %v first %v", ok, first)
	}
	if ok, first := w.Allow(start.Add(3 * time.Second)); ok || first {
		t.Fatalf("fourth: ok %v first %v (only the first denial is first)", ok, first)
	}
	// The first event left the window: one more fits.
	if ok, _ := w.Allow(start.Add(30 * time.Second)); !ok {
		t.Fatal("must allow again once the oldest is out of the window")
	}
	if ok, first := w.Allow(start.Add(30 * time.Second)); ok || !first {
		t.Fatalf("after an allowed event a denial is first again: ok %v first %v", ok, first)
	}
}
