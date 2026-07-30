package mistral

import (
	"context"
	"testing"
	"time"
)

func TestRealtimeSessionDropsAudioWhenInputQueueIsFull(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()

	for i := 0; i < mistralAudioInBuffer+10; i++ {
		if err := session.SendAudio([]byte{1}); err != nil {
			t.Fatalf("SendAudio() error: %v", err)
		}
	}
	if session.DroppedAudio() == 0 {
		t.Fatal("expected dropped audio chunks when input queue is full")
	}
}

func TestRealtimeSessionRejectsLateAudioAfterInterrupt(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()

	turn := session.BeginTurn()
	if !session.PublishAudio(turn, []byte("first")) {
		t.Fatal("first audio should be published")
	}
	session.Interrupt()
	if session.PublishAudio(turn, []byte("late")) {
		t.Fatal("late audio from interrupted turn must be rejected")
	}
}

func TestRealtimeSessionCloseIsIdempotent(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	session.Close()
	session.Close()
	if err := session.SendAudio([]byte{1}); err == nil {
		t.Fatal("SendAudio should fail after close")
	}
}

func TestRealtimeSessionBeginTurnDiscardsPendingSentence(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()

	session.BeginTurn()
	if got := session.PushText("незавершённый ответ", false); len(got) != 0 {
		t.Fatalf("unexpected chunk before boundary: %v", got)
	}

	session.BeginTurn()
	got := session.PushText("новый ответ", true)
	if len(got) != 1 || got[0] != "новый ответ" {
		t.Fatalf("pending text leaked across turns: %v", got)
	}
}

func TestRealtimeSessionOwnsAudioBuffers(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()

	in := []byte{1, 2, 3}
	if err := session.SendAudio(in); err != nil {
		t.Fatal(err)
	}
	in[0] = 9
	if got := <-session.AudioIn; got[0] != 1 {
		t.Fatalf("input buffer was not copied: %v", got)
	}

	turn := session.BeginTurn()
	out := []byte{4, 5, 6}
	if !session.PublishAudio(turn, out) {
		t.Fatal("audio should be published")
	}
	out[0] = 9
	if got := <-session.AudioOut; got[0] != 4 {
		t.Fatalf("output buffer was not copied: %v", got)
	}
}

type duplicateFinalSTT struct{}

func (duplicateFinalSTT) Run(_ context.Context, _ <-chan []byte, callback func(string, bool) error) error {
	if err := callback("одна фраза", true); err != nil {
		return err
	}
	return callback("одна фраза", true)
}

func TestRealtimeSessionDeduplicatesFinalTranscripts(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()

	var turns []uint64
	if err := session.StartSTT(duplicateFinalSTT{}, func(_ string, turnID uint64) error {
		turns = append(turns, turnID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Context().Done():
	case <-time.After(100 * time.Millisecond):
	}
	if len(turns) != 1 {
		t.Fatalf("duplicate final transcript was processed %d times", len(turns))
	}
	if session.Metrics().FinalTranscriptAt.Load() == 0 {
		t.Fatal("final transcript timestamp was not recorded")
	}
}

func TestRealtimeSessionMetricsTrackInterruptAndAudioDrop(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()
	for i := 0; i < mistralAudioInBuffer+1; i++ {
		_ = session.SendAudio([]byte{1})
	}
	session.Interrupt()
	if session.Metrics().DroppedAudio.Load() == 0 {
		t.Fatal("dropped audio metric was not incremented")
	}
	if session.Metrics().Interruptions.Load() != 1 {
		t.Fatal("interruption metric was not incremented")
	}
}
