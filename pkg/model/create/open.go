package create

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"reflect"
	"strings"
	"time"
)

// ============================================================================
// OPENAI EMBEDDINGS API - Генерация эмбеддингов
// ============================================================================

// GenerateOpenAIEmbedding - публичная функция для генерации эмбеддингов через OpenAI API
// Используется для создания векторных представлений текста для семантического поиска
// По умолчанию использует text-embedding-3-small с 512 dimensions (оптимальный баланс цена/качество)
func GenerateOpenAIEmbedding(ctx context.Context, apiKey, text string) ([]float32, error) {
	return generateOpenAIEmbedding(ctx, apiKey, text, "text-embedding-3-small", 512)
}

// TODO метод на будущее: GenerateOpenAIEmbeddingMedium - эмбеддинги средней точности (1536 dimensions)
// GenerateOpenAIEmbeddingLarge - генерация эмбеддингов высокой точности (3072 dimensions)
// Используется когда требуется максимальная точность семантического поиска
func GenerateOpenAIEmbeddingLarge(ctx context.Context, apiKey, text string) ([]float32, error) {
	return generateOpenAIEmbedding(ctx, apiKey, text, "text-embedding-3-large", 3072)
}

// generateOpenAIEmbedding - внутренняя функция для генерации эмбеддингов через OpenAI API
// model: "text-embedding-3-small" или "text-embedding-3-large"
// dimensions: 512, 1536 для small; 256, 1024, 3072 для large
func generateOpenAIEmbedding(ctx context.Context, apiKey, text, model string, dimensions int) ([]float32, error) {
	if text == "" {
		return nil, fmt.Errorf("текст не может быть пустым")
	}

	embedURL := "https://api.openai.com/v1/embeddings"

	payload := map[string]interface{}{
		"input":      text,
		"model":      model,
		"dimensions": dimensions,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ошибка сериализации запроса: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, embedURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("ошибка создания запроса: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка HTTP запроса: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения ответа: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		//logger.Error("generateOpenAIEmbedding: API вернул %d: %s", resp.StatusCode, string(responseBody))
		return nil, fmt.Errorf("API вернул %d: %s", resp.StatusCode, string(responseBody))
	}

	var embedResp struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}

	if err := json.Unmarshal(responseBody, &embedResp); err != nil {
		return nil, fmt.Errorf("ошибка парсинга ответа: %w", err)
	}

	if len(embedResp.Data) == 0 || len(embedResp.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("API вернул пустой эмбеддинг")
	}

	//logger.Debug("generateOpenAIEmbedding: создан эмбеддинг размерности %d", len(embedResp.Data[0].Embedding))
	return embedResp.Data[0].Embedding, nil
}

// ============================================================================
// OPENAI API CLIENT
// ============================================================================

// OpenAIAgentClient клиент для работы с OpenAI API через прямые HTTP вызовы
type OpenAIAgentClient struct {
	apiKey         string
	url            string
	ctx            context.Context
	httpClient     *http.Client
	universalModel *UniversalModel // Ссылка на universalModel для доступа к GetRealUserID
	keyResolver    func(userID uint32) string // Резолвер персональных ключей; nil → глобальный apiKey
}

// StreamingFunctionCall представляет накапливаемый function call для Realtime API
type StreamingFunctionCall struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// AssistantTool определение инструмента
type AssistantTool struct {
	Type      string              `json:"type"`
	Function  *FunctionDefinition `json:"function,omitempty"`
	Container string              `json:"container,omitempty"` // Для Responses API: "function_tool"
}

// FunctionDefinition определение функции
type FunctionDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
	Strict      bool        `json:"strict,omitempty"`
}

// getStringField safely extracts string field from map
func getStringField(m map[string]interface{}, key string) string {
	if val, ok := m[key].(string); ok {
		return val
	}
	return ""
}

