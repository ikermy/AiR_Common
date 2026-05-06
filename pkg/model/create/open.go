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

// NewOpenAIAgentClient создаёт новый OpenAI клиент с API ключом
func NewOpenAIAgentClient(ctx context.Context, apiKey string) *OpenAIAgentClient {
	return &OpenAIAgentClient{
		apiKey: apiKey,
		url:    "https://api.openai.com/v1",
		ctx:    ctx,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// GetAPIKey возвращает API ключ клиента (для использования в функциях генерации эмбеддингов)
func (c *OpenAIAgentClient) GetAPIKey() string {
	return c.apiKey
}

// doRequest выполняет HTTP запрос к OpenAI API
func (c *OpenAIAgentClient) doRequest(method, path string, body interface{}, userId uint32) (*http.Response, error) {
	url := c.url + path

	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}
		reqBody = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequestWithContext(c.ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "assistants=v2")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		//logger.Debug("OpenAI API error: HTTP %d, body: %s", resp.StatusCode, string(bodyBytes), userId)
		return nil, fmt.Errorf("OpenAI API error: HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	return resp, nil
}

// DeleteFile удаляет файл
func (c *OpenAIAgentClient) DeleteFile(ctx context.Context, fileID string) error {
	resp, err := c.doRequest("DELETE", fmt.Sprintf("/files/%s", fileID), nil, 0)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	return nil
}

// DownloadFileContent скачивает содержимое файла
func (c *OpenAIAgentClient) DownloadFileContent(ctx context.Context, fileID string, userId uint32) ([]byte, error) {
	resp, err := c.doRequest("GET", fmt.Sprintf("/files/%s/content", fileID), nil, userId)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	return io.ReadAll(resp.Body)
}

// TranscribeAudio транскрибирует аудио в текст
func (c *OpenAIAgentClient) TranscribeAudio(ctx context.Context, audioData []byte, fileName string, userId uint32) (string, error) {
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

	writer.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", c.url+"/audio/transcriptions", &buf)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
	_ context.Context,
	input string,
	agentConfig interface{}, // *OpenAIAgentConfig
	onDelta func(string) error,
	onToolCall func([]interface{}) ([]interface{}, error),
	userId uint32,
) (interface{}, string, error) {
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
		"model":               configMap["model_name"],
		"input":               input,
		"stream":              true,  // КРИТИЧНО: Включаем streaming
		"parallel_tool_calls": false, // TODO пока не работает, наверное нет подходящих функций
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
			//logger.Debug("[CreateResponse] Включен Extended Caching (24h) для модели %s", modelName, userId)
		} else {
			// Для остальных моделей (включая gpt-4.1-nano) НЕ указываем параметр
			// Кэширование всё равно работает автоматически (in_memory по умолчанию)
			//logger.Debug("[CreateResponse] Используется автоматическое кэширование (in_memory) для модели %s", modelName, userId)
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
			strict, _ := jsonSchema["strict"].(bool)

			payload["text"] = map[string]interface{}{
				"format": map[string]interface{}{
					"type":   "json_schema",
					"name":   name,
					"schema": schema,
					"strict": strict,
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
	//logger.Debug("[CreateResponse] Используется service_tier: %s", SERVICE_TIER, userId)

	// Выполняем streaming запрос к /responses
	resp, err := c.doRequest("POST", "/responses", payload, userId)
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
			//logger.Debug("[CreateResponse] Получен маркер [DONE], завершаем чтение SSE", userId)
			break
		}

		// Парсим JSON событие
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			//logger.Warn("[CreateResponse] Ошибка парсинга SSE события: %v, data: %s", err, data, userId)
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
									//logger.Warn("[CreateResponse] Ошибка при отправке response.output_item.added: %v", streamErr, userId)
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
								//logger.Warn("[CreateResponse] Ошибка при отправке response.function_call_arguments.delta: %v", streamErr, userId)
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
								//logger.Warn("[CreateResponse] Ошибка при отправке response.function_call_arguments.done: %v", streamErr, userId)
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
								//logger.Warn("[CreateResponse] Ошибка при отправке response.output_item.done: %v", streamErr, userId)
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
			//logger.Error("[CreateResponse] OpenAI API error: %s", errorMsg, userId)
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
					//logger.Error("[CreateResponse] Response failed: %s", errorMsg, userId)
					return nil, "", fmt.Errorf("response failed: %s", errorMsg)
				}
			}
			//logger.Error("[CreateResponse] Response failed: no error details", userId)
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
					//		inputTokens, cachedTokens, outputTokens, totalTokens, userId)
					//} else {
					//	logger.Debug("[TOKEN USAGE] Input: %d | Output: %d | Total: %d",
					//		inputTokens, outputTokens, totalTokens, userId)
					//}
				} else {
					//logger.Warn("[CreateResponse] response.completed: поле response.usage отсутствует", userId)
				}
			} else {
				//logger.Warn("[CreateResponse] response.completed: поле response отсутствует", userId)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("error reading SSE stream: %w", err)
	}

	// Обрабатываем накопленные вызовы функций если есть
	if len(functionCallsMap) > 0 && onToolCall != nil {
		//logger.Debug("🔧 [CreateResponse] Обнаружено %d function calls, начинаю обработку...", len(functionCallsMap), userId)

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
			//	outputIndex, fn.Name, fn.CallID, len(fn.Arguments), userId)
		}

		// Вызываем обработчик функций
		//logger.Debug("🔧 [CreateResponse] Вызываю onToolCall с %d функциями...", len(functionCallsArray), userId)
		toolOutputs, err := onToolCall(functionCallsArray)
		if err != nil {
			//logger.Error("[CreateResponse] Ошибка в onToolCall: %v", err, userId)
			return nil, "", fmt.Errorf("tool call handler error: %w", err)
		}
		//logger.Debug("✅ [CreateResponse] onToolCall вернул %d результатов", len(toolOutputs), userId)

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
							//logger.Error("[CreateResponse] Ошибка при отправке результата функции клиенту: %v", streamErr, userId)
						}
					} else {
						//logger.Error("[CreateResponse] Ошибка при сериализации результата функции: %v", err, userId)
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

		// Рекурсивный вызов с результатами функций
		return c.CreateResponse(c.ctx, newInput, agentConfig, onDelta, onToolCall, userId)
	}

	// Отправляем информацию о токенах клиенту в финальной дельте
	if tokenUsageData != nil && onDelta != nil {
		tokenUsage := map[string]interface{}{
			"type":  "token_usage",
			"usage": tokenUsageData,
		}

		if usageJSON, err := json.Marshal(tokenUsage); err == nil {
			if streamErr := onDelta(string(usageJSON)); streamErr != nil {
				//logger.Warn("[CreateResponse] Ошибка при отправке token_usage: %v", streamErr, userId)
			} else {
				// Проверяем наличие cached_tokens для логирования
				//hasCachedTokens := false
				//if inputDetails, ok := tokenUsageData["input_tokens_details"].(map[string]interface{}); ok {
				//	if cached, ok := inputDetails["cached_tokens"].(float64); ok && cached > 0 {
				//		hasCachedTokens = true
				//	}
				//}

				//if hasCachedTokens {
				//	logger.Debug("[CreateResponse] Отправлена информация о расходе токенов клиенту (с cached_tokens): %s", string(usageJSON), userId)
				//} else {
				//	logger.Debug("[CreateResponse] Отправлена информация о расходе токенов клиенту: %s", string(usageJSON), userId)
				//}
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

// buildOpenAITools строит список инструментов (tools) для OpenAI на основе параметров модели
// Эта функция переиспользуется в createModel и updateOpenAIModelInPlace для избежания дублирования кода
func buildOpenAITools(modelData *UniversalModelData, realUserID uint64) []AssistantTool {
	userIDStr := fmt.Sprintf("%d", realUserID)
	var tools []AssistantTool

	if modelData.Interpreter {
		tools = append(tools, AssistantTool{Type: "code_interpreter"})
	}

	if modelData.WebSearch {
		tools = append(tools, AssistantTool{Type: "web_search"})
	}

	// Добавляем функцию get_current_time ВСЕГДА (для получения актуального времени)
	tools = append(tools,
		AssistantTool{
			Type:      "function",
			Container: "function_tool",
			Function: &FunctionDefinition{
				Name: "get_current_time",
				Description: "Получает ТОЧНОЕ текущее время и дату с сервера в часовом поясе пользователя. " +
					"ОБЯЗАТЕЛЬНО используй эту функцию ПЕРЕД расчётом дат (завтра, через неделю, в понедельник и т.д.). " +
					"НЕ используй свои внутренние знания о дате - они УСТАРЕЛИ!",
				Strict: false,
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
							"const":       userIDStr,
						},
					},
					"required": []string{"user_id"},
				},
			},
		},
	)

	// Добавляем функции get_s3_files и create_file ТОЛЬКО если включен S3
	if modelData.S3 {
		tools = append(tools,
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "get_s3_files",
					Description: "Получает список доступных файлов пользователя из S3",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "ID пользователя",
								"const":       userIDStr, // Константа - ВСЕГДА это значение!
							},
						},
						"required": []string{"user_id"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "create_file",
					Description: "Создает текстовый файл и сохраняет в S3",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "ID пользователя",
								"const":       userIDStr, // Константа - ВСЕГДА это значение!
							},
							"content": map[string]interface{}{
								"type":        "string",
								"description": "Текстовое содержимое файла",
							},
							"file_name": map[string]interface{}{
								"type":        "string",
								"description": "Имя файла с расширением (.txt, .md и т.д.)",
							},
						},
						"required": []string{"user_id", "content", "file_name"},
					},
				},
			},
		)
	}

	// Добавляем функции Google Calendar если включен
	if modelData.GOAuth.HasCalendar() {
		tools = append(tools,
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "calendar_create_event",
					Description: "Создает новое событие в Google Calendar пользователя",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"title": map[string]interface{}{
								"type":        "string",
								"description": "Название события",
							},
							"description": map[string]interface{}{
								"type":        "string",
								"description": "Описание события (опционально)",
							},
							"start_time": map[string]interface{}{
								"type":        "string",
								"description": "Время начала в RFC3339 формате",
							},
							"end_time": map[string]interface{}{
								"type":        "string",
								"description": "Время окончания в RFC3339 формате",
							},
							"location": map[string]interface{}{
								"type":        "string",
								"description": "Место проведения (опционально)",
							},
						},
						"required": []string{"user_id", "title", "start_time", "end_time"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "calendar_list_events",
					Description: "Получает список событий из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"time_min": map[string]interface{}{
								"type":        "string",
								"description": "Начало периода в RFC3339 (опционально)",
							},
							"time_max": map[string]interface{}{
								"type":        "string",
								"description": "Конец периода в RFC3339 (опционально)",
							},
							"max_results": map[string]interface{}{
								"type":        "integer",
								"description": "Максимальное количество событий (по умолчанию 10)",
							},
						},
						"required": []string{"user_id"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "calendar_delete_event",
					Description: "Удаляет событие из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"event_id": map[string]interface{}{
								"type":        "string",
								"description": "ID события для удаления",
							},
						},
						"required": []string{"user_id", "event_id"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "calendar_get_event",
					Description: "Получает детали события из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"event_id": map[string]interface{}{
								"type":        "string",
								"description": "ID события для получения деталей",
							},
						},
						"required": []string{"user_id", "event_id"},
					},
				},
			},
		)
	}

	// Добавляем функции Google Sheets если включен
	if modelData.GOAuth.HasSheets() {
		tools = append(tools,
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "sheets_read_range",
					Description: "Читает данные из указанного диапазона в Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
							"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
							"range":          map[string]interface{}{"type": "string", "description": "Диапазон для чтения"},
						},
						"required": []string{"user_id", "spreadsheet_id", "range"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "sheets_write_range",
					Description: "Записывает данные в указанный диапазон Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
							"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
							"range":          map[string]interface{}{"type": "string", "description": "Начальная ячейка"},
							"values": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range", "values"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "sheets_append_range",
					Description: "Добавляет данные в конец Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
							"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
							"range":          map[string]interface{}{"type": "string", "description": "Диапазон колонок"},
							"values": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range", "values"},
					},
				},
			},
		)
	}

	return tools
}

