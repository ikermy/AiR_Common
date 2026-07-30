package mistral

import (
	"context"
	"testing"
)

func TestRealtimeSessionStateAccessors(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()
	if session.AudioOutput() != session.AudioOut || session.DrainOutput() != session.DrainPlayback {
		t.Fatal("session audio accessors returned wrong channels")
	}
	if session.Generating() == nil {
		t.Fatal("Generating() returned nil")
	}
	session.Generating().Store(true)
	if !session.Generating().Load() {
		t.Fatal("generation state was not updated")
	}
}
