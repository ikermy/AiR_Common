package startpoint

import (
	"testing"

	"github.com/ikermy/AiR_Common/pkg/model"
)

func TestExtractStreamText_PartialMessage(t *testing.T) {
	text, complete, err := extractStreamText(`{"message":"Прив`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if complete {
		t.Fatalf("expected incomplete message")
	}
	if text != "Прив" {
		t.Fatalf("unexpected text: %q", text)
	}
}

func TestExtractStreamText_CompleteMessageWithTrailingGarbage(t *testing.T) {
	text, complete, err := extractStreamText(`{"message":"Привет"},"token_usage":{"input":1}`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !complete {
		t.Fatalf("expected complete message")
	}
	if text != "Привет" {
		t.Fatalf("unexpected text: %q", text)
	}
}

func TestExtractStreamText_TopLevelMessagePreferred(t *testing.T) {
	text, complete, err := extractStreamText(`{"meta":{"message":"inner"},"message":"outer"`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !complete {
		t.Fatalf("expected complete message once top-level message string is closed")
	}
	if text != "outer" {
		t.Fatalf("unexpected text: %q", text)
	}
}

func TestExtractStreamText_MessageNotString(t *testing.T) {
	_, _, err := extractStreamText(`{"message":123}`)
	if err == nil {
		t.Fatalf("expected error for non-string message")
	}
}

func TestStart_ProcessStreamDeltaLifecycle(t *testing.T) {
	s := &Start{}
	respID := uint64(42)

	result, err := s.ProcessStreamDelta(respID, `{"message":"Hel`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Kind != model.StreamDeltaKindText {
		t.Fatalf("unexpected kind: %s", result.Kind)
	}
	if result.Complete {
		t.Fatalf("expected incomplete")
	}
	if result.Text != "Hel" {
		t.Fatalf("unexpected partial text: %q", result.Text)
	}

	result, err = s.ProcessStreamDelta(respID, `lo"}`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !result.Complete {
		t.Fatalf("expected complete")
	}
	if result.Text != "Hello" {
		t.Fatalf("unexpected final text: %q", result.Text)
	}

	if got := s.GetStreamDisplayText(respID); got != "Hello" {
		t.Fatalf("unexpected display text: %q", got)
	}

	s.ResetStreamAccumulator(respID)
	if got := s.GetStreamDisplayText(respID); got != "" {
		t.Fatalf("expected empty after reset, got: %q", got)
	}
}

func TestStart_ProcessStreamDelta_FunctionCallArgumentsDone(t *testing.T) {
	s := &Start{}

	result, err := s.ProcessStreamDelta(100, `{"type":"response.function_call_arguments.done","name":"search","arguments":"{\"q\":\"hi\"}"}`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Kind != model.StreamDeltaKindEvent {
		t.Fatalf("unexpected kind: %s", result.Kind)
	}
	if !result.Complete {
		t.Fatalf("expected complete event")
	}
	if result.EventType != "response.function_call_arguments.done" {
		t.Fatalf("unexpected event type: %q", result.EventType)
	}
	if result.Name != "search" {
		t.Fatalf("unexpected name: %q", result.Name)
	}
	if result.Arguments != `{"q":"hi"}` {
		t.Fatalf("unexpected arguments: %q", result.Arguments)
	}
}

func TestStart_ProcessStreamDelta_FunctionCallArgumentsDelta(t *testing.T) {
	s := &Start{}

	result, err := s.ProcessStreamDelta(101, `{"type":"response.function_call_arguments.delta","name":"search","delta":"{\"q\":\"hi"}`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if result.Kind != model.StreamDeltaKindEvent {
		t.Fatalf("unexpected kind: %s", result.Kind)
	}
	if result.Complete {
		t.Fatalf("expected incomplete delta event")
	}
	if result.EventType != "response.function_call_arguments.delta" {
		t.Fatalf("unexpected event type: %q", result.EventType)
	}
	if result.Arguments != `{"q":"hi` {
		t.Fatalf("unexpected delta arguments: %q", result.Arguments)
	}
}