// createModel Создаю новую модель OpenAI Assistant
func (m *UniversalModel) createModel(userId uint32, modelData *UniversalModelData, fileIDs []Ids) (UMCR, error) {
	// modelData уже распарсена и типизирована, используем напрямую

	// Получаем real_user_id для использования в инструкциях
	realUserID, err := m.GetRealUserID(userId)
	if err != nil {
		//logger.Warn("Не удалось получить real_user_id: %v", err, userId)
		realUserID = uint64(userId) // Fallback на обычный userId
	}

	// Используем универсальную функцию для создания ОБЩЕЙ части промпта
	enhancedPrompt := BuildEnhancedPrompt(modelData, realUserID)

	// НОВЫЙ ПОДХОД: Генерируем эмбеддинги локально вместо использования Vector Store API
	// Файлы загружаются в OpenAI, затем их содержимое извлекается, генерируются эмбеддинги
	// и сохраняются в MariaDB для семантического поиска

	// Извлекаю id[]string из fileIDs для сохранения в БД (для обратной совместимости)
	var ids []string
	for _, fileID := range fileIDs {
		if fileID.ID != "" {
			ids = append(ids, fileID.ID)
		}
	}

	// Генерируем динамическую JSON Schema с учётом параметров модели
	hasMetaAction := modelData.MetaAction != ""
	hasOperator := modelData.Operator
	dynamicSchema := GenerateModelSchema(hasMetaAction, hasOperator)
	schemaJSON, err := json.Marshal(dynamicSchema)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка сериализации JSON Schema: %w", err)
	}

	// DEBUG: логируем сгенерированную схему
	//logger.Debug("Generated JSON Schema (hasMetaAction=%v, hasOperator=%v): %s",
	//	userId, hasMetaAction, hasOperator, string(schemaJSON), userId)

	// Добавляем OpenAI-специфичную часть промпта (JSON Schema и примеры)
	enhancedPrompt += BuildOpenAIPromptSuffix(modelData, schemaJSON)

	// Комментарий: В новом подходе с Chat Completions API НЕ создаётся Assistant в OpenAI.
	// AssistId в БД хранит имя модели (например "gpt-4o-mini"), а не ID ассистента.
	// Конфигурация (system_prompt, tools, response_format) формируется динамически
	// в методе buildAgentConfiguration (openai/model.go) при каждом запросе.

	// Добавляем функцию get_current_time ВСЕГДА (для получения актуального времени)
	userIDStr := fmt.Sprintf("%d", realUserID)
	// Используем helper функцию для генерации инструментов (избегаем дублирования с updateOpenAIModelInPlace)
	tools := buildOpenAITools(modelData, realUserID)
	tools = append(tools,
		AssistantTool{
			Type:      "function",
			Container: "function_tool",
			Function: &FunctionDefinition{
				Name: "get_current_time",
				Description: "Получает ТОЧНОЕ текущее время и дату с сервера в часовом поясе пользователя. " +
					"ОБЯЗАТЕЛЬНО используй эту функцию ПЕРЕД расчётом дат (завтра, через неделю, в понедельник и т.д.). " +
					"НЕ используй свои внутренние знания о дате - они УСТАРЕЛИ!",
				Strict: false,
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
							"const":       userIDStr,
						},
					},
					"required": []string{"user_id"},
				},
			},
		},
	)

	// Добавляем функции get_s3_files и create_file ТОЛЬКО если включен S3
	if modelData.S3 {
		// Используем уже созданный userIDStr

		tools = append(tools,
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "get_s3_files",
					Description: "Получает список доступных файлов пользователя из S3",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "ID пользователя",
								"const":       userIDStr, // Константа - ВСЕГДА это значение!
							},
						},
						"required": []string{"user_id"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "create_file",
					Description: "Создает текстовый файл и сохраняет в S3",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "ID пользователя",
								"const":       userIDStr, // Константа - ВСЕГДА это значение!
							},
							"content": map[string]interface{}{
								"type":        "string",
								"description": "Текстовое содержимое файла",
							},
							"file_name": map[string]interface{}{
								"type":        "string",
								"description": "Имя файла с расширением (.txt, .md и т.д.)",
							},
						},
						"required": []string{"user_id", "content", "file_name"},
					},
				},
			},
		)
	}

	// Добавляем функции Google Calendar если включен
	if modelData.GOAuth.HasCalendar() {
		tools = append(tools,
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "calendar_create_event",
					Description: "Создает новое событие в Google Calendar пользователя",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"title": map[string]interface{}{
								"type":        "string",
								"description": "Название события",
							},
							"description": map[string]interface{}{
								"type":        "string",
								"description": "Описание события (опционально)",
							},
							"start_time": map[string]interface{}{
								"type":        "string",
								"description": "Время начала в RFC3339 формате",
							},
							"end_time": map[string]interface{}{
								"type":        "string",
								"description": "Время окончания в RFC3339 формате",
							},
							"location": map[string]interface{}{
								"type":        "string",
								"description": "Место проведения (опционально)",
							},
						},
						"required": []string{"user_id", "title", "start_time", "end_time"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "calendar_list_events",
					Description: "Получает список событий из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"time_min": map[string]interface{}{
								"type":        "string",
								"description": "Начало периода в RFC3339 (опционально)",
							},
							"time_max": map[string]interface{}{
								"type":        "string",
								"description": "Конец периода в RFC3339 (опционально)",
							},
							"max_results": map[string]interface{}{
								"type":        "integer",
								"description": "Максимальное количество событий (по умолчанию 10)",
							},
						},
						"required": []string{"user_id"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "calendar_delete_event",
					Description: "Удаляет событие из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"event_id": map[string]interface{}{
								"type":        "string",
								"description": "ID события для удаления",
							},
						},
						"required": []string{"user_id", "event_id"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "calendar_get_event",
					Description: "Получает детали события из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"event_id": map[string]interface{}{
								"type":        "string",
								"description": "ID события для получения деталей",
							},
						},
						"required": []string{"user_id", "event_id"},
					},
				},
			},
		)
	}

	// Добавляем функции Google Sheets если включен
	if modelData.GOAuth.HasSheets() {
		tools = append(tools,
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "sheets_read_range",
					Description: "Читает данные из Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
							"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
							"range":          map[string]interface{}{"type": "string", "description": "Диапазон (например: 'Лиды!A:F' или 'Sheet1!A1:D10')"},
						},
						"required": []string{"user_id", "spreadsheet_id", "range"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "sheets_write_range",
					Description: "Записывает данные в Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
							"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
							"range":          map[string]interface{}{"type": "string", "description": "Диапазон для записи"},
							"values": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range", "values"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "sheets_append_range",
					Description: "Добавляет данные в конец Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
							"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
							"range":          map[string]interface{}{"type": "string", "description": "Диапазон колонок"},
							"values": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range", "values"},
					},
				},
			},
		)
	}

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
	//	modelData.GptType.Name, userId)

	return UMCR{
		AssistID: modelData.GptType.Name, // Просто имя модели (gpt-4o-mini и т.д.)
		AllIds:   allIds,                 // null - не используется для OpenAI с локальными эмбеддингами
		Provider: ProviderOpenAI,
	}, nil
}