// NewOpenAIAgentClient создаёт новый OpenAI клиент.
// API-ключ не передаётся глобально — используется только персональный ключ
// из БД через SetKeyResolver.
func NewOpenAIAgentClient(ctx context.Context) *OpenAIAgentClient {
	return &OpenAIAgentClient{
		url: "https://api.openai.com/v1",
		ctx: ctx,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// GetAPIKey возвращает API ключ клиента (для использования в функциях генерации эмбеддингов)
func (c *OpenAIAgentClient) GetAPIKey() string {
	return c.apiKey
}

// SetKeyResolver устанавливает функцию-резолвер персонального API-ключа пользователя.
func (c *OpenAIAgentClient) SetKeyResolver(fn func(userID uint32) string) {
	c.keyResolver = fn
}

// resolveKey возвращает API-ключ: персональный для userID (если задан) или глобальный.
func (c *OpenAIAgentClient) resolveKey(userID uint32) string {
	if c.keyResolver != nil && userID != 0 {
		if key := c.keyResolver(userID); key != "" {
			return key
		}
	}
	return c.apiKey
}

// GetAPIKeyForUser возвращает эффективный API-ключ для пользователя (персональный или глобальный).
func (c *OpenAIAgentClient) GetAPIKeyForUser(userID uint32) string {
	return c.resolveKey(userID)
}

// HasAPIKey возвращает true если для пользователя есть действующий API-ключ.
// Используется для ранней проверки перед выполнением запросов.
func (c *OpenAIAgentClient) HasAPIKey(userID uint32) bool {
	return c.resolveKey(userID) != ""
}

// doRequest выполняет HTTP запрос к OpenAI API
func (c *OpenAIAgentClient) doRequest(ctx context.Context, method, path string, body interface{}, userID uint32) (*http.Response, error) {
	url := c.url + path

	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}
		reqBody = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.resolveKey(userID))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "assistants=v2")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		func() { _ = resp.Body.Close() }()
		return nil, fmt.Errorf("OpenAI API error: HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return resp, nil
}

// DeleteFile удаляет файл
func (c *OpenAIAgentClient) DeleteFile(ctx context.Context, fileID string) error {
	resp, err := c.doRequest(ctx, "DELETE", fmt.Sprintf("/files/%s", fileID), nil, 0)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	return nil
}

