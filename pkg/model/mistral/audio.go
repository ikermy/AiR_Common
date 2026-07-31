package mistral

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/ikermy/air_common/pkg/mode"
	modeldomain "github.com/ikermy/air_common/pkg/model/commdom"
)

const (
	mistralTranscriptionPath  = "/audio/transcriptions"
	mistralSpeechPath         = "/audio/speech"
	mistralVoicesPath         = "/audio/voices"
	mistralClientSessionsPath = "/client/sessions"
)

// AudioTranscription contains a final transcription returned by Mistral STT.
type AudioTranscription struct {
	Text string `json:"text"`
}

type TranscriptionStreamEvent struct {
	Event string `json:"event"`
	Data  struct {
		Text string `json:"text"`
	} `json:"data"`
}

// MistralVoice describes a voice returned by the Mistral voices endpoint.
type MistralVoice = modeldomain.Voice
type VoiceList = modeldomain.VoiceList
type CreateVoiceRequest = modeldomain.CreateVoiceRequest
type UpdateVoiceRequest = modeldomain.UpdateVoiceRequest

// RealtimeClientSession is a short-lived credential for browser WebSocket
// clients. The API key must remain on the backend.
type RealtimeClientSession struct {
	Object       string `json:"object"`
	Purpose      string `json:"purpose"`
	ExpiresAt    string `json:"expires_at"`
	ClientSecret struct {
		Value     string `json:"value"`
		ExpiresAt string `json:"expires_at"`
	} `json:"client_secret"`
}

