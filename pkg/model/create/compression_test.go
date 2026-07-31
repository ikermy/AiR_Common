package create

import (
	"testing"

	"github.com/ikermy/air_common/pkg/model/commdom"
)

func TestModelDataCompressionRoundTripPreservesRealtimeAndVectorIDs(t *testing.T) {
	data := &commdom.UniversalModelData{
		Name:         "mistral-voice",
		Provider:     commdom.ProviderMistral,
		Realtime:     true,
		RealtimeVAD:  &commdom.RealtimeVAD{Mistral: &commdom.MistralRealtimeVAD{}},
		UseModelName: &commdom.UseModelName{GptType: &commdom.GptType{ID: 11}, Realtime: &commdom.Realtime{ID: 12}},
	}
	compressed, err := compressModelData(data)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := decompressModelData(compressed, &commdom.VecIds{FileIds: []commdom.Ids{{ID: "file-1"}}, VectorId: []string{"vec-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Name != data.Name || restored.Provider != commdom.ProviderMistral || len(restored.FileIds) != 1 || len(restored.VecIds.VectorId) != 1 {
		t.Fatalf("unexpected restored model: %+v", restored)
	}
	if restored.RealtimeVAD.Mistral.STTStream == nil || restored.RealtimeVAD.Mistral.TTSStream == nil || restored.RealtimeVAD.Mistral.SpeechFormat == nil {
		t.Fatal("Mistral realtime defaults were not applied")
	}
}
