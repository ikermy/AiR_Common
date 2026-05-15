package create

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gorilla/websocket"
)

// IntOrInf хранит значение max_response_output_tokens: 0 → "inf", >0 → число.
// Используется в OpenAI Realtime API — поле принимает либо целое число, либо строку "inf".
type IntOrInf struct {
	Value int // 0 означает "inf"
}

func (v *IntOrInf) MarshalJSON() ([]byte, error) {
	if v.Value == 0 {
		return []byte(`"inf"`), nil
	}
	return json.Marshal(v.Value)
}

func (v *IntOrInf) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s == "inf" {
			v.Value = 0
			return nil
		}
		return fmt.Errorf("IntOrInf: неизвестная строка %q", s)
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("IntOrInf: ожидалось число или \"inf\": %w", err)
	}
	v.Value = n
	return nil
}

// DialRealtimeSession устанавливает WebSocket соединение к OpenAI Realtime API.
// Возвращает готовое *websocket.Conn для отправки/приёма событий.
//
// Заголовки:
//   - Authorization: Bearer <apiKey>
//   - OpenAI-Beta: realtime=v1
func DialRealtimeSession(apiKey, model string) (*websocket.Conn, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("DialRealtimeSession: apiKey не может быть пустым")
	}
	if model == "" {
		model = RealtimeOpenAIModel
	}

	// Формируем URL с параметрами сессии
	baseURL, _ := url.Parse(RealtimeBaseURL)
	q := baseURL.Query()
	q.Set("model", model)
	q.Set("temperature", strconv.FormatFloat(RealtimeTemperature, 'f', 1, 64))
	q.Set("max_output_tokens", strconv.Itoa(RealtimeMaxOutTokens))
	baseURL.RawQuery = q.Encode()

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+apiKey)
	headers.Set("OpenAI-Beta", "realtime=v1")

	dialer := websocket.Dialer{
		// Используем стандартный TLS — OpenAI не требует кастомного
	}

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
