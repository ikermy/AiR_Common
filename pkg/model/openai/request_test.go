package openai

import (
	"testing"

	"github.com/ikermy/AiR_Common/pkg/model"
)

func TestUnmarshalAssistResponseJSONEncodedString(t *testing.T) {
	input := `"{\"message\":\"Котик прекрасен\",\"action\":{\"send_files\":[{\"type\":\"photo\",\"url\":\"https://example.test/cat.jpg\",\"file_name\":\"котик.jpg\"}]}}"`

	var response model.AssistResponse
	if err := unmarshalAssistResponse(input, &response); err != nil {
		t.Fatalf("unmarshalAssistResponse() error = %v", err)
	}
	if response.Message != "Котик прекрасен" {
		t.Fatalf("Message = %q, want %q", response.Message, "Котик прекрасен")
	}
	if len(response.Action.SendFiles) != 1 {
		t.Fatalf("SendFiles length = %d, want 1", len(response.Action.SendFiles))
	}
	if response.Action.SendFiles[0].FileName != "котик.jpg" {
		t.Fatalf("FileName = %q, want %q", response.Action.SendFiles[0].FileName, "котик.jpg")
	}
}

func TestUnmarshalAssistResponseMarkdown(t *testing.T) {
	var response model.AssistResponse
	if err := unmarshalAssistResponse("```json\n{\"message\":\"ok\"}\n```", &response); err != nil {
		t.Fatalf("unmarshalAssistResponse() error = %v", err)
	}
	if response.Message != "ok" {
		t.Fatalf("Message = %q, want %q", response.Message, "ok")
	}
}
