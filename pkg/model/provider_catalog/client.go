package provider_catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ikermy/air_common/pkg/mode"
	"github.com/ikermy/air_common/pkg/model/domain"
)

// Client fetches provider model lists from external provider APIs.
// It is intentionally kept outside pkg/comdb so that the DB layer remains database-only.
type Client struct {
	HTTPClient *http.Client
}

// DefaultMistralSTTModel is the production default Voxtral realtime
// transcription model. STT models are not stored in realtime_models because
// that table has a single realtime-model role in the legacy schema.
const DefaultMistralSTTModel = "voxtral-mini-transcribe-realtime-2602"

func NewClient() *Client {
	return &Client{HTTPClient: &http.Client{}}
}

type Syncer interface {
	SyncProviderModels(union domain.Union, modelNames []string) (domain.ProviderModelsSyncResult, error)
}

func SyncProviderModels(ctx context.Context, syncer Syncer, union domain.Union, apiKey string) (domain.ProviderModelsSyncResult, error) {
	result := domain.ProviderModelsSyncResult{Provider: union.Provider}
	if ctx == nil {
		ctx = context.Background()
	}

	client := NewClient()
	modelNames, err := client.FetchModelNames(ctx, union, apiKey)
	if err != nil {
		return result, fmt.Errorf("не удалось получить каталог моделей провайдера %s: %w", union.Provider, err)
	}

	if union.ModelType.IsGeneral() || union.ModelType.IsRealtime() {
		result, err = syncer.SyncProviderModels(union, modelNames)
	}
	if err != nil {
		return result, fmt.Errorf("не удалось синхронизировать каталог моделей провайдера %s: %w", union.Provider, err)
	}
	return result, nil
}

// FetchModelNames получает актуальный список моделей провайдера из внешнего API.
func (c *Client) FetchModelNames(
	ctx context.Context,
	union domain.Union,
	apiKey string,
) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	client := c
	if client == nil || client.HTTPClient == nil {
		client = NewClient()
	}

	if !union.Provider.IsValid() {
		return nil, fmt.Errorf("некорректный provider: %d", union.Provider)
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("пустой API-ключ для провайдера %s", union.Provider.String())
	}

	switch union.Provider {
	case domain.ProviderOpenAI:
		switch {
		case union.ModelType.IsGeneral():
			return client.generalOpenAIModels(ctx, apiKey)
		case union.ModelType.IsRealtime():
			return client.realtimeOpenAIModels(ctx, apiKey)
		default:
			return client.fetchOpenAIModels(ctx, apiKey)
		}
	case domain.ProviderMistral:
		switch {
		case union.ModelType.IsGeneral():
			return client.generalMistralModels(ctx, apiKey)
		case union.ModelType.IsRealtime():
			return client.realtimeMistralModels(ctx, apiKey)
		default:
			return client.fetchMistralModels(ctx, apiKey)
		}
	case domain.ProviderGoogle:
		switch {
		case union.ModelType.IsGeneral():
			return client.generalGoogleModels(ctx, apiKey)
		case union.ModelType.IsRealtime():
			return client.realtimeGoogleModels()
		default:
			return client.fetchGoogleModels(ctx, apiKey)
		}
	default:
		return nil, fmt.Errorf("неподдерживаемый провайдер: %s", union.Provider.String())
	}
}

// FetchMistralVoiceModels returns the specialized models used by the
// server-side realtime voice pipeline. They are auxiliary fields of a
// realtime model response, not independent public ModelType values.
func (c *Client) FetchMistralVoiceModels(ctx context.Context, apiKey string) (stt, tts []string, err error) {
	stt, err = c.sttMistralModels(ctx, apiKey)
	if err != nil {
		return nil, nil, err
	}
	tts, err = c.ttsMistralModels(ctx, apiKey)
	if err != nil {
		return nil, nil, err
	}
	return stt, tts, nil
}

func (c *Client) fetchOpenAIModels(ctx context.Context, apiKey string) ([]string, error) {
	return c.fetchListModels(ctx, mode.OpenAIAgentsURL+"/models", apiKey, func(body []byte) ([]string, error) {
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("ошибка разбора ответа OpenAI: %w", err)
		}
		result := make([]string, 0, len(payload.Data))
		for _, item := range payload.Data {
			if name := strings.TrimSpace(item.ID); name != "" {
				result = append(result, name)
			}
		}
		return result, nil
	})
}

func (c *Client) generalOpenAIModels(ctx context.Context, apiKey string) ([]string, error) {
	allModels, err := c.fetchOpenAIModels(ctx, apiKey)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения общих моделей OpenIA: %v", err)
	}

	result := make([]string, 0, len(allModels))
	for _, name := range allModels {
		if isGeneralOpenAIModel(name) {
			result = append(result, name)
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("получено 0 общих моделей OpenAI")
	}

	return result, nil
}

func (c *Client) realtimeOpenAIModels(ctx context.Context, apiKey string) ([]string, error) {
	allModels, err := c.fetchOpenAIModels(ctx, apiKey)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения моделей OpenAI: %v", err)
	}

	result := make([]string, 0, len(allModels))
	for _, name := range allModels {
		if isRealtimeModel(name) {
			result = append(result, name)
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("получено 0 realtime моделей OpenAI")
	}

	return result, nil
}

func (c *Client) generalGoogleModels(ctx context.Context, apiKey string) ([]string, error) {
	allModels, err := c.fetchGoogleModels(ctx, apiKey)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения общих моделей Google: %v", err)
	}

	result := make([]string, 0, len(allModels))
	for _, name := range allModels {
		if isGeneralGoogleModel(name) {
			result = append(result, name)
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("получено 0 общих моделей Google")
	}

	return result, nil
}

