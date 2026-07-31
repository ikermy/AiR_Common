package create

import (
	"testing"

	"github.com/ikermy/air_common/pkg/model/domain"
)

func TestModelDataCompressionRoundTripPreservesRealtimeAndVectorIDs(t *testing.T) {
	data := &domain.UniversalModelData{
		Name:         "mistral-voice",
		Provider:     domain.ProviderMistral,
		Realtime:     true,
		RealtimeVAD:  &domain.RealtimeVAD{Mistral: &domain.MistralRealtimeVAD{}},
		UseModelName: &domain.UseModelName{GptType: &domain.GptType{ID: 11}, Realtime: &domain.Realtime{ID: 12}},
	}
	compressed, err := compressModelData(data)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := decompressModelData(compressed, &domain.VecIds{FileIds: []domain.Ids{{ID: "file-1"}}, VectorId: []string{"vec-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Name != data.Name || restored.Provider != domain.ProviderMistral || len(restored.FileIds) != 1 || len(restored.VecIds.VectorId) != 1 {
		t.Fatalf("unexpected restored model: %+v", restored)
	}
	if restored.RealtimeVAD.Mistral.STTStream == nil || restored.RealtimeVAD.Mistral.TTSStream == nil || restored.RealtimeVAD.Mistral.SpeechFormat == nil {
		t.Fatal("Mistral realtime defaults were not applied")
	}
}
