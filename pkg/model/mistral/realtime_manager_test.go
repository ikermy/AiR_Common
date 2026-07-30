package mistral

import (
	"context"
	"testing"
)

func TestRealtimeManagerReusesSession(t *testing.T) {
	manager := NewRealtimeManager(context.Background())
	first, err := manager.Start(1, 2, 3)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	second, err := manager.Start(1, 2, 3)
	if err != nil {
		t.Fatalf("Start() second error: %v", err)
	}
	if first != second {
		t.Fatal("Start() should reuse session for the same respID")
	}
	manager.Close(3)
	if _, ok := manager.Get(3); ok {
		t.Fatal("closed session must be removed")
	}
}

func TestRealtimeManagerCloseAll(t *testing.T) {
	manager := NewRealtimeManager(context.Background())
	_, _ = manager.Start(1, 2, 3)
	_, _ = manager.Start(1, 4, 5)
	manager.CloseAll()
	if _, ok := manager.Get(3); ok {
		t.Fatal("session 3 remains after CloseAll")
	}
	if _, ok := manager.Get(5); ok {
		t.Fatal("session 5 remains after CloseAll")
	}
}
