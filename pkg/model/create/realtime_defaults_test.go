package create

import "testing"

func TestApplyRealtimeVADDefaultsMistral(t *testing.T) {
	vad := applyRealtimeVADDefaults(&RealtimeVAD{Mistral: &MistralRealtimeVAD{}})
	if vad.Mistral == nil || vad.Mistral.STTStream == nil || !*vad.Mistral.STTStream {
		t.Fatal("Mistral STT stream should default to true")
	}
	if vad.Mistral.TTSStream == nil || !*vad.Mistral.TTSStream {
		t.Fatal("Mistral TTS stream should default to true")
	}
	if vad.Mistral.SpeechFormat == nil || *vad.Mistral.SpeechFormat != "wav" {
		t.Fatalf("Mistral speech format = %v, want wav", vad.Mistral.SpeechFormat)
	}
}

func TestApplyRealtimeVADDefaultsPreservesMistralValues(t *testing.T) {
	stream := false
	format := "mp3"
	vad := applyRealtimeVADDefaults(&RealtimeVAD{Mistral: &MistralRealtimeVAD{
		STTStream:    &stream,
		TTSStream:    &stream,
		SpeechFormat: &format,
	}})
	if *vad.Mistral.STTStream || *vad.Mistral.TTSStream || *vad.Mistral.SpeechFormat != "mp3" {
		t.Fatal("explicit Mistral values were overwritten")
	}
}
