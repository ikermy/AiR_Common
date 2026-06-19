package startpoint

import "testing"

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

	text, complete, err := s.ProcessStreamDelta(respID, `{"message":"Hel`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if complete {
		t.Fatalf("expected incomplete")
	}
	if text != "Hel" {
		t.Fatalf("unexpected partial text: %q", text)
	}

	text, complete, err = s.ProcessStreamDelta(respID, `lo"}`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !complete {
		t.Fatalf("expected complete")
	}
	if text != "Hello" {
		t.Fatalf("unexpected final text: %q", text)
	}

	if got := s.GetStreamDisplayText(respID); got != "Hello" {
		t.Fatalf("unexpected display text: %q", got)
	}

	s.ResetStreamAccumulator(respID)
	if got := s.GetStreamDisplayText(respID); got != "" {
		t.Fatalf("expected empty after reset, got: %q", got)
	}
}