// Google не отдаёт список live моделей, альтернативных источников я тоже не нашёл...
func (c *Client) realtimeGoogleModels() ([]string, error) {
	result := []string{
		"gemini-3.1-flash-live-preview",
		"gemini-2.5-flash-live-preview",
		"gemini-2.0-flash-exp",
		"gemini-omni-flash-preview",
	}
	return result, nil
}

func isRealtimeModel(modelName string) bool {
	if strings.HasPrefix(modelName, "-realtime-") ||
		strings.HasPrefix(modelName, "-tts-") {
		return true
	}
	return strings.Contains(modelName, "realtime")
}

// isMistralRealtimeLLMModel selects only the realtime language models that
// belong in realtime_models. Mistral exposes STT and TTS models from the same
// /models endpoint, but they have different roles and must not be synced into
// the realtime-model table.
func isMistralRealtimeLLMModel(modelName string) bool {
	lower := strings.ToLower(strings.TrimSpace(modelName))
	return strings.Contains(lower, "realtime") &&
		!strings.Contains(lower, "transcribe") &&
		!strings.Contains(lower, "-tts")
}

func isGeneralOpenAIModel(modelName string) bool {
	// исключаем realtime, tts, transcribe, embedding, moderation
	exclude := []string{"realtime", "tts", "transcribe", "embedding", "moderation", "audio"}
	for _, bad := range exclude {
		if strings.Contains(modelName, bad) {
			return false
		}
	}
	return true
}

func isGeneralGoogleModel(modelName string) bool {
	return isGeneralOpenAIModel(modelName) && !strings.Contains(modelName, "live") &&
		!strings.Contains(modelName, "imagen") && !strings.Contains(modelName, "veo") &&
		!strings.Contains(modelName, "embedding")
}

func (c *Client) fetchMistralModels(ctx context.Context, apiKey string) ([]string, error) {
	return c.fetchListModels(ctx, mode.MistralBaseURL+"/models", apiKey, func(body []byte) ([]string, error) {
		var payload struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("ошибка разбора ответа Mistral: %w", err)
		}
		result := make([]string, 0, len(payload.Data))
		for _, item := range payload.Data {
			if name := strings.TrimSpace(item.ID); name != "" {
				result = append(result, name)
			}
		}
		return result, nil
	})
}

func (c *Client) generalMistralModels(ctx context.Context, apiKey string) ([]string, error) {
	allModels, err := c.fetchMistralModels(ctx, apiKey)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения общих моделей Mistral: %v", err)
	}

	result := make([]string, 0, len(allModels))
	for _, name := range allModels {
		if isGeneralMistralModel(name) {
			result = append(result, name)
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("получено 0 общих моделей Mistral")
	}
	return result, nil
}

func isGeneralMistralModel(modelName string) bool {
	exclude := []string{"embed", "moderation", "ocr", "realtime", "transcribe", "voxtral"}
	for _, bad := range exclude {
		if strings.Contains(modelName, bad) {
			return false
		}
	}
	return true
}

func (c *Client) realtimeMistralModels(ctx context.Context, apiKey string) ([]string, error) {
	allModels, err := c.fetchMistralModels(ctx, apiKey)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения моделей Mistral: %v", err)
	}

	result := make([]string, 0, len(allModels))
	for _, name := range allModels {
		if isMistralRealtimeLLMModel(name) {
			result = append(result, name)
		}
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("получено 0 realtime моделей Mistral")
	}

	return result, nil
}

func filterMistralModels(models []string, predicate func(string) bool) []string {
	result := make([]string, 0, len(models))
	for _, name := range models {
		if predicate(name) {
			result = append(result, name)
		}
	}
	return result
}

func isMistralSTTModel(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "transcribe-realtime")
}

func isMistralTTSModel(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "-tts")
}

func (c *Client) sttMistralModels(ctx context.Context, apiKey string) ([]string, error) {
	allModels, err := c.fetchMistralModels(ctx, apiKey)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения STT моделей Mistral: %w", err)
	}
	result := filterMistralModels(allModels, isMistralSTTModel)
	if len(result) == 0 {
		return nil, fmt.Errorf("получено 0 STT моделей Mistral")
	}
	return result, nil
}

func (c *Client) ttsMistralModels(ctx context.Context, apiKey string) ([]string, error) {
	allModels, err := c.fetchMistralModels(ctx, apiKey)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения TTS моделей Mistral: %w", err)
	}
	result := filterMistralModels(allModels, isMistralTTSModel)
	if len(result) == 0 {
		return nil, fmt.Errorf("получено 0 TTS моделей Mistral")
	}
	return result, nil
}

func (c *Client) fetchGoogleModels(ctx context.Context, apiKey string) ([]string, error) {
	// Google API expects the API key as a query parameter, not as a Bearer token.
	baseURL := mode.GoogleAgentsURL + "/models"
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("ошибка формирования URL Google: %w", err)
	}
	q := u.Query()
	q.Set("key", apiKey)
	u.RawQuery = q.Encode()
	return c.fetchListModels(ctx, u.String(), "", func(body []byte) ([]string, error) {
		var payload struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("ошибка разбора ответа Google: %w", err)
		}
		result := make([]string, 0, len(payload.Models))
		for _, item := range payload.Models {
			name := strings.TrimSpace(strings.TrimPrefix(item.Name, "models/"))
			if name != "" {
				result = append(result, name)
			}
		}
		return result, nil
	})
}

func (c *Client) fetchListModels(ctx context.Context, url, apiKey string, parser func([]byte) ([]string, error)) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API вернул %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parser(body)
}
