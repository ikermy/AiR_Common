package provider_catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// Client fetches provider model lists from external provider APIs.
// It is intentionally kept outside pkg/comdb so that the DB layer remains database-only.
type Client struct {
	HTTPClient *http.Client
}

func NewClient() *Client {
	return &Client{HTTPClient: &http.Client{}}
}

type Syncer interface {
	SyncProviderModels(provider create.ProviderType, modelNames []string) (create.ProviderModelsSyncResult, error)
}

func SyncProviderModels(ctx context.Context, syncer Syncer, provider create.ProviderType, apiKey string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	client := NewClient()
	modelNames, err := client.FetchModelNames(ctx, provider, apiKey)
	if err != nil {
		return fmt.Errorf("не удалось получить каталог моделей провайдера %s: %w", provider, err)
	}

	_, err = syncer.SyncProviderModels(provider, modelNames)
	if err != nil {
		return fmt.Errorf("не удалось синхронизировать каталог моделей провайдера %s: %w", provider, err)
	}
	return nil
}

// FetchModelNames получает актуальный список моделей провайдера из внешнего API.
func (c *Client) FetchModelNames(ctx context.Context, provider create.ProviderType, apiKey string) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	client := c
	if client == nil || client.HTTPClient == nil {
		client = NewClient()
	}

	if !provider.IsValid() {
		return nil, fmt.Errorf("некорректный provider: %d", provider)
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("пустой API-ключ для провайдера %s", provider.String())
	}

	switch provider {
	case create.ProviderOpenAI:
		return client.fetchOpenAIModels(ctx, apiKey)
	case create.ProviderMistral:
		return client.fetchMistralModels(ctx, apiKey)
	case create.ProviderGoogle:
		return client.fetchGoogleModels(ctx, apiKey)
	default:
		return nil, fmt.Errorf("неподдерживаемый провайдер: %s", provider.String())
	}
}

func (c *Client) fetchOpenAIModels(ctx context.Context, apiKey string) ([]string, error) {
	return c.fetchListModels(ctx, "https://api.openai.com/v1/models", apiKey, func(body []byte) ([]string, error) {
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

func (c *Client) fetchMistralModels(ctx context.Context, apiKey string) ([]string, error) {
	return c.fetchListModels(ctx, "https://api.mistral.ai/v1/models", apiKey, func(body []byte) ([]string, error) {
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

func (c *Client) fetchGoogleModels(ctx context.Context, apiKey string) ([]string, error) {
	return c.fetchListModels(ctx, "https://generativelanguage.googleapis.com/v1beta/models", apiKey, func(body []byte) ([]string, error) {
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
	req.Header.Set("Authorization", "Bearer "+apiKey)
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
