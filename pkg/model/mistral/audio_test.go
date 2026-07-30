package mistral

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAudioNormalizerPassesRawPCM(t *testing.T) {
	var normalizer AudioNormalizer
	got := normalizer.Push([]byte("raw-pcm-audio"))
	if string(got) != "raw-pcm-audio" {
		t.Fatalf("AudioNormalizer raw output = %q", got)
	}
}

func TestAudioNormalizerStripsWAVHeaderAcrossChunks(t *testing.T) {
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], 4)

	var normalizer AudioNormalizer
	if got := normalizer.Push(header[:10]); got != nil {
		t.Fatalf("partial WAV header produced output: %q", got)
	}
	got := normalizer.Push(append(header[10:], []byte("pcm1")...))
	if string(got) != "pcm1" {
		t.Fatalf("normalized WAV output = %q, want pcm1", got)
	}
	if got := normalizer.Push([]byte("pcm2")); string(got) != "pcm2" {
		t.Fatalf("post-header output = %q, want pcm2", got)
	}
}

type audioRoundTripper func(*http.Request) (*http.Response, error)

func (f audioRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testAudioClient(handler audioRoundTripper) *MistralAgentClient {
	client := NewMistralAgentClient(context.Background())
	client.httpClient = &http.Client{Transport: handler}
	client.SetKeyResolver(func(uint32) string { return "test-key" })
	return client
}

func TestTranscribeAudioBuildsMultipartRequest(t *testing.T) {
	client := testAudioClient(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/audio/transcriptions") {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing authorization header")
		}
		if err := req.ParseMultipartForm(1024 * 1024); err != nil {
			t.Fatalf("ParseMultipartForm() error: %v", err)
		}
		if req.FormValue("model") != "voxtral-mini-transcribe-realtime-2602" {
			t.Fatalf("unexpected model: %q", req.FormValue("model"))
		}
		if req.FormValue("fileName") != "audio.pcm" {
			t.Fatalf("unexpected fileName: %q", req.FormValue("fileName"))
		}
		file, _, err := req.FormFile("file")
		if err != nil {
			t.Fatalf("missing file: %v", err)
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		if string(data) != "pcm" {
			t.Fatalf("unexpected audio: %q", data)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"text":"hello"}`))}, nil
	})

	text, err := client.TranscribeAudio(context.Background(), 42, "voxtral-mini-transcribe-realtime-2602", "en", "audio.pcm", []byte("pcm"))
	if err != nil {
		t.Fatalf("TranscribeAudio() error: %v", err)
	}
	if text != "hello" {
		t.Fatalf("TranscribeAudio() = %q, want hello", text)
	}
}

func TestSpeechForcesStreamingAndReturnsBody(t *testing.T) {
	client := testAudioClient(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			t.Fatalf("speech request is not streaming: %s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"audio/wav"}},
			Body:       io.NopCloser(strings.NewReader("audio")),
		}, nil
	})

	body, contentType, err := client.Speech(context.Background(), 42, SpeechRequest{
		Model: "voxtral-mini-tts-2603",
		Input: "Hello",
		Voice: "default",
	})
	if err != nil {
		t.Fatalf("Speech() error: %v", err)
	}
	defer body.Close()
	data, _ := io.ReadAll(body)
	if string(data) != "audio" || contentType != "audio/wav" {
		t.Fatalf("Speech() = %q, %q", data, contentType)
	}
}

func TestVoicesParsesDataResponse(t *testing.T) {
	client := testAudioClient(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"voice-1","name":"Test"}]}`)),
		}, nil
	})

	voices, err := client.Voices(context.Background(), 42)
	if err != nil {
		t.Fatalf("Voices() error: %v", err)
	}
	if len(voices) != 1 || voices[0].ID != "voice-1" {
		t.Fatalf("Voices() = %+v", voices)
	}
}

func TestListVoicesParsesPaginatedItems(t *testing.T) {
	client := testAudioClient(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/audio/voices") {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
		}
		if req.URL.Query().Get("limit") != "10" || req.URL.Query().Get("offset") != "20" || req.URL.Query().Get("type") != "custom" {
			t.Fatalf("unexpected pagination: %s", req.URL.RawQuery)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"items":[{"id":"v1","name":"Clone","retention_notice":30}],"page":3,"page_size":10,"total":21,"total_pages":3}`))}, nil
	})
	result, err := client.ListVoices(context.Background(), 42, 10, 20, "custom")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "v1" || result.Total != 21 || result.Page != 3 {
		t.Fatalf("unexpected voice list: %+v", result)
	}
}

func TestStreamSpeechToSessionRejectsLateTurn(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()
	turn := session.BeginTurn()
	session.Interrupt()

	body := io.NopCloser(strings.NewReader("late audio"))
	if err := StreamSpeechToSession(context.Background(), session, turn, body); err != nil {
		t.Fatalf("StreamSpeechToSession() error: %v", err)
	}
	if len(session.AudioOut) != 0 {
		t.Fatal("late audio must not be published")
	}
}

func TestStreamSpeechToSessionPublishesRawAudio(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()
	turn := session.BeginTurn()

	if err := StreamSpeechToSession(context.Background(), session, turn, io.NopCloser(strings.NewReader("audio"))); err != nil {
		t.Fatalf("StreamSpeechToSession() error: %v", err)
	}
	if len(session.AudioOut) != 1 {
		t.Fatalf("AudioOut length = %d, want 1", len(session.AudioOut))
	}
}

func TestStreamSpeechSSEToSessionDecodesAudioData(t *testing.T) {
	session := NewRealtimeSession(context.Background(), 1, 2, 3)
	defer session.Close()
	turn := session.BeginTurn()
	body := io.NopCloser(strings.NewReader("data: {\"audio_data\":\"AQID\"}\n\ndata: [DONE]\n\n"))
	if err := StreamSpeechSSEToSession(context.Background(), session, turn, body); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-session.AudioOut:
		if string(got) != string([]byte{1, 2, 3}) {
			t.Fatalf("decoded audio = %v", got)
		}
	default:
		t.Fatal("expected decoded audio chunk")
	}
}

func TestStreamTranscribeAudioParsesSSE(t *testing.T) {
	client := testAudioClient(func(req *http.Request) (*http.Response, error) {
		if req.FormValue("stream") != "true" {
			t.Fatalf("stream flag was not sent")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("data: {\"event\":\"transcription.text.delta\",\"data\":{\"text\":\"hello\"}}\n\ndata: [DONE]\n"))}, nil
	})
	var events []TranscriptionStreamEvent
	err := client.StreamTranscribeAudio(context.Background(), 42, "voxtral-mini-latest", "en", "audio.pcm", []byte("pcm"), func(event TranscriptionStreamEvent) error { events = append(events, event); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Data.Text != "hello" {
		t.Fatalf("unexpected events: %+v", events)
	}
}
