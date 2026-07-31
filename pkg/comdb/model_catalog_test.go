package comdb

import "testing"

func TestProviderModelName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"voxtral-mini-tts-2603", "voxtral-mini-tts-2603"},
		{"voxtral-mini-transcribe-realtime-2602", "voxtral-mini-transcribe-realtime-2602"},
		{"mistral-medium-latest", "mistral-medium-latest"},
	}
	for _, tt := range tests {
		if tt.name != tt.want {
			t.Errorf("name = %q, want %q", tt.name, tt.want)
		}
	}
}
