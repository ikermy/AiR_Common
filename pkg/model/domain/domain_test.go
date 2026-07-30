package domain

import (
	"encoding/json"
	"testing"
)

func TestUniversalModelDataJSONCompatibility(t *testing.T) {
	data := UniversalModelData{
		Name:     "voice-model",
		Realtime: true,
		RealtimeVAD: &RealtimeVAD{
			Mistral: &MistralRealtimeVAD{
				STTModel:   str("voxtral-mini-transcribe-realtime-2602"),
				VoiceClone: &MistralVoiceCloneConfig{Enabled: true, ProfileID: "voice-1"},
			},
		},
		UseModelName: &UseModelName{GptType: &GptType{Name: "mistral-large", ID: 1}, Realtime: &Realtime{Name: "voxtral", ID: 2}},
		Provider:     ProviderMistral,
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var decoded UniversalModelData
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Provider != ProviderMistral || decoded.UseModelName.GptType.ID != 1 || decoded.RealtimeVAD.Mistral.VoiceClone.ProfileID != "voice-1" {
		t.Fatalf("unexpected decoded data: %+v", decoded)
	}
}

func TestProviderAndModelTypeParsing(t *testing.T) {
	provider, err := FromString("mistral")
	if err != nil || provider != ProviderMistral || provider.String() != "mistral" {
		t.Fatalf("unexpected provider: %v", provider)
	}
	modelType, err := ModelTypeFromString("realtime")
	if err != nil || !modelType.IsRealtime() || modelType.IsGeneral() {
		t.Fatalf("unexpected model type: %v", modelType)
	}
}

func str(value string) *string { return &value }