// MintRealtimeToken creates an rt_* token scoped to one transcription model.
func (m *MistralAgentClient) MintRealtimeToken(ctx context.Context, userID uint32, modelName string) (RealtimeClientSession, error) {
	if strings.TrimSpace(modelName) == "" {
		return RealtimeClientSession{}, fmt.Errorf("не задана realtime STT-модель Mistral")
	}
	body, err := json.Marshal(map[string]string{"purpose": "realtime", "model": modelName})
	if err != nil {
		return RealtimeClientSession{}, fmt.Errorf("сериализация realtime token: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.audioURL(mistralClientSessionsPath), bytes.NewReader(body))
	if err != nil {
		return RealtimeClientSession{}, err
	}
	m.setAudioHeaders(request, userID, "application/json")
	response, err := m.audioHTTPClient().Do(request)
	if err != nil {
		return RealtimeClientSession{}, fmt.Errorf("запрос realtime token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return RealtimeClientSession{}, fmt.Errorf("realtime token вернул %d: %s", response.StatusCode, string(data))
	}
	var result RealtimeClientSession
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return RealtimeClientSession{}, fmt.Errorf("разбор realtime token: %w", err)
	}
	if !strings.HasPrefix(result.ClientSecret.Value, "rt_") {
		return RealtimeClientSession{}, fmt.Errorf("Mistral вернул некорректный realtime token")
	}
	return result, nil
}

// SpeechRequest is the request sent to Voxtral TTS.
type SpeechRequest struct {
	Model          string `json:"model"`
	Input          string `json:"input"`
	Voice          string `json:"voice,omitempty"`
	VoiceID        string `json:"voice_id,omitempty"`
	ReferenceAudio string `json:"ref_audio,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	Stream         bool   `json:"stream"`
}

// AudioNormalizer removes a WAV container header from the beginning of a
// streaming response. Raw PCM and non-WAV streams pass through unchanged.
type AudioNormalizer struct {
	prefix []byte
	done   bool
}

// StreamSpeechToSession copies a streaming TTS response into AudioOut.
// Every chunk is guarded by turnID, so a late network response cannot leak
// audio after an interruption.
func StreamSpeechToSession(ctx context.Context, session *MistralRealtimeSession, turnID uint64, body io.ReadCloser) error {
	if session == nil || body == nil {
		return fmt.Errorf("не задана realtime-сессия или TTS body")
	}
	defer body.Close()

	normalizer := AudioNormalizer{}
	var pendingByte []byte
	var totalInput, totalPCM int
	loggedHeader := false
	publishPCM := func(chunk []byte) error {
		chunk = normalizeTTSFormat(chunk)
		chunk = normalizeTTSChannels(chunk)
		if len(chunk) == 0 {
			return nil
		}
		if len(pendingByte) > 0 {
			chunk = append(pendingByte, chunk...)
			pendingByte = nil
		}
		if len(chunk)%2 != 0 {
			pendingByte = append(pendingByte, chunk...)
			return nil
		}
		if len(chunk) > 0 && !session.PublishAudio(turnID, chunk) {
			if !session.IsCurrentTurn(turnID) {
				return nil
			}
			return fmt.Errorf("AudioOut переполнен или realtime-сессия закрыта")
		}
		totalPCM += len(chunk)
		return nil
	}
	buffer := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.Context().Done():
			return session.Context().Err()
		default:
		}

		n, err := body.Read(buffer)
		if n > 0 {
			totalInput += n
			if !loggedHeader {
				limit := n
				if limit > 16 {
					limit = 16
				}
				loggedHeader = true
			}
			chunk := normalizer.Push(buffer[:n])
			if err := publishPCM(chunk); err != nil {
				return err
			}
		}
		if err == io.EOF {
			if err := publishPCM(normalizer.Flush()); err != nil {
				return err
			}
			if len(pendingByte) != 0 {
				if err := session.PublishAudio(turnID, pendingByte); !err && session.IsCurrentTurn(turnID) {
					return fmt.Errorf("AudioOut переполнен при завершении TTS")
				}
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("чтение streaming TTS: %w", err)
		}
	}
}

// StreamSpeechSSEToSession decodes Mistral's streaming speech response. The
// API returns text/event-stream records whose data contains base64 audio_data.
func StreamSpeechSSEToSession(ctx context.Context, session *MistralRealtimeSession, turnID uint64, body io.ReadCloser) error {
	if session == nil || body == nil {
		return fmt.Errorf("не задана realtime-сессия или TTS body")
	}
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	normalizer := AudioNormalizer{}
	var pendingByte []byte
	var totalDecoded int
	loggedHeader := false
	publishPCM := func(audio []byte) error {
		audio = normalizeTTSFormat(audio)
		audio = normalizeTTSChannels(audio)
		if len(pendingByte) > 0 {
			audio = append(pendingByte, audio...)
			pendingByte = nil
		}
		if len(audio)%2 != 0 {
			pendingByte = append(pendingByte, audio...)
			return nil
		}
		if len(audio) > 0 && !session.PublishAudio(turnID, audio) && session.IsCurrentTurn(turnID) {
			return fmt.Errorf("AudioOut переполнен или realtime-сессия закрыта")
		}
		totalDecoded += len(audio)
		return nil
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == ':' {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			if err := publishPCM(normalizer.Flush()); err != nil {
				return err
			}
			if len(pendingByte) != 0 {
				if !session.PublishAudio(turnID, pendingByte) && session.IsCurrentTurn(turnID) {
					return fmt.Errorf("AudioOut переполнен при завершении TTS")
				}
			}
			return nil
		}
		var event struct {
			AudioData string `json:"audio_data"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("разбор streaming TTS SSE: %w", err)
		}
		if event.AudioData == "" {
			continue
		}
		audio, err := base64.StdEncoding.DecodeString(event.AudioData)
		if err != nil {
			return fmt.Errorf("декодирование streaming TTS audio_data: %w", err)
		}
		if !loggedHeader {
			limit := len(audio)
			if limit > 16 {
				limit = 16
			}
			loggedHeader = true
		}
		totalDecoded += len(audio)

		if err := publishPCM(normalizer.Push(audio)); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("чтение streaming TTS SSE: %w", err)
	}
	if len(pendingByte) != 0 {
		if !session.PublishAudio(turnID, pendingByte) && session.IsCurrentTurn(turnID) {
			return fmt.Errorf("AudioOut переполнен при завершении TTS")
		}
	}
	return nil
}

