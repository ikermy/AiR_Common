package create

import (
	"encoding/json"
	"testing"
)

func TestMistralVoiceCloneConfigValidate(t *testing.T) {
	if err := (&MistralVoiceCloneConfig{Enabled: true}).Validate(); err == nil {
		t.Fatal("expected missing voice reference error")
	}
	if err := (&MistralVoiceCloneConfig{Enabled: true, ProfileID: "profile", ReferenceAudioID: "audio"}).Validate(); err == nil {
		t.Fatal("expected mutually exclusive reference error")
	}
	if err := (&MistralVoiceCloneConfig{Enabled: true, ReferenceAudioID: "audio", ReferenceDurationMs: 1000}).Validate(); err == nil {
		t.Fatal("expected duration validation error")
	}
	if err := (&MistralVoiceCloneConfig{Enabled: true, ReferenceAudioID: "audio", ReferenceDurationMs: 3000}).Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestMistralRealtimeVoiceCloneJSONRoundTrip(t *testing.T) {
	ttsModel := "voxtral-mini-tts-2603"
	voiceID := "voice-123"
	referenceID := "audio-456"
	data := UniversalModelData{
		Realtime: true,
		RealtimeVAD: &RealtimeVAD{
			Mistral: &MistralRealtimeVAD{
				TTSModel:         &ttsModel,
				VoiceID:          &voiceID,
				ReferenceAudioID: &referenceID,
				VoiceClone: &MistralVoiceCloneConfig{
					Enabled:             true,
					ReferenceAudioID:    referenceID,
					ReferenceFormat:     "wav",
					ReferenceDurationMs: 3000,
				},
			},
		},
	}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	var decoded UniversalModelData
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}
	if decoded.RealtimeVAD == nil || decoded.RealtimeVAD.Mistral == nil || decoded.RealtimeVAD.Mistral.TTSModel == nil {
		t.Fatal("Mistral realtime configuration was lost")
	}
	if *decoded.RealtimeVAD.Mistral.TTSModel != ttsModel || *decoded.RealtimeVAD.Mistral.VoiceID != voiceID {
		t.Fatalf("decoded config = %+v", decoded.RealtimeVAD.Mistral)
	}
	if decoded.RealtimeVAD.Mistral.VoiceClone == nil || decoded.RealtimeVAD.Mistral.VoiceClone.ReferenceAudioID != referenceID {
		t.Fatal("voice clone configuration was lost")
	}
}
