package create

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/gorilla/websocket"
)

// ============================================================================
// GOOGLE MULTIMODAL LIVE API
// ============================================================================

const (
	// GoogleRealtimeDefaultVoice голос по умолчанию для Google Live API
	GoogleRealtimeDefaultVoice = "Puck"

	// GoogleRealtimeSilenceDurationMs пауза ожидания после окончания речи пользователя (мс)
	GoogleRealtimeSilenceDurationMs = 500

	// GoogleRealtimeInputSampleRate частота дискретизации входящего аудио (Гц) — PCM16 mono
	GoogleRealtimeInputSampleRate = 16000

	// GoogleRealtimeOutputSampleRate частота дискретизации исходящего аудио (Гц) — PCM16 mono
	// ВАЖНО: Google Live API всегда возвращает аудио с частотой 24 kHz, изменить нельзя.
	GoogleRealtimeOutputSampleRate = 24000
)

// DialGoogleRealtimeSession устанавливает WebSocket соединение к Google Multimodal Live API.
// Возвращает готовое *websocket.Conn. После установки соединения необходимо отправить setup-сообщение.
func DialGoogleRealtimeSession(apiKey string) (*websocket.Conn, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("DialGoogleRealtimeSession: apiKey не может быть пустым")
	}

	// API key передаётся как заголовок x-goog-api-key (НЕ ?key= в URL!)
	// Источник: googleapis/go-genai live.go → header.Set("x-goog-api-key", apiKey)
	headers := http.Header{}
	headers.Set("x-goog-api-key", apiKey)

	dialer := websocket.Dialer{}
	conn, resp, err := dialer.Dial(RealtimeGoogleURL, headers)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("DialGoogleRealtimeSession: ошибка подключения (HTTP %d): %w",
				resp.StatusCode, err)
		}
		return nil, fmt.Errorf("DialGoogleRealtimeSession: ошибка подключения к %s: %w", RealtimeGoogleURL, err)
	}

	return conn, nil
}

// DialRealtimeSession устанавливает WebSocket соединение к OpenAI Realtime GA API.
// Возвращает готовое *websocket.Conn для отправки/приёма событий.
// Голос, VAD и прочие настройки передаются через session.update после подключения.
func DialRealtimeSession(apiKey, model string) (*websocket.Conn, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("DialRealtimeSession: apiKey не может быть пустым")
	}
	if model == "" {
		return nil, fmt.Errorf("DialRealtimeSession: не указана модель")
	}

	baseURL, _ := url.Parse(RealtimeOpenAIURL)
	q := baseURL.Query()
	q.Set("model", model)
	baseURL.RawQuery = q.Encode()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+apiKey)

	dialer := websocket.Dialer{}

	conn, resp, err := dialer.Dial(baseURL.String(), headers)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("DialRealtimeSession: ошибка подключения к %s (HTTP %d): %w",
				baseURL.String(), resp.StatusCode, err)
		}
		return nil, fmt.Errorf("DialRealtimeSession: ошибка подключения к %s: %w", baseURL.String(), err)
	}

	return conn, nil
}