// DownloadFileContent скачивает содержимое файла
func (c *OpenAIAgentClient) DownloadFileContent(ctx context.Context, fileID string) ([]byte, error) {
	resp, err := c.doRequest(ctx, "GET", fmt.Sprintf("/files/%s/content", fileID), nil, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	return io.ReadAll(resp.Body)
}

// TranscribeAudio транскрибирует аудио в текст
func (c *OpenAIAgentClient) TranscribeAudio(ctx context.Context, audioData []byte, fileName string) (string, error) {
	// Создаём multipart запрос для Whisper API
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	// Добавляем аудио файл
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return "", fmt.Errorf("failed to create form file: %w", err)
	}
	if _, err := part.Write(audioData); err != nil {
		return "", fmt.Errorf("failed to write audio data: %w", err)
	}

	// Добавляем модель
	if err := writer.WriteField("model", "whisper-1"); err != nil {
		return "", fmt.Errorf("failed to write model field: %w", err)
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("failed to close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.url+"/audio/transcriptions", &buf)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.resolveKey(0))
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("OpenAI API error: HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return result.Text, nil
}

// maxToolCallDepth — максимальная глубина рекурсии при вызове инструментов.
// Ограничивает бесконечные циклы когда модель навязчиво вызывает инструменты.
const maxToolCallDepth = 5

// CreateResponse выполняет запрос к Responses API
// Поддерживает file_search, code_interpreter, web_search в отличие от Chat Completions
// Поддерживает вызов функций (function calls) в streaming режиме
//
// ✅ PROMPT CACHING - Экономия токенов:
// Кэширование работает АВТОМАТИЧЕСКИ для промптов >= 1024 токенов (без изменений кода!).
// Работает следующим образом:
//   - Запросы маршрутизируются на сервер по хэшу первых ~256 токенов промпта
//   - Система кэширует статическую часть: instructions + tools + schema
//   - При повторных запросах cached_tokens стоят в 10 раз дешевле!
//
// Политики хранения кэша:
//   - "in_memory" (по умолчанию): 5-10 минут, максимум 1 час
//   - "24h" (Extended Caching): до 24 часов (только gpt-5.x, gpt-4.1+)
//
// Пример экономии:
//
//	Запрос 1: input_tokens=1500 (полная стоимость)
//	Запрос 2: input_tokens=150, cached_tokens=1350 (экономия 90%!)
//
// Требования:
//   - Минимум 1024 токена для кэширования
//   - Статический контент в начале промпта (instructions, tools)
//   - Динамический контент в конце (user input, история)
//
// Совместимость:
//   - In-memory caching: все модели с Prompt Caching
//   - Extended caching (24h): gpt-5.2, gpt-5.1, gpt-5, gpt-4.1
func (c *OpenAIAgentClient) CreateResponse(
	ctx context.Context,
	input string,
	agentConfig interface{},
	onDelta func(string) error,
	onToolCall func([]interface{}) ([]interface{}, error),
	userID uint32,
) (interface{}, string, error) {
	return c.createResponseInternal(ctx, input, agentConfig, onDelta, onToolCall, userID, 0)
}

func (c *OpenAIAgentClient) createResponseInternal(
	ctx context.Context,
	input string,
	agentConfig interface{}, // *OpenAIAgentConfig
	onDelta func(string) error,
	onToolCall func([]interface{}) ([]interface{}, error),
	userID uint32,
	depth int,
) (interface{}, string, error) {
	// Защита от бесконечной рекурсии при многократных вызовах инструментов
	if depth >= maxToolCallDepth {
		return nil, "", fmt.Errorf("превышен лимит вложенных вызовов инструментов (%d)", maxToolCallDepth)
	}

	// КРИТИЧНО: НЕ используем json.Marshal/Unmarshal для agentConfig,
	// так как это уничтожает custom MarshalJSON для FunctionTool!
	// Вместо этого используем type assertion напрямую

	// Type assertion к OpenAIAgentConfig из пакета openai
	// Но так как мы в пакете create, нужно использовать interface{} и рефлексию
	// Альтернативно: Marshal только для извлечения простых полей, tools берем отдельно

	configBytes, err := json.Marshal(agentConfig)
	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal agentConfig: %w", err)
	}

	// Временная структура для извлечения полей БЕЗ tools
	var configMap map[string]interface{}
	if err := json.Unmarshal(configBytes, &configMap); err != nil {
		return nil, "", fmt.Errorf("failed to unmarshal agentConfig: %w", err)
	}

	// Формируем payload для Responses API
	payload := map[string]interface{}{
		"model":  configMap["model_name"],
		"input":  input,
		"stream": true, // КРИТИЧНО: Включаем streaming
	}

	// Добавляем instructions (system prompt)
	if systemPrompt, ok := configMap["system_prompt"].(string); ok && systemPrompt != "" {
		payload["instructions"] = systemPrompt

		// ✅ PROMPT CACHING - ЭКОНОМИЯ 70-90% ТОКЕНОВ!
		// Кэширование работает АВТОМАТИЧЕСКИ для промптов >= 1024 токенов (без параметров!)
		// Кэшируется статическая часть: instructions + tools + schema
		// При повторных запросах cached_tokens стоят в 10 раз дешевле input_tokens
		//
		// Extended Prompt Caching (24h retention) доступен ТОЛЬКО для:
		// gpt-5.2, gpt-5.1-codex-max, gpt-5.1, gpt-5.1-codex, gpt-5.1-codex-mini,
		// gpt-5.1-chat-latest, gpt-5, gpt-5-codex, gpt-4.1
		//
		// Политики хранения кэша:
		// - "in_memory": 5-10 минут, макс 1 час (по умолчанию)
		// - "24h": до 24 часов (Extended Caching)
		//
		// Проверяем, поддерживает ли модель prompt_cache_retention
		modelName, _ := configMap["model_name"].(string)
		supportsExtendedCaching := false

		// Список моделей, поддерживающих Extended Caching (24h)
		supportedModels := OpenAIExtandingCacheModels

		for _, supported := range supportedModels {
			if modelName == supported {
				supportsExtendedCaching = true
				break
			}
		}

		if supportsExtendedCaching {
			// Extended Caching для поддерживаемых моделей
			payload["prompt_cache_retention"] = "24h"
			//logger.Debug("[CreateResponse] Включен Extended Caching (24h) для модели %s", modelName, userID)
		} else {
			// Для остальных моделей (включая gpt-4.1-nano) НЕ указываем параметр
			// Кэширование всё равно работает автоматически (in_memory по умолчанию)
			//logger.Debug("[CreateResponse] Используется автоматическое кэширование (in_memory) для модели %s", modelName, userID)
		}
	}

	// КРИТИЧНО: Извлекаем tools напрямую из агентConfig используя reflection,
	// чтобы сохранить тип FunctionTool и его custom MarshalJSON
	if agentConfigValue := reflect.ValueOf(agentConfig); agentConfigValue.Kind() == reflect.Ptr {
		configStruct := agentConfigValue.Elem()
		if configStruct.Kind() == reflect.Struct {
			toolsField := configStruct.FieldByName("Tools")
			if toolsField.IsValid() && toolsField.Kind() == reflect.Slice && toolsField.Len() > 0 {
				// Берем tools напрямую как interface{}, сохраняя тип FunctionTool
				payload["tools"] = toolsField.Interface()
			}
		}
	}

	// Всегда добавляем text.format для получения структурированного JSON ответа
	// ВАЖНО: В Responses API text.format НЕ блокирует вызовы функций (в отличие от Chat Completions API)
	// Модель может сначала вызвать функции, а затем вернуть JSON согласно schema
	if responseFormat, ok := configMap["response_format"].(map[string]interface{}); ok {
		// Извлекаем json_schema из response_format
		if jsonSchema, ok := responseFormat["json_schema"].(map[string]interface{}); ok {
			// Извлекаем name и schema из json_schema
			name, _ := jsonSchema["name"].(string)
			schema, _ := jsonSchema["schema"].(map[string]interface{})

			// strict=false когда в запросе есть tools (function-инструменты из MCP/calendar/sheets
			// могут иметь схемы без additionalProperties:false, что несовместимо с strict=true).
			// OpenAI возвращает HTTP 400 если strict=true + non-strict function tool schemas.
			_, hasTools := payload["tools"]
			strictMode := !hasTools

			payload["text"] = map[string]interface{}{
				"format": map[string]interface{}{
					"type":   "json_schema",
					"name":   name,
					"schema": schema,
					"strict": strictMode,
				},
			}
		}
	}

	// ✅ ТЕСТИРОВАНИЕ: Priority Processing для Responses API
	// Доступные значения service_tier:
	// - "default" (стандартная обработка, гибкое ценообразование)
	// - "priority" (приоритетная обработка, низкая задержка, премиум цена)
	//
	// Priority processing обеспечивает значительно более низкую и стабильную задержку
	// по сравнению со стандартной обработкой, сохраняя гибкость pay-as-you-go.
	//
	// Идеально для высокоценных приложений с регулярным трафиком, где важна задержка.
	// НЕ использовать для обработки данных, оценок или нестабильного трафика.
	//Supported values are:'auto', 'default', 'flex', and 'priority'
	const SERVICE_TIER = "default"
	payload["service_tier"] = SERVICE_TIER
	//logger.Debug("[CreateResponse] Используется service_tier: %s", SERVICE_TIER, userID)

	// Выполняем streaming запрос к /responses
	resp, err := c.doRequest(ctx, "POST", "/responses", payload, userID)
	if err != nil {
		return nil, "", fmt.Errorf("responses API request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Читаем streaming ответ (Server-Sent Events)
	fullText := ""
	scanner := bufio.NewScanner(resp.Body)
	var result map[string]interface{}
	var tokenUsageData map[string]interface{} // Сохраняем информацию о токенах

	// Накопление function calls (output_index -> function call с накопленными аргументами)
	// По аналогии с документацией OpenAI для Responses API
	functionCallsMap := make(map[int]*StreamingFunctionCall)

	for scanner.Scan() {
		line := scanner.Text()

		// SSE формат: "data: {...json...}"
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		// Пропускаем "[DONE]" маркер
		if data == "[DONE]" {
			//logger.Debug("[CreateResponse] Получен маркер [DONE], завершаем чтение SSE", userID)
			break
		}

		// Парсим JSON событие
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			//logger.Warn("[CreateResponse] Ошибка парсинга SSE события: %v, data: %s", err, data, userID)
			continue
		}

		// Извлекаем тип события
		eventType, _ := event["type"].(string)

		// Обрабатываем различные типы событий
		switch eventType {
		case "response.output_text.delta":
			// Обрабатываем события с текстовыми дельтами
			if delta, ok := event["delta"].(string); ok && delta != "" {
				fullText += delta

				// Вызываем callback для потоковой отправки
				if onDelta != nil {
					if err := onDelta(delta); err != nil {
						//logger.Warn("[CreateResponse] Ошибка в onDelta callback: %v", err)
					}
				}
			}

		case "response.output_item.added":
			// Начало нового вызова функции - транслируем событие клиенту
			if outputIndexFloat, ok := event["output_index"].(float64); ok {
				outputIndex := int(outputIndexFloat)
				if item, ok := event["item"].(map[string]interface{}); ok {
					if itemType, ok := item["type"].(string); ok && itemType == "function_call" {
						// Инициализируем функцию в map для накопления
						functionCallsMap[outputIndex] = &StreamingFunctionCall{
							Type:      "function_call",
							ID:        getStringField(item, "id"),
							CallID:    getStringField(item, "call_id"),
							Name:      getStringField(item, "name"),
							Arguments: "",
						}

						// Транслируем событие клиенту в формате OpenAI
						if onDelta != nil {
							eventJSON, err := json.Marshal(event)
							if err == nil {
								if streamErr := onDelta(string(eventJSON)); streamErr != nil {
									//logger.Warn("[CreateResponse] Ошибка при отправке response.output_item.added: %v", streamErr, userID)
								}
							}
						}
					}
				}
			}

		case "response.function_call_arguments.delta":
			// Накопление аргументов функции - транслируем дельту клиенту
			if outputIndexFloat, ok := event["output_index"].(float64); ok {
				outputIndex := int(outputIndexFloat)
				if delta, ok := event["delta"].(string); ok && delta != "" {
					if fn, exists := functionCallsMap[outputIndex]; exists {
						fn.Arguments += delta
					}

					// Транслируем событие клиенту в формате OpenAI
					if onDelta != nil {
						eventJSON, err := json.Marshal(event)
						if err == nil {
							if streamErr := onDelta(string(eventJSON)); streamErr != nil {
								//logger.Warn("[CreateResponse] Ошибка при отправке response.function_call_arguments.delta: %v", streamErr, userID)
							}
						}
					}
				}
			}

		case "response.function_call_arguments.done":
			// Завершение накопления аргументов функции - транслируем событие клиенту
			if outputIndexFloat, ok := event["output_index"].(float64); ok {
				outputIndex := int(outputIndexFloat)
				if arguments, ok := event["arguments"].(string); ok {
					if fn, exists := functionCallsMap[outputIndex]; exists {
						fn.Arguments = arguments
					}

					// Транслируем событие клиенту в формате OpenAI
					if onDelta != nil {
						eventJSON, err := json.Marshal(event)
						if err == nil {
							if streamErr := onDelta(string(eventJSON)); streamErr != nil {
								//logger.Warn("[CreateResponse] Ошибка при отправке response.function_call_arguments.done: %v", streamErr, userID)
							}
						}
					}
				}
			}

		case "response.output_item.done":
			// Завершение элемента вывода - транслируем событие клиенту
			if item, ok := event["item"].(map[string]interface{}); ok {
				if itemType, ok := item["type"].(string); ok && itemType == "function_call" {
					// Транслируем событие клиенту в формате OpenAI
					if onDelta != nil {
						eventJSON, err := json.Marshal(event)
						if err == nil {
							if streamErr := onDelta(string(eventJSON)); streamErr != nil {
								//logger.Warn("[CreateResponse] Ошибка при отправке response.output_item.done: %v", streamErr, userID)
							}
						}
					}
				}
			}

		case "error":
			// Обработка события ошибки
			errorMsg := "unknown error"
			if err, ok := event["error"].(map[string]interface{}); ok {
				if msg, ok := err["message"].(string); ok {
					errorMsg = msg
				}
				if code, ok := err["code"].(string); ok {
					errorMsg = fmt.Sprintf("%s (code: %s)", errorMsg, code)
				}
			}
			//logger.Error("[CreateResponse] OpenAI API error: %s", errorMsg, userID)
			return nil, "", fmt.Errorf("OpenAI API error: %s", errorMsg)

		case "response.failed":
			// Обработка события response.failed
			if response, ok := event["response"].(map[string]interface{}); ok {
				if lastError, ok := response["last_error"].(map[string]interface{}); ok {
					errorMsg := "unknown error"
					if msg, ok := lastError["message"].(string); ok {
						errorMsg = msg
					}
					if code, ok := lastError["code"].(string); ok {
						errorMsg = fmt.Sprintf("%s (code: %s)", errorMsg, code)
					}
					//logger.Error("[CreateResponse] Response failed: %s", errorMsg, userID)
					return nil, "", fmt.Errorf("response failed: %s", errorMsg)
				}
			}
			//logger.Error("[CreateResponse] Response failed: no error details", userID)
			return nil, "", fmt.Errorf("response failed: no error details")

		case "response.completed":
			// Сохраняем финальное событие
			result = event

			// Извлекаем и логируем информацию о расходе токенов
			// В Responses API usage находится в event.response.usage
			if response, ok := event["response"].(map[string]interface{}); ok {
				if usage, ok := response["usage"].(map[string]interface{}); ok {
					// Сохраняем usage для передачи клиенту
					tokenUsageData = usage

					// Извлекаем конкретные значения для читаемого вывода
					//inputTokens := 0
					//outputTokens := 0
					//totalTokens := 0
					//cachedTokens := 0
					//
					//if val, ok := usage["input_tokens"].(float64); ok {
					//	inputTokens = int(val)
					//}
					//if val, ok := usage["output_tokens"].(float64); ok {
					//	outputTokens = int(val)
					//}
					//if val, ok := usage["total_tokens"].(float64); ok {
					//	totalTokens = int(val)
					//}
					//
					//// Извлекаем информацию о кэшированных токенах
					//if inputDetails, ok := usage["input_tokens_details"].(map[string]interface{}); ok {
					//	if val, ok := inputDetails["cached_tokens"].(float64); ok {
					//		cachedTokens = int(val)
					//	}
					//}

					// Выводим в консоль с информацией о кэшировании
					//if cachedTokens > 0 {
					//	// Показываем экономию от кэширования
					//	logger.Debug("[TOKEN USAGE] Input: %d | Cached: %d (💰 90%% экономия!) | Output: %d | Total: %d",
					//		inputTokens, cachedTokens, outputTokens, totalTokens, userID)
					//} else {
					//	logger.Debug("[TOKEN USAGE] Input: %d | Output: %d | Total: %d",
					//		inputTokens, outputTokens, totalTokens, userID)
					//}
				} else {
					//logger.Warn("[CreateResponse] response.completed: поле response.usage отсутствует", userID)
				}
			} else {
				//logger.Warn("[CreateResponse] response.completed: поле response отсутствует", userID)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("error reading SSE stream: %w", err)
	}

	// Обрабатываем накопленные вызовы функций если есть
	if len(functionCallsMap) > 0 && onToolCall != nil {
		//logger.Debug("🔧 [CreateResponse] Обнаружено %d function calls, начинаю обработку...", len(functionCallsMap), userID)

		// Преобразуем map в массив (извлекаем все значения)
		var functionCallsArray []interface{}
		//for outputIndex, fn := range functionCallsMap {
		for _, fn := range functionCallsMap {
			// Преобразуем *StreamingFunctionCall в map[string]interface{} для совместимости с onToolCall
			toolCallMap := map[string]interface{}{
				"call_id":   fn.CallID,
				"name":      fn.Name,
				"arguments": fn.Arguments,
			}
			functionCallsArray = append(functionCallsArray, toolCallMap)
			//logger.Debug("[CreateResponse] Function call [output_index=%d]: name=%s, call_id=%s, args_length=%d",
			//	outputIndex, fn.Name, fn.CallID, len(fn.Arguments), userID)
		}

		// Вызываем обработчик функций
		//logger.Debug("🔧 [CreateResponse] Вызываю onToolCall с %d функциями...", len(functionCallsArray), userID)
		toolOutputs, err := onToolCall(functionCallsArray)
		if err != nil {
			//logger.Error("[CreateResponse] Ошибка в onToolCall: %v", err, userID)
			return nil, "", fmt.Errorf("tool call handler error: %w", err)
		}
		//logger.Debug("✅ [CreateResponse] onToolCall вернул %d результатов", len(toolOutputs), userID)

		// Отправляем результаты функций клиенту через streaming (ДО обработки моделью)
		if onDelta != nil {
			for _, output := range toolOutputs {
				if outputMap, ok := output.(map[string]interface{}); ok {
					callID, _ := outputMap["call_id"].(string)
					name, _ := outputMap["name"].(string)
					content, _ := outputMap["content"].(string)

					// Формируем JSON событие с результатом функции
					functionResult := map[string]interface{}{
						"type":      "function_result",
						"call_id":   callID,
						"name":      name,
						"content":   content,
						"timestamp": time.Now().Format(time.RFC3339),
					}

					resultJSON, err := json.Marshal(functionResult)
					if err == nil {
						// Отправляем результат клиенту через streaming
						if streamErr := onDelta(string(resultJSON)); streamErr != nil {
							//logger.Error("[CreateResponse] Ошибка при отправке результата функции клиенту: %v", streamErr, userID)
						}
					} else {
						//logger.Error("[CreateResponse] Ошибка при сериализации результата функции: %v", err, userID)
					}
				}
			}
		}

		// ВАЖНО: После выполнения функций нужно отправить результаты обратно в модель
		// Для Responses API формируем новый запрос с tool_outputs
		// Формат: добавляем в input информацию о результатах вызова функций

		// Собираем результаты функций в структурированный JSON контекст
		var toolResultsContext strings.Builder
		toolResultsContext.WriteString("\n\n## РЕЗУЛЬТАТЫ ВЫЗОВА ФУНКЦИЙ (используй их в финальном ответе!):\n```json\n")

		// Формируем массив результатов в JSON
		toolResults := make([]map[string]interface{}, 0, len(toolOutputs))
		for _, output := range toolOutputs {
			if outputMap, ok := output.(map[string]interface{}); ok {
				callID, _ := outputMap["call_id"].(string)
				content, _ := outputMap["content"].(string)

				// Парсим content как JSON если возможно
				var contentJSON interface{}
				if err := json.Unmarshal([]byte(content), &contentJSON); err == nil {
					toolResults = append(toolResults, map[string]interface{}{
						"call_id": callID,
						"result":  contentJSON,
					})
				} else {
					toolResults = append(toolResults, map[string]interface{}{
						"call_id": callID,
						"result":  content,
					})
				}
			}
		}

		// Сериализуем результаты в читаемый JSON
		if resultsJSON, err := json.MarshalIndent(toolResults, "", "  "); err == nil {
			toolResultsContext.Write(resultsJSON)
		}
		toolResultsContext.WriteString("\n```\n")
		toolResultsContext.WriteString("ИНСТРУКЦИЯ: Используй поля из result (file_name, Url, type) для заполнения action.send_files в финальном ответе!\n")

		// Создаём новый запрос с результатами функций
		// Модифицируем input чтобы включить контекст результатов
		newInput := input + toolResultsContext.String()

		// Рекурсивный вызов с результатами функций (увеличиваем глубину)
		return c.createResponseInternal(ctx, newInput, agentConfig, onDelta, onToolCall, userID, depth+1)
	}

	// Отправляем информацию о токенах клиенту в финальной дельте
	if tokenUsageData != nil && onDelta != nil {
		tokenUsage := map[string]interface{}{
			"type":  "token_usage",
			"usage": tokenUsageData,
		}

		if usageJSON, err := json.Marshal(tokenUsage); err == nil {
			if streamErr := onDelta(string(usageJSON)); streamErr != nil {
				//logger.Warn("[CreateResponse] Ошибка при отправке token_usage: %v", streamErr, userID)
				//} else {
				//	// Проверяем наличие cached_tokens для логирования
				//	hasCachedTokens := false
				//	if inputDetails, ok := tokenUsageData["input_tokens_details"].(map[string]interface{}); ok {
				//		if cached, ok := inputDetails["cached_tokens"].(float64); ok && cached > 0 {
				//			hasCachedTokens = true
				//		}
				//	}
				//
				//	if hasCachedTokens {
				//		logger.Debug("[CreateResponse] Отправлена информация о расходе токенов клиенту (с cached_tokens): %s", string(usageJSON), userID)
				//	} else {
				//		logger.Debug("[CreateResponse] Отправлена информация о расходе токенов клиенту: %s", string(usageJSON), userID)
				//	}
			}
		}
	}

	return result, fullText, nil
}

// GenerateModelSchema генерирует JSON Schema с учётом параметров модели
func GenerateModelSchema(hasMetaAction bool, hasOperator bool) map[string]interface{} {
	// Формируем список required полей
	requiredFields := []string{"message", "action", "target"}

	// operator добавляем в required только если он включен
	if hasOperator {
		requiredFields = append(requiredFields, "operator")
	}

	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"message": map[string]interface{}{
				"type": "string",
			},
			"action": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"send_files": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"type": map[string]interface{}{
									"type": "string",
									"enum": []string{"photo", "video", "audio", "doc"},
								},
								"Url": map[string]interface{}{
									"type": "string",
								},
								"file_name": map[string]interface{}{
									"type": "string",
								},
								"caption": map[string]interface{}{
									"type": "string",
								},
							},
							"required":             []string{"type", "Url", "file_name", "caption"},
							"additionalProperties": false,
						},
					},
				},
				"required":             []string{"send_files"},
				"additionalProperties": false,
			},
		},
		"required":             requiredFields,
		"additionalProperties": false,
	}

	// Настраиваем поле target
	if hasMetaAction {
		// Если есть MetaAction - target может быть true или false
		schema["properties"].(map[string]interface{})["target"] = map[string]interface{}{
			"type": "boolean",
		}
	} else {
		// Если нет MetaAction - target ВСЕГДА false
		schema["properties"].(map[string]interface{})["target"] = map[string]interface{}{
			"type": "boolean",
			"enum": []interface{}{false},
		}
	}

	// Настраиваем поле operator ТОЛЬКО если оно включено
	if hasOperator {
		// Если Operator включен - operator может быть true или false
		schema["properties"].(map[string]interface{})["operator"] = map[string]interface{}{
			"type": "boolean",
		}
	}
	// Если operator выключен - НЕ добавляем его в schema вообще!
	// Значение operator: false будет добавлено на стороне кода при парсинге ответа

	return schema
}