// deleteModel удаляет модель OpenAI и связанные ресурсы
func (m *UniversalModel) deleteModel(userId uint32, modelRecord *UserModelRecord, deleteFiles bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Удаление модели OpenAI...")
	}

	// Парсим VecIds из AllIds
	var vecIds VecIds
	if len(modelRecord.AllIds) > 0 {
		if err := json.Unmarshal(modelRecord.AllIds, &vecIds); err != nil {
			//logger.Warn("Ошибка парсинга VecIds: %v", err, userId)
		}
	}

	// Удаляем векторные эмбеддинги из MariaDB
	if progressCallback != nil {
		progressCallback("🔄 Удаление векторных эмбеддингов из БД...")
	}

	// Удаляем все эмбеддинги связанные с этой моделью
	if err := m.db.DeleteAllModelEmbeddings(modelRecord.ModelId); err != nil {
		//	logger.Warn("Ошибка удаления эмбеддингов модели %d: %v", modelRecord.ModelId, err, userId)
		//} else {
		//	logger.Debug("Векторные эмбеддинги модели %d успешно удалены из БД", modelRecord.ModelId, userId)
	}

	if progressCallback != nil {
		progressCallback("✅ Модель OpenAI успешно удалена")
	}

	//logger.Debug("Модель OpenAI успешно удалена (включая векторные эмбеддинги)", userId)
	return nil
}

