package comdb

import (
	"testing"

	"github.com/ikermy/AiR_Common/pkg/model/domain"
)

func TestProviderModelCategory(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"voxtral-mini-tts-2603", domain.ProviderModelTTS},
		{"voxtral-mini-transcribe-realtime-2602", domain.ProviderModelSTT},
		{"mistral-medium-latest", domain.ProviderModelGeneral},
	}
	for _, tt := range tests {
		if got := providerModelCategory(tt.name, domain.ProviderModelGeneral); got != tt.want {
			t.Errorf("providerModelCategory(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}