// createModel Создаю новую модель OpenAI Assistant
func (m *UniversalModel) createModel(_ uint32, modelData *UniversalModelData, _ []Ids) (UMCR, error) {
	// modelData уже распарсена и типизирована, используем напрямую

	// НОВЫЙ ПОДХОД: Генерируем эмбеддинги локально вместо использования Vector Store API
	// Файлы загружаются в OpenAI, затем их содержимое извлекается, генерируются эмбеддинги
	// и сохраняются в MariaDB для семантического поиска

	// Комментарий: В новом подходе с Chat Completions API НЕ создаётся Assistant в OpenAI.
	// AssistId в БД хранит имя модели (например "gpt-4o-mini"), а не ID ассистента.
	// Конфигурация (system_prompt, tools, response_format) формируется динамически
	// в методе buildAgentConfiguration (openai/model.go) при каждом запросе.

	// ВАЖНО: В новом подходе с Chat Completions API НЕ создаётся Assistant в OpenAI.
	// AssistId в БД хранит имя модели (например "gpt-4o-mini"), а не ID ассистента.
	// Конфигурация (system_prompt, tools, response_format) формируется динамически
	// в методе buildAgentConfiguration (openai/model.go) при каждом запросе.
	// Поэтому обновление через ModifyAssistant НЕ требуется и было удалено.

	// Для OpenAI с локальными эмбеддингами AllIds не используется
	// Эмбеддинги хранятся в таблице vector_embeddings с привязкой к ModelId
	// FileIds и VectorId больше не актуальны - Vector Store API не используется
	var allIds []byte = nil

	//logger.Debug("Конфигурация OpenAI модели создана: model=%s, embeddings=local",
	//	modelData.GptType.Name, userID)

	return UMCR{
		AssistID: modelData.GptType.Name, // Просто имя модели (gpt-4o-mini и т.д.)
		AllIds:   allIds,                 // null - не используется для OpenAI с локальными эмбеддингами
		Provider: ProviderOpenAI,
	}, nil
}

