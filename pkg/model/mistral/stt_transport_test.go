package mistral

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeSTTTransport struct{}

type failingSTTTransport struct{}

func (failingSTTTransport) Run(context.Context, <-chan []byte, func(string, bool) error) error {
	return errors.New("transport failed")
}

func (fakeSTTTransport) Run(ctx context.Context, audio <-chan []byte, callback func(string, bool) error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-audio:
			return callback("recognized speech", true)
		}
	}
}

func TestRealtimeSessionStartsSTTAndCreatesTurn(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()
	called := make(chan uint64, 1)
	if err := session.StartSTT(fakeSTTTransport{}, func(_ string, turnID uint64) error {
		called <- turnID
		return nil
	}); err != nil {
		t.Fatalf("StartSTT() error: %v", err)
	}
	if err := session.SendAudio([]byte("pcm")); err != nil {
		t.Fatalf("SendAudio() error: %v", err)
	}
	select {
	case turnID := <-called:
		if turnID == 0 {
			t.Fatal("final transcript should create a non-zero turn")
		}
	case <-time.After(time.Second):
		t.Fatal("final transcript callback was not called")
	}
}

func TestRealtimeSessionPublishesSTTTransportError(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()
	if err := session.StartSTT(failingSTTTransport{}, func(string, uint64) error { return nil }); err != nil {
		t.Fatalf("StartSTT() error: %v", err)
	}
	select {
	case err := <-session.Errors:
		if err == nil || err.Error() != "transport failed" {
			t.Fatalf("unexpected transport error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("transport error was not published")
	}
}

func TestRealtimeSessionAllowsOnlyOneSTTTransport(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()
	callback := func(string, uint64) error { return nil }
	if err := session.StartSTT(fakeSTTTransport{}, callback); err != nil {
		t.Fatalf("first StartSTT() error: %v", err)
	}
	if err := session.StartSTT(fakeSTTTransport{}, callback); err == nil {
		t.Fatal("second STT transport should be rejected")
	}
}
