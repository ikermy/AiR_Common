package comdb

import (
	"testing"

	"github.com/ikermy/AiR_Common/pkg/model/create"
)

func TestProviderModelCategory(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"voxtral-mini-tts-2603", create.ProviderModelTTS},
		{"voxtral-mini-transcribe-realtime-2602", create.ProviderModelSTT},
		{"mistral-medium-latest", create.ProviderModelGeneral},
	}
	for _, tt := range tests {
		if got := providerModelCategory(tt.name, create.ProviderModelGeneral); got != tt.want {
			t.Errorf("providerModelCategory(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}