// deleteModel удаляет модель OpenAI и связанные ресурсы
func (m *UniversalModel) deleteModel(_ uint32, modelRecord *UserModelRecord, _ bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Удаление модели OpenAI...")
	}

	// Парсим VecIds из AllIds
	var vecIds VecIds
	if len(modelRecord.AllIds) > 0 {
		if err := json.Unmarshal(modelRecord.AllIds, &vecIds); err != nil {
			//logger.Warn("Ошибка парсинга VecIds: %v", err, userID)
		}
	}

	// Удаляем векторные эмбеддинги из MariaDB
	if progressCallback != nil {
		progressCallback("🔄 Удаление векторных эмбеддингов из БД...")
	}

	// Удаляем все эмбеддинги связанные с этой моделью
	if err := m.db.DeleteAllModelEmbeddings(modelRecord.ModelId); err != nil {
		//	logger.Warn("Ошибка удаления эмбеддингов модели %d: %v", modelRecord.ModelId, err, userID)
		//} else {
		//	logger.Debug("Векторные эмбеддинги модели %d успешно удалены из БД", modelRecord.ModelId, userID)
	}

	if progressCallback != nil {
		progressCallback("✅ Модель OpenAI успешно удалена")
	}

	//logger.Debug("Модель OpenAI успешно удалена (включая векторные эмбеддинги)", userID)
	return nil
}

