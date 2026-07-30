package mistral

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMistralRealtimeSTTSendsAudioAndReceivesFinalTranscript(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("model") != "voxtral-mini-transcribe-realtime-2602" {
			t.Errorf("unexpected model: %s", r.URL.Query().Get("model"))
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("missing authorization header")
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if !strings.Contains(string(message), "session.update") {
			t.Errorf("unexpected session message: %s", message)
		}
		_, message, err = conn.ReadMessage()
		if err != nil {
			return
		}
		if !strings.Contains(string(message), "input_audio.append") {
			t.Errorf("unexpected audio message: %s", message)
		}
		_ = conn.WriteJSON(map[string]any{"type": "transcription.delta", "delta": "привет"})
		_ = conn.WriteJSON(map[string]any{"type": "transcription.done", "transcript": "привет"})
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	transport, err := NewMistralRealtimeSTT(RealtimeSTTConfig{
		URL:    "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/audio/transcriptions/realtime",
		Model:  "voxtral-mini-transcribe-realtime-2602",
		APIKey: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	audio := make(chan []byte, 2)
	audio <- []byte{1, 2, 3}
	audio <- []byte{4, 5, 6}
	close(audio)
	got := make(chan string, 2)
	go func() {
		_ = transport.Run(ctx, audio, func(text string, final bool) error {
			got <- text + ":" + map[bool]string{true: "final", false: "interim"}[final]
			return nil
		})
	}()
	if value := <-got; value != "привет:interim" {
		t.Fatalf("unexpected interim transcript: %s", value)
	}
	if value := <-got; value != "привет:final" {
		t.Fatalf("unexpected final transcript: %s", value)
	}
}

func TestMistralRealtimeSTTTokenValidation(t *testing.T) {
	if _, err := NewMistralRealtimeSTT(RealtimeSTTConfig{Model: "m", APIKey: "a", RealtimeToken: "rt_b"}); err == nil {
		t.Fatal("expected mutually exclusive credential error")
	}
	if got := base64.StdEncoding.EncodeToString([]byte("audio")); got == "" {
		t.Fatal("base64 sanity check failed")
	}
}

func TestMistralRealtimeSTTReconnectsAfterUnexpectedClose(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if connections.Add(1) == 1 {
			// Abrupt network failure: no WebSocket close frame. This is the
			// condition reconnect is expected to handle.
			_ = conn.UnderlyingConn().Close()
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "transcription.done", "transcript": "после reconnect"})
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	transport, err := NewMistralRealtimeSTT(RealtimeSTTConfig{
		URL:               "ws" + strings.TrimPrefix(server.URL, "http") + "/realtime",
		Model:             "voxtral-mini-transcribe-realtime-2602",
		APIKey:            "secret",
		ReconnectAttempts: 1,
		ReconnectDelay:    time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	audio := make(chan []byte, 2)
	audio <- []byte{1, 2, 3}
	audio <- []byte{4, 5, 6}
	transcript := make(chan string, 1)
	go func() {
		_ = transport.Run(ctx, audio, func(text string, final bool) error {
			if final {
				transcript <- text
			}
			return nil
		})
	}()
	select {
	case got := <-transcript:
		if got != "после reconnect" {
			t.Fatalf("unexpected transcript: %q", got)
		}
	case <-ctx.Done():
		t.Fatal("transport did not reconnect")
	}
}

func requireMistralRealtimeIntegration(t *testing.T) string {
	t.Helper()
	apiKey := os.Getenv("MISTRAL_API_KEY")
	if apiKey == "" {
		t.Skip("MISTRAL_API_KEY не задан в окружении")
	}
	return apiKey
}

func TestMistralRealtimeMintClientSessionIntegration(t *testing.T) {
	apiKey := requireMistralRealtimeIntegration(t)
	client := NewMistralAgentClient(context.Background())
	client.apiKey = apiKey

	result, err := client.MintRealtimeToken(context.Background(), 0, "voxtral-mini-transcribe-realtime-2602")
	if err != nil {
		t.Fatalf("MintRealtimeToken() error: %v", err)
	}
	if !strings.HasPrefix(result.ClientSecret.Value, "rt_") {
		t.Fatalf("unexpected realtime token: %q", result.ClientSecret.Value)
	}
}

func TestMistralRealtimeWebSocketIntegration(t *testing.T) {
	apiKey := requireMistralRealtimeIntegration(t)
	transport, err := NewMistralRealtimeSTT(RealtimeSTTConfig{
		Model:             "voxtral-mini-transcribe-realtime-2602",
		APIKey:            apiKey,
		ReconnectAttempts: 0,
		ReadTimeout:       15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	audio := make(chan []byte, 1)
	// Synthetic PCM16 mono data. The test verifies transport/authentication;
	// it intentionally does not assert a particular transcription.
	audio <- make([]byte, 3200)
	transcripts := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- transport.Run(ctx, audio, func(text string, final bool) error {
			if text != "" {
				select {
				case transcripts <- text:
				default:
				}
			}
			return nil
		})
	}()

	select {
	case <-transcripts:
		// A transcript is optional for synthetic silence; receiving one proves
		// that the server accepted and processed the stream.
	case <-time.After(5 * time.Second):
		// Keep the test focused on connection and write path. The server may
		// legitimately emit no transcript for silence.
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("realtime WebSocket transport error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("realtime WebSocket transport did not stop after cancellation")
	}
}

func TestMistralRealtimeSpeechAudioIntegration(t *testing.T) {
	apiKey := requireMistralRealtimeIntegration(t)
	fileName := os.Getenv("MISTRAL_REALTIME_AUDIO_FILE")
	if fileName == "" {
		t.Skip("MISTRAL_REALTIME_AUDIO_FILE не задан")
	}
	audio, err := os.ReadFile(fileName)
	if err != nil {
		t.Fatalf("чтение MISTRAL_REALTIME_AUDIO_FILE: %v", err)
	}
	transport, err := NewMistralRealtimeSTT(RealtimeSTTConfig{Model: "voxtral-mini-transcribe-realtime-2602", APIKey: apiKey, ReadTimeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	input := make(chan []byte, 8)
	const chunkSize = 3200
	final := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- transport.Run(ctx, input, func(text string, done bool) error {
			if done && strings.TrimSpace(text) != "" {
				final <- text
			}
			return nil
		})
	}()
	go func() {
		defer close(input)
		for offset := 0; offset < len(audio); offset += chunkSize {
			end := offset + chunkSize
			if end > len(audio) {
				end = len(audio)
			}
			select {
			case input <- audio[offset:end]:
			case <-ctx.Done():
				return
			}
		}
	}()
	select {
	case text := <-final:
		if strings.TrimSpace(text) == "" {
			t.Fatal("empty final transcript")
		}
	case err := <-errCh:
		if err != nil {
			t.Fatalf("realtime STT: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("final transcript was not received")
	}
}