func normalizeTTSFormat(data []byte) []byte {
	if len(data) < 16 || len(data)%4 != 0 {
		return data
	}
	checked, valid := 0, 0
	for i := 0; i+3 < len(data) && checked < 2048; i, checked = i+4, checked+1 {
		v := math.Float32frombits(binary.LittleEndian.Uint32(data[i : i+4]))
		if !math.IsNaN(float64(v)) && !math.IsInf(float64(v), 0) && math.Abs(float64(v)) <= 1.0 {
			valid++
		}
	}
	if checked == 0 || valid*100/checked < 90 {
		return data
	}
	out := make([]byte, (len(data)/4)*2)
	for i := 0; i < len(data); i += 4 {
		v := math.Float32frombits(binary.LittleEndian.Uint32(data[i : i+4]))
		if v > 1 {
			v = 1
		}
		if v < -1 {
			v = -1
		}
		value := int16(v * 32767)
		binary.LittleEndian.PutUint16(out[(i/4)*2:], uint16(value))
	}
	return out
}

// normalizeTTSChannels converts the stereo PCM16 shape currently returned by
// Voxtral TTS (one silent channel) to the mono stream expected by the
// realtime frontend. If both candidate channels contain meaningful signal,
// the bytes are left untouched because the input may already be mono PCM.
func normalizeTTSChannels(pcm []byte) []byte {
	if len(pcm) < 16 || len(pcm)%4 != 0 {
		return pcm
	}
	var leftEnergy, rightEnergy int64
	frames := len(pcm) / 4
	for i := 0; i < frames; i++ {
		left := int16(uint16(pcm[i*4]) | uint16(pcm[i*4+1])<<8)
		right := int16(uint16(pcm[i*4+2]) | uint16(pcm[i*4+3])<<8)
		if left < 0 {
			leftEnergy -= int64(left)
		} else {
			leftEnergy += int64(left)
		}
		if right < 0 {
			rightEnergy -= int64(right)
		} else {
			rightEnergy += int64(right)
		}
	}
	if leftEnergy == 0 && rightEnergy == 0 {
		return pcm
	}
	activeRight := leftEnergy*20 < rightEnergy
	activeLeft := rightEnergy*20 < leftEnergy
	if !activeLeft && !activeRight {
		return pcm
	}
	out := make([]byte, frames*2)
	for i := 0; i < frames; i++ {
		if activeRight {
			out[i*2] = pcm[i*4+2]
			out[i*2+1] = pcm[i*4+3]
		} else {
			out[i*2] = pcm[i*4]
			out[i*2+1] = pcm[i*4+1]
		}
	}
	return out
}

func minAudioLogBytes(n int) int {
	if n > 16 {
		return 16
	}
	return n
}

// Push normalizes one incoming audio chunk. The returned bytes are owned by
// the caller and can be sent directly to the client's AudioOut channel.
func (n *AudioNormalizer) Push(chunk []byte) []byte {
	if n.done {
		return chunk
	}
	n.prefix = append(n.prefix, chunk...)
	if len(n.prefix) < 12 {
		return nil
	}
	if string(n.prefix[:4]) != "RIFF" || string(n.prefix[8:12]) != "WAVE" {
		n.done = true
		out := n.prefix
		n.prefix = nil
		return out
	}

	// Parse RIFF chunks until the data chunk. This handles optional LIST,
	// JUNK and fact chunks instead of assuming a fixed 44-byte header.
	pos := 12
	for pos+8 <= len(n.prefix) {
		chunkID := string(n.prefix[pos : pos+4])
		chunkSize := int(binary.LittleEndian.Uint32(n.prefix[pos+4 : pos+8]))
		dataStart := pos + 8
		if chunkID == "data" {
			if len(n.prefix) < dataStart {
				return nil
			}
			n.done = true
			out := append([]byte(nil), n.prefix[dataStart:]...)
			n.prefix = nil
			return out
		}
		if chunkSize < 0 || dataStart+chunkSize > len(n.prefix) {
			return nil
		}
		pos = dataStart + chunkSize
		if pos%2 != 0 {
			pos++
		}
	}
	return nil
}

// Flush returns buffered bytes when the stream ends before a complete WAV
// header is received. It prevents silent data loss on truncated responses.
func (n *AudioNormalizer) Flush() []byte {
	if len(n.prefix) == 0 {
		return nil
	}
	out := append([]byte(nil), n.prefix...)
	n.prefix = nil
	n.done = true
	return out
}