// updateOpenAIModelInPlace обновляет OpenAI Assistant
func (m *UniversalModel) updateOpenAIModelInPlace(userID uint32, _, updated *UniversalModelData) error {

	// ВАЖНО: В новом подходе с Chat Completions API НЕ создаётся Assistant в OpenAI.
	// AssistId в БД хранит имя модели (например "gpt-4o-mini"), а не ID ассистента.
	// Конфигурация (system_prompt, tools, response_format) формируется динамически
	// в методе buildAgentConfiguration (openai/model.go) при каждом запросе.
	// Поэтому обновление через ModifyAssistant НЕ требуется и было удалено.

	// Получаем запись из БД для получения AssistId
	record, err := m.db.GetModelByProviderAnyStatus(userID, ProviderOpenAI)
	if err != nil {
		return fmt.Errorf("ошибка получения записи модели: %w", err)
	}
	if record == nil {
		return fmt.Errorf("запись модели не найдена")
	}

	// Для OpenAI с локальными эмбеддингами AllIds не используется
	// Оставляем существующее значение из БД без изменений
	umcr := UMCR{
		AssistID: record.AssistId,
		AllIds:   record.AllIds, // Сохраняем как было (для обратной совместимости)
		Provider: ProviderOpenAI,
	}

	// Сохраняем в БД
	if err := m.SaveModel(userID, umcr, updated); err != nil {
		return fmt.Errorf("ошибка сохранения обновленной модели в БД: %w", err)
	}

	return nil
}