// updateOpenAIModelInPlace обновляет OpenAI Assistant
func (m *UniversalModel) updateOpenAIModelInPlace(userId uint32, existing, updated *UniversalModelData) error {
	// Получаем real_user_id для использования в инструкциях
	realUserID, err := m.GetRealUserID(userId)
	if err != nil {
		//logger.Warn("Не удалось получить real_user_id: %v", err, userId)
		//realUserID = uint64(userId) // Fallback
	}

	// Переменные для инструментов
	userIDStr := fmt.Sprintf("%d", realUserID)
	var tools []AssistantTool

	// Автоматически генерируем системные инструкции (ТА ЖЕ ЛОГИКА ЧТО В createModel)
	enhancedPrompt := updated.Prompt + "\n\n"

	// Добавляем важное напоминание
	if updated.MetaAction != "" || updated.Operator {
		enhancedPrompt += "##IMPORTANT REMINDER:\n" +
			"In EVERY response you MUST:\n"

		if updated.MetaAction != "" {
			enhancedPrompt += "1. Check GOAL condition (from your instructions above) and set target correctly\n"
		}

		if updated.Operator {
			enhancedPrompt += "2. Check if operator needed (from your instructions above) and set operator correctly\n"
		}

		enhancedPrompt += "3. DO NOT IGNORE these checks!\n\n"
	}

	// Добавляем инструкции по работе с S3 файлами
	if updated.S3 {
		enhancedPrompt += "## S3 FILES OPERATIONS:\n\n" +
			"### Two file types:\n" +
			"1. **Existing files** (found via get_s3_files) - use their real URLs\n" +
			"2. **Created files** (via create_file) - use URL from function response\n\n" +
			"### File operations algorithm:\n" +
			"1. To get files list call: get_s3_files() - no parameters\n" +
			"2. To create new file call: create_file({\"content\": \"...\", \"file_name\": \"...txt\"})\n" +
			"3. For existing files use URL from get_s3_files response\n" +
			"4. For created files use URL from create_file response\n\n" +
			"### File type detection:\n" +
			"- .jpg, .jpeg, .png, .gif, .webp, .bmp -> \"photo\"\n" +
			"- .mp4, .avi, .mov, .webm, .mkv -> \"video\"\n" +
			"- .mp3, .wav, .flac, .aac, .ogg -> \"audio\"\n" +
			"- Others -> \"doc\"\n\n"
	}

	// Добавляем инструкции по Code Interpreter
	if updated.Interpreter {
		enhancedPrompt += "## CODE INTERPRETER:\n" +
			"You can execute Python code for:\n" +
			"- Data analysis and calculations\n" +
			"- Creating charts and visualizations\n" +
			"- Processing files (CSV, Excel, JSON, etc.)\n" +
			"- Generating result files\n\n" +
			"Files created via Code Interpreter are automatically available in response.\n\n"
	}

	// Добавляем инструкции по GOOGLE CALENDAR
	if updated.GOAuth.HasCalendar() {
		enhancedPrompt += "## GOOGLE CALENDAR - Event Management:\n" +
			"You have access to user's Google Calendar.\n\n" +
			fmt.Sprintf("**user_id for all functions: \"%d\"** (string)\n\n", realUserID) +
			"### Available functions:\n" +
			"- calendar_create_event - create event\n" +
			"- calendar_list_events - list events\n" +
			"- calendar_delete_event - delete event\n" +
			"- calendar_get_event - event details\n\n" +
			"### IMPORTANT:\n" +
			"- Time format: RFC3339 (e.g.: \"2026-02-05T15:00:00+03:00\")\n" +
			"- ALWAYS call get_current_time BEFORE calculating dates!\n" +
			"- After create/delete confirm action\n\n" +
			"### CRITICAL - EVENT DELETION:\n" +
			"When user asks to DELETE event:\n" +
			"1. FORBIDDEN to create new events (calendar_create_event)\n" +
			"2. Deletion algorithm:\n" +
			"   a) FIRST get events list: calendar_list_events\n" +
			"   b) Find required event_id in results\n" +
			"   c) THEN delete each: calendar_delete_event(user_id, event_id)\n" +
			"3. To delete \"all today's events\":\n" +
			"   - Call get_current_time\n" +
			"   - Get today's events via calendar_list_events\n" +
			"   - Delete each via calendar_delete_event\n" +
			"4. DO NOT create events on deletion requests!\n\n"
	}

	// Добавляем инструкции по GOOGLE SHEETS
	if updated.GOAuth.HasSheets() {
		enhancedPrompt += "## GOOGLE SHEETS - Spreadsheet Operations:\n" +
			"You have access to user's Google Sheets.\n\n" +
			fmt.Sprintf("**user_id for all functions: \"%d\"** (string)\n\n", realUserID) +
			"\n" +
			"CRITICALLY IMPORTANT - ALWAYS CALL FUNCTIONS!\n" +
			"STRICTLY FORBIDDEN:\n" +
			"\"Unfortunately, I cannot determine number of rows\"\n" +
			"\"No access to user account\"\n" +
			"\"I cannot get data from spreadsheet\"\n" +
			"\"Please ensure I have access\"\n" +
			"Answer WITHOUT calling sheets_read_range\n\n" +
			"CORRECT ACTIONS:\n" +
			"1. Question \"what's in spreadsheet\" -> IMMEDIATELY call sheets_read_range\n" +
			"2. Question \"how many rows\" -> MUST call sheets_read_range and count len(values)-1\n" +
			"3. DO NOT answer from prompt - CALL FUNCTION and get REAL data!\n" +
			"4. YOU ALREADY HAVE ACCESS via functions!\n\n" +
			"spreadsheet_id find in prompt or user request\n" +
			"Range: 'Leads!A:F' or 'Sheet1!A:Z'\n\n" +
			"### Functions:\n" +
			"- sheets_read_range - read data\n" +
			"- sheets_write_range - write\n" +
			"- sheets_append_range - append rows\n" +
			"═══════════════════════════════════════════════════════════\n\n"
	}

	// Добавляем базовые инструменты на основе флагов
	// ПРИМЕЧАНИЕ: file_search больше НЕ используется для OpenAI
	// Семантический поиск выполняется через RAG (см. комментарий в createModel)

	if updated.Interpreter {
		tools = append(tools, AssistantTool{Type: "code_interpreter"})
	}

	if updated.WebSearch {
		tools = append(tools, AssistantTool{Type: "web_search"})
	}

	// Добавляем функции Google Calendar если включен
	if updated.GOAuth.HasCalendar() {
		tools = append(tools,
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "calendar_create_event",
					Description: "Создает новое событие в Google Calendar пользователя",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"title": map[string]interface{}{
								"type":        "string",
								"description": "Название события",
							},
							"description": map[string]interface{}{
								"type":        "string",
								"description": "Описание события (опционально)",
							},
							"start_time": map[string]interface{}{
								"type":        "string",
								"description": "Время начала в RFC3339 формате",
							},
							"end_time": map[string]interface{}{
								"type":        "string",
								"description": "Время окончания в RFC3339 формате",
							},
							"location": map[string]interface{}{
								"type":        "string",
								"description": "Место проведения (опционально)",
							},
						},
						"required": []string{"user_id", "title", "start_time", "end_time"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "calendar_list_events",
					Description: "Получает список событий из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"time_min": map[string]interface{}{
								"type":        "string",
								"description": "Начало периода в RFC3339 (опционально)",
							},
							"time_max": map[string]interface{}{
								"type":        "string",
								"description": "Конец периода в RFC3339 (опционально)",
							},
							"max_results": map[string]interface{}{
								"type":        "integer",
								"description": "Максимальное количество событий (по умолчанию 10)",
							},
						},
						"required": []string{"user_id"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "calendar_delete_event",
					Description: "Удаляет событие из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"event_id": map[string]interface{}{
								"type":        "string",
								"description": "ID события для удаления",
							},
						},
						"required": []string{"user_id", "event_id"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "calendar_get_event",
					Description: "Получает детали события из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"event_id": map[string]interface{}{
								"type":        "string",
								"description": "ID события для получения деталей",
							},
						},
						"required": []string{"user_id", "event_id"},
					},
				},
			},
		)
	}

	// Добавляем функции Google Sheets если включен
	if updated.GOAuth.HasSheets() {
		tools = append(tools,
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "sheets_read_range",
					Description: "Читает данные из Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
							"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
							"range":          map[string]interface{}{"type": "string", "description": "Диапазон (например: 'Лиды!A:F' или 'Sheet1!A1:D10')"},
						},
						"required": []string{"user_id", "spreadsheet_id", "range"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "sheets_write_range",
					Description: "Записывает данные в Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
							"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
							"range":          map[string]interface{}{"type": "string", "description": "Диапазон для записи"},
							"values": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range", "values"},
					},
				},
			},
			AssistantTool{
				Type:      "function",
				Container: "function_tool",
				Function: &FunctionDefinition{
					Name:        "sheets_append_range",
					Description: "Добавляет данные в конец Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
							"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
							"range":          map[string]interface{}{"type": "string", "description": "Диапазон колонок"},
							"values": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range", "values"},
					},
				},
			},
		)
	}

	// ВАЖНО: В новом подходе с Chat Completions API НЕ создаётся Assistant в OpenAI.
	// AssistId в БД хранит имя модели (например "gpt-4o-mini"), а не ID ассистента.
	// Конфигурация (system_prompt, tools, response_format) формируется динамически
	// в методе buildAgentConfiguration (openai/model.go) при каждом запросе.
	// Поэтому обновление через ModifyAssistant НЕ требуется и было удалено.

	// Получаем запись из БД для получения AssistId
	record, err := m.db.GetModelByProviderAnyStatus(userId, ProviderOpenAI)
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
	if err := m.SaveModel(userId, umcr, updated); err != nil {
		return fmt.Errorf("ошибка сохранения обновленной модели в БД: %w", err)
	}

	return nil
}
