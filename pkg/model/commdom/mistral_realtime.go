package commdom

import "fmt"

type MistralRealtimeVAD struct {
	STTModel         *string                  `json:"stt_model,omitempty"`
	TTSModel         *string                  `json:"tts_model,omitempty"`
	Voice            *string                  `json:"voice,omitempty"`
	VoiceID          *string                  `json:"voice_id,omitempty"`
	ReferenceAudioID *string                  `json:"reference_audio_id,omitempty"`
	VoiceClone       *MistralVoiceCloneConfig `json:"voice_clone,omitempty"`
	SpeechFormat     *string                  `json:"speech_format,omitempty"`
	STTLanguage      *string                  `json:"stt_language,omitempty"`
	STTStream        *bool                    `json:"stt_stream,omitempty"`
	TTSStream        *bool                    `json:"tts_stream,omitempty"`
}
type MistralVoiceCloneConfig struct {
	Enabled             bool   `json:"enabled"`
	ProfileID           string `json:"profile_id,omitempty"`
	ReferenceAudioID    string `json:"reference_audio_id,omitempty"`
	ReferenceFormat     string `json:"reference_format,omitempty"`
	ReferenceDurationMs int    `json:"reference_duration_ms,omitempty"`
}
type MistralVoiceProfile struct {
	ID                  string `json:"id"`
	Name                string `json:"name,omitempty"`
	TTSModel            string `json:"tts_model,omitempty"`
	ReferenceAudioID    string `json:"reference_audio_id,omitempty"`
	ReferenceFormat     string `json:"reference_format,omitempty"`
	ReferenceDurationMs int    `json:"reference_duration_ms,omitempty"`
}

func (v *MistralVoiceCloneConfig) Validate() error {
	if v == nil || !v.Enabled {
		return nil
	}
	if v.ProfileID == "" && v.ReferenceAudioID == "" {
		return fmt.Errorf("для voice cloning не задан profile_id или reference_audio_id")
	}
	if v.ProfileID != "" && v.ReferenceAudioID != "" {
		return fmt.Errorf("нельзя одновременно задавать profile_id и reference_audio_id")
	}
	if v.ReferenceDurationMs != 0 && (v.ReferenceDurationMs < 2000 || v.ReferenceDurationMs > 10000) {
		return fmt.Errorf("длительность reference audio должна быть от 2000 до 10000 мс")
	}
	return nil
}
