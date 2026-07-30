package create

import "github.com/ikermy/AiR_Common/pkg/model/domain"

// applyRealtimeVADDefaults применяет дефолтные значения к RealtimeVAD и вложенному GoogleRealtimeVAD.
//
// Общие дефолты: SilenceDurationMs=500, InterruptResponse=true,
// InputAudioTranscription=true, InitialGreeting=true.
//
// OpenAI-специфичные дефолты: Threshold=0.5, PrefixPaddingMs=200.
//
// Google-специфичные дефолты (Google-блок): VoiceName="Puck",
// InputAudioTranscription=true, OutputAudioTranscription=false,
// AutomaticActivityDetection=true, BargeIn=true, SilenceDurationMs=500.
// Mistral-специфичные дефолты: STTStream=true, TTSStream=true,
// SpeechFormat="wav".
func applyRealtimeVADDefaults(vad *domain.RealtimeVAD) *domain.RealtimeVAD {
	if vad == nil {
		return nil
	}

	// ── Общие параметры ──────────────────────────────────────────────────────

	// SilenceDurationMs: дефолт 500
	if vad.SilenceDurationMs == nil {
		v := 500
		vad.SilenceDurationMs = &v
	}

	// InterruptResponse: дефолт true
	if vad.InterruptResponse == nil {
		v := true
		vad.InterruptResponse = &v
	}

	// InputAudioTranscription: дефолт true
	if vad.InputAudioTranscription == nil {
		v := true
		vad.InputAudioTranscription = &v
	}

	// InitialGreeting: дефолт true
	if vad.InitialGreeting == nil {
		v := true
		vad.InitialGreeting = &v
	}

	// ── OpenAI-специфичные дефолты ───────────────────────────────────────────

	// Threshold: дефолт 0.5
	if vad.Threshold == nil {
		v := 0.5
		vad.Threshold = &v
	}

	// PrefixPaddingMs: дефолт 200
	if vad.PrefixPaddingMs == nil {
		v := 200
		vad.PrefixPaddingMs = &v
	}

	// ── Google-специфичные дефолты ───────────────────────────────────────────
	if vad.Google != nil {
		g := vad.Google

		// VoiceName: дефолт "Puck"
		if g.VoiceName == nil {
			v := GoogleRealtimeDefaultVoice
			g.VoiceName = &v
		}

		// InputAudioTranscription: дефолт true
		if g.InputAudioTranscription == nil {
			v := true
			g.InputAudioTranscription = &v
		}

		// OutputAudioTranscription: дефолт false
		if g.OutputAudioTranscription == nil {
			v := false
			g.OutputAudioTranscription = &v
		}

		// AutomaticActivityDetection: дефолт true
		if g.AutomaticActivityDetection == nil {
			v := true
			g.AutomaticActivityDetection = &v
		}

		// BargeIn: дефолт true
		if g.BargeIn == nil {
			v := true
			g.BargeIn = &v
		}

		// SilenceDurationMs: дефолт 500
		if g.SilenceDurationMs == nil {
			v := GoogleRealtimeSilenceDurationMs
			g.SilenceDurationMs = &v
		}
	}

	// ── Mistral-специфичные дефолты ─────────────────────────────────────────
	if vad.Mistral != nil {
		if vad.Mistral.STTStream == nil {
			v := true
			vad.Mistral.STTStream = &v
		}
		if vad.Mistral.TTSStream == nil {
			v := true
			vad.Mistral.TTSStream = &v
		}
		if vad.Mistral.SpeechFormat == nil {
			v := "wav"
			vad.Mistral.SpeechFormat = &v
		}
	}

	return vad
}
