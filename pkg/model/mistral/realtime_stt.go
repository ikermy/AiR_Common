package mistral

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const defaultMistralRealtimeSTTURL = "wss://api.mistral.ai/v1/audio/transcriptions/realtime"

// RealtimeSTTConfig configures the official Mistral Voxtral realtime
// transcription WebSocket. APIKey is intended for a server-side connection;
// RealtimeToken is the short-lived rt_* credential intended for browser
// connections. Exactly one credential must be supplied.
type RealtimeSTTConfig struct {
	URL               string
	Model             string
	APIKey            string
	RealtimeToken     string
	Dialer            *websocket.Dialer
	Header            http.Header
	PingInterval      time.Duration
	ReadTimeout       time.Duration
	ReconnectAttempts int
	ReconnectDelay    time.Duration
	AudioEncoding     string
	SampleRate        int
}

// MistralRealtimeSTT implements STTTransport using one long-lived WebSocket.
// It deliberately contains no provider-specific session state: the session
// coordinator remains responsible for turns, cancellation and callbacks.
type MistralRealtimeSTT struct {
	config RealtimeSTTConfig
}

func NewMistralRealtimeSTT(config RealtimeSTTConfig) (*MistralRealtimeSTT, error) {
	if strings.TrimSpace(config.Model) == "" {
		return nil, fmt.Errorf("не задана realtime STT-модель Mistral")
	}
	if config.APIKey == "" && config.RealtimeToken == "" {
		return nil, fmt.Errorf("не задан API key или realtime token Mistral")
	}
	if config.APIKey != "" && config.RealtimeToken != "" {
		return nil, fmt.Errorf("нельзя одновременно использовать API key и realtime token")
	}
	if config.URL == "" {
		config.URL = defaultMistralRealtimeSTTURL
	}
	if config.PingInterval <= 0 {
		config.PingInterval = 20 * time.Second
	}
	if config.ReadTimeout <= 0 {
		config.ReadTimeout = config.PingInterval * 2
	}
	if config.ReconnectAttempts < 0 {
		config.ReconnectAttempts = 0
	}
	if config.ReconnectDelay <= 0 {
		config.ReconnectDelay = 250 * time.Millisecond
	}
	if config.AudioEncoding == "" {
		config.AudioEncoding = "pcm_s16le"
	}
	if config.SampleRate <= 0 {
		config.SampleRate = 16000
	}
	if config.RealtimeToken != "" && !strings.HasPrefix(config.RealtimeToken, "rt_") {
		return nil, fmt.Errorf("realtime token Mistral должен начинаться с rt_")
	}
	return &MistralRealtimeSTT{config: config}, nil
}

func (t *MistralRealtimeSTT) Run(ctx context.Context, audio <-chan []byte, onTranscript func(string, bool) error) error {
	var lastErr error
	for attempt := 0; ; attempt++ {
		err := t.runOnce(ctx, audio, onTranscript)
		if err == nil || ctx.Err() != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		lastErr = err
		if attempt >= t.config.ReconnectAttempts {
			return lastErr
		}
		timer := time.NewTimer(t.config.ReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (t *MistralRealtimeSTT) runOnce(ctx context.Context, audio <-chan []byte, onTranscript func(string, bool) error) error {
	if t == nil {
		return fmt.Errorf("Mistral realtime STT transport is nil")
	}
	if onTranscript == nil {
		return fmt.Errorf("не задан transcript callback")
	}

	endpoint, err := url.Parse(t.config.URL)
	if err != nil {
		return fmt.Errorf("некорректный Mistral realtime URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("model", t.config.Model)
	endpoint.RawQuery = query.Encode()

	header := t.config.Header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	if t.config.APIKey != "" {
		header.Set("Authorization", "Bearer "+t.config.APIKey)
	}
	dialer := websocket.DefaultDialer
	if t.config.Dialer != nil {
		copyDialer := *t.config.Dialer
		dialer = &copyDialer
	}
	if t.config.RealtimeToken != "" {
		// RFC 6455 represents the browser's ["realtime", token] value as
		// two subprotocols. Let gorilla construct the header correctly.
		dialer.Subprotocols = []string{"realtime", t.config.RealtimeToken}
	}
	conn, _, err := dialer.DialContext(ctx, endpoint.String(), header)
	if err != nil {
		return fmt.Errorf("подключение к Mistral realtime STT: %w", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(t.config.ReadTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(t.config.ReadTimeout))
	})

	var writeMu sync.Mutex
	writeJSON := func(value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(value)
	}
	if err := writeJSON(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"audio_format": map[string]any{
				"encoding":    t.config.AudioEncoding,
				"sample_rate": t.config.SampleRate,
			},
		},
	}); err != nil {
		return fmt.Errorf("отправка Mistral realtime session.update: %w", err)
	}

	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(t.config.ReadTimeout))
			var event realtimeSTTEvent
			if err := json.Unmarshal(data, &event); err != nil {
				readErr <- fmt.Errorf("разбор Mistral realtime STT-события: %w", err)
				return
			}
			if event.Error != nil {
				readErr <- fmt.Errorf("Mistral realtime STT: %s", event.Error.Message)
				return
			}
			if event.Text == "" {
				continue
			}
			final := isFinalRealtimeSTTEvent(event.Type)
			if err := onTranscript(event.Text, final); err != nil {
				readErr <- err
				return
			}
		}
	}()

	ping := time.NewTicker(t.config.PingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "context canceled"),
				time.Now().Add(time.Second))
			return ctx.Err()
		case err := <-readErr:
			if closeErr, ok := err.(*websocket.CloseError); ok && closeErr.Code == websocket.CloseNormalClosure {
				return nil
			}
			return err
		case <-ping.C:
			writeMu.Lock()
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second))
			writeMu.Unlock()
			if err != nil {
				return fmt.Errorf("Mistral realtime STT ping: %w", err)
			}
		case chunk, ok := <-audio:
			if !ok {
				_ = writeJSON(map[string]any{"type": "input_audio.flush"})
				_ = writeJSON(map[string]any{"type": "input_audio.end"})
				// Keep the socket open: Mistral sends the final transcript after
				// processing the end event. Disable further audio reads while
				// waiting for that response or context cancellation.
				audio = nil
				continue
			}
			if len(chunk) == 0 {
				continue
			}
			message := map[string]any{
				"type":  "input_audio.append",
				"audio": base64.StdEncoding.EncodeToString(chunk),
			}
			if err := writeJSON(message); err != nil {
				return fmt.Errorf("отправка аудио в Mistral realtime STT: %w", err)
			}
		}
	}
}

type realtimeSTTEvent struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Delta string `json:"delta"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (e *realtimeSTTEvent) UnmarshalJSON(data []byte) error {
	type alias realtimeSTTEvent
	var value struct {
		alias
		Transcript string `json:"transcript"`
		TextDelta  string `json:"text_delta"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*e = realtimeSTTEvent(value.alias)
	if e.Text == "" {
		e.Text = value.Transcript
	}
	if e.Text == "" {
		e.Text = value.Delta
	}
	if e.Text == "" {
		e.Text = value.TextDelta
	}
	return nil
}

func isFinalRealtimeSTTEvent(eventType string) bool {
	eventType = strings.ToLower(eventType)
	return strings.Contains(eventType, "completed") ||
		strings.Contains(eventType, "final") ||
		strings.HasSuffix(eventType, ".done") ||
		eventType == "transcription.done"
}