// TranscribeAudio sends one audio segment to the Mistral transcription API.
// Continuous streaming transport is intentionally kept separate: this method
// is the stable multipart primitive for finalized audio segments.
func (m *MistralAgentClient) TranscribeAudio(ctx context.Context, userID uint32, modelName, language, fileName string, audio []byte) (string, error) {
	if len(audio) == 0 {
		return "", fmt.Errorf("пустой аудиосегмент")
	}
	if modelName == "" {
		return "", fmt.Errorf("не задана STT-модель Mistral")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition", `form-data; name="file"; filename="`+fileName+`"`)
	partHeader.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(partHeader)
	if err != nil {
		return "", fmt.Errorf("создание multipart file: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return "", fmt.Errorf("запись audio: %w", err)
	}
	if err := writeMultipartField(writer, "model", modelName); err != nil {
		return "", err
	}
	if err := writeMultipartField(writer, "fileName", fileName); err != nil {
		return "", err
	}
	if language != "" {
		if err := writeMultipartField(writer, "language", language); err != nil {
			return "", err
		}
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("закрытие multipart: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.audioURL(mistralTranscriptionPath), &body)
	if err != nil {
		return "", err
	}
	m.setAudioHeaders(request, userID, writer.FormDataContentType())
	response, err := m.audioHTTPClient().Do(request)
	if err != nil {
		return "", fmt.Errorf("запрос transcription: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("transcription вернул %d: %s", response.StatusCode, string(data))
	}
	var result AudioTranscription
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("разбор transcription: %w", err)
	}
	return result.Text, nil
}

// StreamTranscribeAudio sends a completed audio segment and consumes the
// official SSE transcription stream (text.delta/segment/done).
func (m *MistralAgentClient) StreamTranscribeAudio(ctx context.Context, userID uint32, modelName, language, fileName string, audio []byte, onEvent func(TranscriptionStreamEvent) error) error {
	if len(audio) == 0 || modelName == "" {
		return fmt.Errorf("не заданы audio или STT-модель Mistral")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition", `form-data; name="file"; filename="`+fileName+`"`)
	partHeader.Set("Content-Type", "application/octet-stream")
	part, err := writer.CreatePart(partHeader)
	if err != nil {
		return err
	}
	if _, err = part.Write(audio); err != nil {
		return err
	}
	for name, value := range map[string]string{"model": modelName, "fileName": fileName, "stream": "true", "language": language} {
		if value != "" {
			if err := writeMultipartField(writer, name, value); err != nil {
				return err
			}
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, m.audioURL(mistralTranscriptionPath), &body)
	if err != nil {
		return err
	}
	m.setAudioHeaders(request, userID, writer.FormDataContentType())
	response, err := m.audioHTTPClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("stream transcription вернул %d: %s", response.StatusCode, string(data))
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return nil
		}
		var event TranscriptionStreamEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return fmt.Errorf("разбор transcription SSE: %w", err)
		}
		if onEvent != nil {
			if err := onEvent(event); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

// Speech opens a streaming TTS response. The caller owns and must close Body.
func (m *MistralAgentClient) Speech(ctx context.Context, userID uint32, request SpeechRequest) (io.ReadCloser, string, error) {
	if request.Model == "" || request.Input == "" {
		return nil, "", fmt.Errorf("не заданы модель или текст TTS Mistral")
	}
	request.Stream = true
	if request.ResponseFormat == "" {
		request.ResponseFormat = "wav"
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, "", fmt.Errorf("сериализация speech: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, m.audioURL(mistralSpeechPath), bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	m.setAudioHeaders(httpRequest, userID, "application/json")
	response, err := m.audioHTTPClient().Do(httpRequest)
	if err != nil {
		return nil, "", fmt.Errorf("запрос speech: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		return nil, "", fmt.Errorf("speech вернул %d: %s", response.StatusCode, string(data))
	}
	return response.Body, response.Header.Get("Content-Type"), nil
}

// Voices returns the voices available to the current Mistral account.
func (m *MistralAgentClient) Voices(ctx context.Context, userID uint32) ([]MistralVoice, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.audioURL(mistralVoicesPath), nil)
	if err != nil {
		return nil, err
	}
	m.setAudioHeaders(request, userID, "")
	response, err := m.audioHTTPClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("запрос voices: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("voices вернул %d: %s", response.StatusCode, string(data))
	}
	var result struct {
		Items  []MistralVoice `json:"items"`
		Data   []MistralVoice `json:"data"`
		Voices []MistralVoice `json:"voices"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("разбор voices: %w", err)
	}
	if len(result.Data) > 0 {
		return result.Data, nil
	}
	if len(result.Items) > 0 {
		return result.Items, nil
	}
	return result.Voices, nil
}

func (m *MistralAgentClient) ListVoices(ctx context.Context, userID uint32, limit, offset int, voiceType string) (VoiceList, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.audioURL(mistralVoicesPath), nil)
	if err != nil {
		return VoiceList{}, err
	}
	q := request.URL.Query()
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	if offset > 0 {
		q.Set("offset", fmt.Sprint(offset))
	}
	if voiceType != "" {
		q.Set("type", voiceType)
	}
	request.URL.RawQuery = q.Encode()
	m.setAudioHeaders(request, userID, "")
	response, err := m.audioHTTPClient().Do(request)
	if err != nil {
		return VoiceList{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return VoiceList{}, fmt.Errorf("voices вернул %d: %s", response.StatusCode, string(data))
	}
	var result VoiceList
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return VoiceList{}, err
	}
	return result, nil
}

func (m *MistralAgentClient) CreateVoice(ctx context.Context, userID uint32, input CreateVoiceRequest) (MistralVoice, error) {
	var out MistralVoice
	err := m.voiceJSON(ctx, userID, http.MethodPost, "", input, &out)
	return out, err
}
func (m *MistralAgentClient) GetVoice(ctx context.Context, userID uint32, voiceID string) (MistralVoice, error) {
	var out MistralVoice
	err := m.voiceJSON(ctx, userID, http.MethodGet, "/"+url.PathEscape(voiceID), nil, &out)
	return out, err
}
func (m *MistralAgentClient) UpdateVoice(ctx context.Context, userID uint32, voiceID string, input UpdateVoiceRequest) (MistralVoice, error) {
	var out MistralVoice
	err := m.voiceJSON(ctx, userID, http.MethodPatch, "/"+url.PathEscape(voiceID), input, &out)
	return out, err
}
func (m *MistralAgentClient) DeleteVoice(ctx context.Context, userID uint32, voiceID string) (MistralVoice, error) {
	var out MistralVoice
	err := m.voiceJSON(ctx, userID, http.MethodDelete, "/"+url.PathEscape(voiceID), nil, &out)
	return out, err
}
func (m *MistralAgentClient) GetVoiceSample(ctx context.Context, userID uint32, voiceID string) (io.ReadCloser, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, m.audioURL(mistralVoicesPath+"/"+url.PathEscape(voiceID)+"/sample"), nil)
	if err != nil {
		return nil, "", err
	}
	m.setAudioHeaders(request, userID, "")
	response, err := m.audioHTTPClient().Do(request)
	if err != nil {
		return nil, "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, "", fmt.Errorf("voice sample вернул %d", response.StatusCode)
	}
	return response.Body, response.Header.Get("Content-Type"), nil
}

func (m *MistralAgentClient) voiceJSON(ctx context.Context, userID uint32, method, suffix string, input, output any) error {
	var reader io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, m.audioURL(mistralVoicesPath+suffix), reader)
	if err != nil {
		return err
	}
	m.setAudioHeaders(request, userID, "application/json")
	response, err := m.audioHTTPClient().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("voice API вернул %d: %s", response.StatusCode, string(data))
	}
	if output != nil {
		return json.NewDecoder(response.Body).Decode(output)
	}
	return nil
}

func writeMultipartField(writer *multipart.Writer, name, value string) error {
	field, err := writer.CreateFormField(name)
	if err != nil {
		return fmt.Errorf("создание multipart %s: %w", name, err)
	}
	if _, err := field.Write([]byte(value)); err != nil {
		return fmt.Errorf("запись multipart %s: %w", name, err)
	}
	return nil
}

func (m *MistralAgentClient) audioURL(path string) string {
	return strings.TrimRight(mode.MistralBaseURL, "/") + path
}

func (m *MistralAgentClient) setAudioHeaders(request *http.Request, userID uint32, contentType string) {
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if key := m.resolveKey(userID); key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
}

func (m *MistralAgentClient) audioHTTPClient() *http.Client {
	if m != nil && m.httpClient != nil {
		return m.httpClient
	}
	return http.DefaultClient
}
