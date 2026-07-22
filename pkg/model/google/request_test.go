package google

import (
	"testing"

	"github.com/ikermy/AiR_Common/pkg/model"
)

func TestUnmarshalGoogleAssistResponseJSONEncodedString(t *testing.T) {
	input := `"{\"message\":\"Котик прекрасен\",\"action\":{\"send_files\":[{\"type\":\"photo\",\"url\":\"https://example.test/cat.jpg\",\"file_name\":\"котик.jpg\"}]}}"`
	var response model.AssistResponse
	if err := unmarshalGoogleAssistResponse(input, &response); err != nil {
		t.Fatal(err)
	}
	if response.Message != "Котик прекрасен" || len(response.Action.SendFiles) != 1 {
		t.Fatalf("unexpected response: %+v", response)
	}
}
