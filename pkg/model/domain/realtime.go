package domain

import (
	"encoding/json"
	"fmt"
)

type IntOrInf struct{ Value int }

func (v *IntOrInf) MarshalJSON() ([]byte, error) {
	if v.Value == 0 {
		return []byte(`"inf"`), nil
	}
	return json.Marshal(v.Value)
}
func (v *IntOrInf) UnmarshalJSON(data []byte) error {
	var s string
	if json.Unmarshal(data, &s) == nil {
		if s == "inf" {
			v.Value = 0
			return nil
		}
		return fmt.Errorf("IntOrInf: неизвестная строка %q", s)
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("IntOrInf: ожидалось число или inf: %w", err)
	}
	v.Value = n
	return nil
}

type GoogleRealtimeVAD struct {
	VoiceName                  *string `json:"voice_name,omitempty"`
	LanguageCode               *string `json:"language_code,omitempty"`
	InputAudioTranscription    *bool   `json:"input_audio_transcription,omitempty"`
	OutputAudioTranscription   *bool   `json:"output_audio_transcription,omitempty"`
	AutomaticActivityDetection *bool   `json:"automatic_activity_detection,omitempty"`
	BargeIn                    *bool   `json:"barge_in,omitempty"`
	SilenceDurationMs          *int    `json:"silence_duration_ms,omitempty"`
}
type RealtimeVAD struct {
	SilenceDurationMs       *int                `json:"silence_duration_ms,omitempty"`
	InterruptResponse       *bool               `json:"interrupt_response,omitempty"`
	Temperature             *float64            `json:"temperature,omitempty"`
	InputAudioTranscription *bool               `json:"input_audio_transcription,omitempty"`
	InitialGreeting         *bool               `json:"initial_greeting,omitempty"`
	Greeting                *string             `json:"greeting,omitempty"`
	Voice                   *string             `json:"voice,omitempty"`
	Threshold               *float64            `json:"threshold,omitempty"`
	PrefixPaddingMs         *int                `json:"prefix_padding_ms,omitempty"`
	MaxResponseOutputTokens *IntOrInf           `json:"max_response_output_tokens,omitempty"`
	Google                  *GoogleRealtimeVAD  `json:"google,omitempty"`
	Mistral                 *MistralRealtimeVAD `json:"mistral,omitempty"`
}
