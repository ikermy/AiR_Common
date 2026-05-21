package google

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// DialogMessage представляет сообщение из истории диалога (формат БД)
type DialogMessage struct {
	Creator   interface{}            `json:"creator"`   // 1 = "assistant", 2 = "user", или строка "user"/"assistant"
	Message   map[string]interface{} `json:"message"`   // AssistResponse в виде map
	Timestamp string                 `json:"timestamp"` // ISO 8601 timestamp
}

// GetCreator возвращает creator в виде строки (нормализует 1->assistant, 2->user)
func (dm *DialogMessage) GetCreator() string {
	if creator, ok := dm.Creator.(float64); ok {
		// JSON парсит числа как float64
		if creator == 1 {
			return "assistant"
		} else if creator == 2 {
			return "user"
		}
	} else if creator, ok := dm.Creator.(string); ok {
		return creator
	}
	return "user" // По умолчанию
}

// Request отправляет запрос к Google Gemini с учетом истории диалога
// Основной метод для взаимодействия с моделью
// google не хранит модели на своей стороне, поэтому modelId игнорируется
// ОПТИМИЗАЦИЯ: История диалога кэшируется локально в памяти с LiveTTL для избежания постоянных обращений к БД
// Использует RequestStreaming с буферизацией для получения финального ответа
func (m *Model) Request(userID uint32, dialogID uint64, text string, files ...model.FileUpload) (model.AssistResponse, error) {
	return model.StreamingToSync(text, files, func(onDelta func(string, bool) error, files ...model.FileUpload) error {
		return m.RequestStreaming(userID, dialogID, text, onDelta, files...)
	})
}

// ConvertDialogToGoogleFormat конвертирует историю из БД в формат Google Gemini
func (m *Model) ConvertDialogToGoogleFormat(dialogID uint64) ([]GoogleContent, error) {
	// Читаем историю из БД
	dialogData, err := m.db.ReadDialog(dialogID, create.DialogHistoryLimit)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения диалога: %w", err)
	}

	if len(dialogData) == 0 {
		return nil, nil // Пустая история
	}

	// Используем базовый парсер для консолидации логики парсинга
	parsedMessages, err := model.ParseDialogHistory(dialogData)
	if err != nil {
		return nil, fmt.Errorf("ошибка парсинга истории: %w", err)
	}

	// Конвертируем парсенные сообщения в формат Google
	var contents []GoogleContent
	for _, msg := range parsedMessages {
		// Нормализуем creator:
		//   1 = AI (text)          → "model"
		//   2 = User (text)        → "user"
		//   3 = UserVoice          → "user"
		//   4 = Operator           → "model"
		//   5 = SpeechRealTimeAI   → "model"
		//   6 = SpeechRealTimeUser → "user"
		role := "user"
		if creator, ok := msg.Creator.(float64); ok {
			switch int(creator) {
			case 1, 4, 5: // AI, Operator, SpeechRealTimeAI
				role = "model"
			}
		} else if creator, ok := msg.Creator.(string); ok {
			if creator == "assistant" || creator == "model" {
				role = "model"
			}
		}

		// Извлекаем текст сообщения
		var messageText string
		if msgInterface, ok := msg.Message.(map[string]interface{}); ok {
			if msgStr, ok := msgInterface["message"].(string); ok {
				messageText = msgStr
			}
		}

		if messageText == "" {
			continue // Пропускаем пустые сообщения
		}

		// Формируем parts
		parts := []map[string]interface{}{
			{"text": messageText},
		}

		contents = append(contents, GoogleContent{
			Role:  role,
			Parts: parts,
		})
	}

	return contents, nil
}

// createUserMessage создаёт сообщение пользователя в формате Google
func (m *Model) createUserMessage(text string, files []model.FileUpload) GoogleContent {
	parts := []map[string]interface{}{
		{"text": text},
	}

	// Добавляем поддержку файлов (изображений)
	for _, file := range files {
		// Если это изображение с URL - используем fileUri
		if file.HasURL() && file.IsImageMimeType() {
			parts = append(parts, map[string]interface{}{
				"fileData": map[string]string{
					"mimeType": file.MimeType,
					"fileUri":  file.URL,
				},
			})
		} else if file.Content != nil {
			// Для файлов без URL - читаем байты и используем inline_data
			data, err := io.ReadAll(file.Content)
			if err != nil {
				//logger.Warn("Не удалось прочитать содержимое файла %s: %v, пропускаем", file.Name, err)
				continue
			}
			parts = append(parts, map[string]interface{}{
				"inline_data": map[string]string{
					"mime_type": file.MimeType,
					"data":      base64.StdEncoding.EncodeToString(data),
				},
			})
		}
	}

	return GoogleContent{
		Role:  "user",
		Parts: parts,
	}
}

// createModelMessage создаёт сообщение модели в формате Google Gemini
func (m *Model) createModelMessage(assistResponse model.AssistResponse) GoogleContent {
	// Извлекаем текстовое сообщение
	messageText := assistResponse.Message
	if messageText == "" {
		messageText = "(null answer)"
	}

	parts := []map[string]interface{}{
		{"text": messageText},
	}

	return GoogleContent{
		Role:  "model",
		Parts: parts,
	}
}

// sendToGeminiAPI отправляет запрос к Google Gemini API
// Автоматически обрабатывает ошибку 429 (quota exceeded) с retry логикой
func (m *Model) sendToGeminiAPI(modelName string, payload map[string]interface{}, userID uint32) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s",
		m.client.GetUrl(), modelName, m.client.GetAPIKeyForUser(userID))

	// Попытка запроса с автоматическим retry для ошибки 429
	maxRetries := 2
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, url, bytes.NewBuffer(body))
		if err != nil {
			return nil, fmt.Errorf("ошибка создания запроса: %v", err)
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("ошибка HTTP запроса: %v", err)
		}

		responseBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if err != nil {
			return nil, fmt.Errorf("ошибка чтения ответа: %v", err)
		}

		if resp.StatusCode == http.StatusOK {
			return responseBody, nil
		}

		// Обработка ошибки 429 (quota exceeded)
		if resp.StatusCode == 429 && attempt < maxRetries {
			// Пытаемся извлечь retryDelay из ответа
			var errorResp struct {
				Error struct {
					Details []map[string]interface{} `json:"details"`
				} `json:"error"`
			}

			retryDelay := 5 * time.Second // По умолчанию 5 секунд

			if json.Unmarshal(responseBody, &errorResp) == nil {
				for _, detail := range errorResp.Error.Details {
					if detail["@type"] == "type.googleapis.com/google.rpc.RetryInfo" {
						if retryDelayStr, ok := detail["retryDelay"].(string); ok {
							// Парсим "11s" или "27.077507321s" в time.Duration
							if duration, err := time.ParseDuration(retryDelayStr); err == nil {
								retryDelay = duration
							}
						}
					}
				}
			}

			//logger.Warn("Квота Google API превышена (429), retry через %v (попытка %d/%d)",
			//	retryDelay, attempt+1, maxRetries)

			time.Sleep(retryDelay)
			continue // Повторяем запрос
		}

		// Другие ошибки или последняя попытка
		return nil, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return nil, fmt.Errorf("превышено количество попыток retry")
}

// sendToGeminiAPIStreaming отправляет запрос к Google Gemini API с поддержкой SSE стриминга
// Использует endpoint streamGenerateContent для получения ответа в режиме реального времени
// onDelta вызывается для каждого delta-события, onComplete - для финального ответа с токенами
// Возвращает: fullText, usageMetadata, functionCalls, error
func (m *Model) sendToGeminiAPIStreaming(modelName string, payload map[string]interface{}, onDelta func(delta string) error, userID uint32) (string, map[string]interface{}, []map[string]interface{}, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, nil, fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	// Используем streamGenerateContent для SSE
	// m.client.GetUrl() уже содержит версию API (v1beta), поэтому не добавляем её повторно
	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse&key=%s",
		m.client.GetUrl(), modelName, m.client.GetAPIKeyForUser(userID))

	// Попытка запроса с автоматическим retry для ошибки 429
	maxRetries := 2
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, url, bytes.NewBuffer(body))
		if err != nil {
			return "", nil, nil, fmt.Errorf("ошибка создания запроса: %v", err)
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", nil, nil, fmt.Errorf("ошибка HTTP запроса: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			responseBody, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			// Обработка ошибки 429 (quota exceeded)
			if resp.StatusCode == 429 && attempt < maxRetries {
				var errorResp struct {
					Error struct {
						Details []map[string]interface{} `json:"details"`
					} `json:"error"`
				}

				retryDelay := 5 * time.Second

				if json.Unmarshal(responseBody, &errorResp) == nil {
					for _, detail := range errorResp.Error.Details {
						if detail["@type"] == "type.googleapis.com/google.rpc.RetryInfo" {
							if retryDelayStr, ok := detail["retryDelay"].(string); ok {
								if duration, err := time.ParseDuration(retryDelayStr); err == nil {
									retryDelay = duration
								}
							}
						}
					}
				}

				//logger.Warn("Квота Google API превышена (429), retry через %v (попытка %d/%d)",
				//	retryDelay, attempt+1, maxRetries, userID)

				time.Sleep(retryDelay)
				continue
			}

			return "", nil, nil, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
		}

		// Обрабатываем SSE поток в отдельной функции, чтобы defer корректно
		// закрывал тело ответа в конце каждой итерации, а не в конце внешней функции.
		fullText, usageMetadata, functionCalls, err := func(body io.ReadCloser) (string, map[string]interface{}, []map[string]interface{}, error) {
			defer func() { _ = body.Close() }()

			scanner := bufio.NewScanner(body)
			// Увеличиваем буфер для обработки больших SSE-событий (по умолчанию 64KB может быть недостаточно)
			const maxCapacity = 512 * 1024 // 512 KB
			buf := make([]byte, maxCapacity)
			scanner.Buffer(buf, maxCapacity)

			var fullText strings.Builder
			var usageMetadata map[string]interface{}
			var functionCalls []map[string]interface{}

			eventCount := 0

			for scanner.Scan() {
				line := scanner.Text()

				// SSE формат: "data: {...}"
				if !strings.HasPrefix(line, "data: ") {
					continue
				}

				// Извлекаем JSON после "data: "
				data := strings.TrimPrefix(line, "data: ")
				if data == "" || data == "[DONE]" {
					continue
				}

				eventCount++

				// Парсим SSE событие
				var sseEvent struct {
					Candidates []struct {
						Content struct {
							Parts []struct {
								Text         string                 `json:"text,omitempty"`
								FunctionCall map[string]interface{} `json:"functionCall,omitempty"`
							} `json:"parts"`
						} `json:"content"`
					} `json:"candidates"`
					UsageMetadata map[string]interface{} `json:"usageMetadata,omitempty"`
				}

				if err := json.Unmarshal([]byte(data), &sseEvent); err != nil {
					//logger.Warn("[SSE] Ошибка парсинга SSE события: %v, data: %s", err, data, userID)
					continue
				}

				// Извлекаем текстовую дельту
				if len(sseEvent.Candidates) > 0 && len(sseEvent.Candidates[0].Content.Parts) > 0 {
					for _, part := range sseEvent.Candidates[0].Content.Parts {
						if part.Text != "" {
							fullText.WriteString(part.Text)

							// Отправляем сырую JSON-дельту клиенту в реальном времени
							// Клиент сам будет накапливать и парсить финальный JSON
							if onDelta != nil {
								if err := onDelta(part.Text); err != nil {
									//logger.Warn("[SSE] Ошибка в onDelta callback: %v", err, userID)
									return "", nil, nil, err
								}
							}
						}

						// Обрабатываем function calls (если есть)
						// ВАЖНО: Gemini присылает functionCall целиком в одном чанке
						// В отличие от OpenAI, аргументы НЕ стримятся по кусочкам
						if part.FunctionCall != nil {
							//logger.Debug("[SSE] Получен function call: %+v", part.FunctionCall, userID)

							// Сохраняем function call для возврата (для multi-turn conversation)
							functionCalls = append(functionCalls, part.FunctionCall)

							// Извлекаем имя функции и аргументы
							functionName := ""
							if name, ok := part.FunctionCall["name"].(string); ok {
								functionName = name
							}

							// Сериализуем аргументы в JSON строку
							argsJSON, _ := json.Marshal(part.FunctionCall["args"])

							// КРИТИЧЕСКИ ВАЖНО: arguments должен быть СТРОКОЙ JSON (как в OpenAI)
							// Экранируем для сохранения как строки через multiple JSON parsing layers
							argsJSONString := string(argsJSON)
							// Сначала экранируем обратные слеши, затем кавычки
							escapedArgs := strings.ReplaceAll(argsJSONString, `\`, `\\`)
							escapedArgs = strings.ReplaceAll(escapedArgs, `"`, `\"`)

							// Формируем событие в OpenAI-совместимом формате
							// ВАЖНО: arguments обёрнут в кавычки - это СТРОКА JSON, не объект!
							functionCallEvent := fmt.Sprintf(`{"type":"response.function_call_arguments.done","response_id":"gemini-%d","item_id":"fc-%d","output_index":0,"name":"%s","arguments":"%s"}`,
								time.Now().Unix(),
								time.Now().UnixNano(),
								functionName,
								escapedArgs,
							)

							// Отправляем как JSON строку клиенту (точно так же, как в OpenAI)
							if onDelta != nil {
								if err := onDelta(functionCallEvent); err != nil {
									//logger.Warn("[SSE] Ошибка при отправке function_call: %v", err, userID)
									return "", nil, nil, err
								}
								//logger.Debug("📨 [SSE] Function call отправлен клиенту: name=%s, args_len=%d",
								//	functionName, len(argsJSON), userID)
							}
						}
					}
				}

				// Сохраняем метаданные использования токенов (приходят в последнем чанке)
				if sseEvent.UsageMetadata != nil {
					usageMetadata = sseEvent.UsageMetadata
				}
			}

			if err := scanner.Err(); err != nil {
				return "", nil, nil, fmt.Errorf("ошибка чтения SSE потока: %w", err)
			}

			return fullText.String(), usageMetadata, functionCalls, nil
		}(resp.Body)
		if err != nil {
			return "", nil, nil, err
		}

		return fullText, usageMetadata, functionCalls, nil
	}

	return "", nil, nil, fmt.Errorf("превышено количество попыток retry")
}

// parseGeminiResponseWithFunctionHandling парсит ответ и обрабатывает function calls через multi-turn conversation
// Если модель вызывает функцию без текста, отправляем результат обратно модели для продолжения
func (m *Model) parseGeminiResponseWithFunctionHandling(responseBody []byte, history []GoogleContent,
	payload map[string]interface{}, modelName string, provider create.ProviderType, userID uint32) (model.AssistResponse, error) {

	var emptyResponse model.AssistResponse

	var apiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string                 `json:"text,omitempty"`
					FunctionCall map[string]interface{} `json:"functionCall,omitempty"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.Unmarshal(responseBody, &apiResp); err != nil {
		return emptyResponse, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	//logger.Debug("parseGeminiResponseWithFunctionHandling: получено %d candidates от Google Gemini API", len(apiResp.Candidates))

	if len(apiResp.Candidates) == 0 || len(apiResp.Candidates[0].Content.Parts) == 0 {
		return emptyResponse, fmt.Errorf("получен пустой ответ от модели")
	}

	// Собираем текстовые ответы и function calls
	var textParts []string
	var functionCalls []map[string]interface{}

	for _, part := range apiResp.Candidates[0].Content.Parts {
		if part.Text != "" {
			textParts = append(textParts, part.Text)
		}
		if part.FunctionCall != nil {
			functionCalls = append(functionCalls, part.FunctionCall)
		}
	}

	//logger.Debug("parseGeminiResponseWithFunctionHandling: собрано %d текстовых частей и %d функций", len(textParts), len(functionCalls))

	// Если есть function calls БЕЗ текста - отправляем результаты модели для продолжения
	if len(functionCalls) > 0 && len(textParts) == 0 {
		// Добавляем model response в историю со ВСЕМИ функциями
		modelResponseParts := make([]map[string]interface{}, len(functionCalls))
		for i, fc := range functionCalls {
			modelResponseParts[i] = map[string]interface{}{"functionCall": fc}
		}

		history = append(history, GoogleContent{
			Role:  "model",
			Parts: modelResponseParts,
		})

		// Обрабатываем все функции и собираем результаты
		for _, fc := range functionCalls {
			result, err := m.handleFunctionCall(fc, provider, userID)
			if err != nil {
				//logger.Warn("Ошибка обработки function call: %v", err)
				continue
			}

			// Добавляем результат функции в историю (в правильном формате для Google Gemini)
			// Google использует functionResponse (не functionResult)
			history = append(history, GoogleContent{
				Role: "user",
				Parts: []map[string]interface{}{
					{
						"functionResponse": map[string]interface{}{
							"name":     fc["name"],
							"response": result,
						},
					},
				},
			})
		}

		// Отправляем обновленный payload с результатами
		payload["contents"] = history
		response, err := m.sendToGeminiAPI(modelName, payload, userID)
		if err != nil {
			return emptyResponse, fmt.Errorf("ошибка повторного запроса к Gemini API: %w", err)
		}

		// Рекурсивно парсим ответ (модель должна вернуть текст)
		return m.parseGeminiResponseWithFunctionHandling(response, history, payload, modelName, provider, userID)
	}

	// Если есть function calls И текст - обрабатываем функции (но текст используем как ответ)
	if len(functionCalls) > 0 && len(textParts) > 0 {
		//logger.Debug("Модель вернула текст и вызвала функции")
		for _, fc := range functionCalls {
			//result, err := m.handleFunctionCall(fc, provider, userID)
			_, err := m.handleFunctionCall(fc, provider, userID)
			if err != nil {
				//logger.Warn("Ошибка обработки function call: %v", err)
				continue
			}

			// Проверяем это generate_video
			//if action, ok := result["action"].(string); ok && action == "generate_video" {
			//	logger.Debug("Обнаружен запрос на генерацию видео: %+v", result)
			//}
		}
	}

	// Объединяем текстовые части
	fullText := strings.Join(textParts, "\n")

	if fullText == "" {
		return emptyResponse, fmt.Errorf("получен пустой текст от модели")
	}

	// Пытаемся распарсить как JSON (если модель вернула структурированный ответ)
	var assistResp model.AssistResponse
	var rawResp map[string]interface{}

	// Сначала пытаемся распарсить ПЕРВУЮ текстовую часть как JSON
	// (модель может отправить текст + JSON в разных частях)
	parsedJSON := false
	if len(textParts) > 0 {
		// Проверяем первую часть
		err := json.Unmarshal([]byte(textParts[0]), &rawResp)
		if err == nil {
			parsedJSON = true
		} else {
			// Пытаемся найти JSON в markdown блоке первой части
			jsonText := extractJSONFromMarkdown(textParts[0])
			err = json.Unmarshal([]byte(jsonText), &rawResp)
			if err == nil {
				parsedJSON = true
			}
		}
	}

	// Если не удалось распарсить первую часть, пытаемся весь объединенный текст
	if !parsedJSON {
		jsonText := fullText
		err := json.Unmarshal([]byte(jsonText), &rawResp)
		if err != nil {
			jsonText = extractJSONFromMarkdown(fullText)
			err = json.Unmarshal([]byte(jsonText), &rawResp)
		}
		if err == nil {
			parsedJSON = true
		}
	}

	if parsedJSON {
		// Успешно распарсили как JSON объект
		// Извлекаем поля из JSON (модель может использовать "message" вместо "Message")
		if msg, ok := rawResp["message"].(string); ok {
			assistResp.Message = msg
		}

		// Парсим action если есть
		if actionData, ok := rawResp["action"].(map[string]interface{}); ok {
			if sendFiles, ok := actionData["send_files"].([]interface{}); ok {
				for _, fileIface := range sendFiles {
					if fileMap, ok := fileIface.(map[string]interface{}); ok {
						file := model.File{
							Type:     model.FileType(getStringField(fileMap, "type")),
							URL:      getStringField(fileMap, "url"),
							FileName: getStringField(fileMap, "file_name"),
							Caption:  getStringField(fileMap, "caption"),
						}
						assistResp.Action.SendFiles = append(assistResp.Action.SendFiles, file)
					}
				}
				//logger.Debug("Всего добавлено файлов в assistResp: %d", len(assistResp.Action.SendFiles))
			}
		}

		// Парсим target и operator
		if target, ok := rawResp["target"].(bool); ok {
			assistResp.Meta = target
		}
		if operator, ok := rawResp["operator"].(bool); ok {
			assistResp.Operator = operator
		}
	} else {
		// Если не JSON, создаём простой ответ
		assistResp = model.AssistResponse{
			Message: fullText,
			Action: model.Action{
				SendFiles: []model.File{},
			},
			Meta:     false,
			Operator: false,
		}
	}

	return assistResp, nil
}

// handleFunctionCall обрабатывает вызов функции от модели
func (m *Model) handleFunctionCall(functionCall map[string]interface{}, provider create.ProviderType, userID uint32) (map[string]interface{}, error) {
	functionName, ok := functionCall["name"].(string)
	if !ok {
		return nil, fmt.Errorf("function call не содержит имени")
	}

	argsInterface, ok := functionCall["args"]
	if !ok {
		return nil, fmt.Errorf("function call не содержит аргументов")
	}

	argsJSON, err := json.Marshal(argsInterface)
	if err != nil {
		return nil, fmt.Errorf("ошибка сериализации аргументов: %v", err)
	}

	// Все функции обрабатываются через action handler
	if m.actionHandler != nil {
		result := m.actionHandler.RunAction(m.ctx, functionName, string(argsJSON), provider, userID)

		var resultMap map[string]interface{}
		if err := json.Unmarshal([]byte(result), &resultMap); err != nil {
			// Если результат не JSON, оборачиваем в объект
			resultMap = map[string]interface{}{
				"result": result,
			}
		}

		//logger.Debug("Function %s выполнена, результат: %s", functionName, result)
		return resultMap, nil
	}

	return nil, fmt.Errorf("action handler не инициализирован")
}

// processVideoGeneration автоматически генерирует видео если модель вызвала generate_video
// или если в промпте агента включен флаг Video и обнаружены ключевые слова
func (m *Model) processVideoGeneration(userID uint32, userText string, response model.AssistResponse, agentConfig *GoogleAgentConfig, provider create.ProviderType) (model.AssistResponse, error) {
	// Проверяем включена ли генерация видео в конфигурации
	if !m.isVideoEnabled(agentConfig) {
		return response, nil
	}

	// Проверяем есть ли ключевые слова для генерации видео
	shouldGenerate := false
	userTextLower := strings.ToLower(userText)
	videoKeywords := []string{"видео", "video", "сгенерируй видео", "создай видео", "нарисуй видео"}

	for _, keyword := range videoKeywords {
		if strings.Contains(userTextLower, keyword) {
			shouldGenerate = true
			break
		}
	}

	if !shouldGenerate {
		return response, nil
	}

	//logger.Debug("processVideoGeneration: начинаем генерацию видео", userID)

	// Извлекаем параметры для генерации
	prompt := m.extractVideoPrompt(userText, response.Message)
	aspectRatio := "16:9"
	duration := 4

	// Пробуем извлечь параметры из текста пользователя
	if strings.Contains(userTextLower, "вертикал") || strings.Contains(userTextLower, "9:16") {
		aspectRatio = "9:16"
	} else if strings.Contains(userTextLower, "квадрат") || strings.Contains(userTextLower, "1:1") {
		aspectRatio = "1:1"
	}

	if strings.Contains(userTextLower, "8 секунд") || strings.Contains(userTextLower, "8 сек") {
		duration = 8
	} else if strings.Contains(userTextLower, "6 секунд") {
		duration = 6
	}

	//logger.Debug("processVideoGeneration: параметры - prompt='%s', aspect=%s, duration=%d", prompt, aspectRatio, duration)

	// Генерируем видео через клиент
	//videoData, mimeType, err := m.client.GenerateVideo(prompt, aspectRatio, duration)
	videoData, _, err := m.client.GenerateVideo(prompt, aspectRatio, duration)
	if err != nil {
		//logger.Error("processVideoGeneration: ошибка генерации видео: %v", err)
		response.Message += fmt.Sprintf("\n\n⚠️ К сожалению, не удалось сгенерировать видео: %v", err)
		return response, err
	}

	//logger.Debug("processVideoGeneration: видео успешно сгенерировано: %d bytes, %s", len(videoData), mimeType)

	// Сохраняем видео через save_image_data (используем тот же механизм)
	// TODO: Можно создать отдельный save_video_data endpoint
	fileName := fmt.Sprintf("video_%d_%d.mp4", userID, time.Now().Unix())

	// Кодируем в base64 для передачи
	videoBase64 := base64.StdEncoding.EncodeToString(videoData)

	args := fmt.Sprintf(`{"image_data":"%s","file_name":"%s"}`,
		videoBase64, fileName)

	result := m.actionHandler.RunAction(m.ctx, "save_image_data", args, provider, userID)

	// Парсим результат сохранения
	var saveResult struct {
		Success bool   `json:"success"`
		URL     string `json:"url"`
		Error   string `json:"error"`
	}

	// Пробуем распарсить как JSON
	if err := json.Unmarshal([]byte(result), &saveResult); err != nil {
		// Если не JSON, возможно это просто URL
		saveResult.URL = strings.TrimSpace(result)
		saveResult.Success = saveResult.URL != "" && !strings.Contains(saveResult.URL, "error")
	}

	if saveResult.Success && saveResult.URL != "" {
		//logger.Debug("processVideoGeneration: видео сохранено: URL=%s", saveResult.URL)

		// Добавляем в send_files
		response.Action.SendFiles = append(response.Action.SendFiles, model.File{
			Type:     "video",
			URL:      saveResult.URL,
			FileName: fileName,
			Caption:  fmt.Sprintf("🎬 Сгенерированное видео: %s", prompt),
		})

		// Обновляем сообщение
		response.Message += "\n\n✅ Видео успешно создано!"
	} else {
		//logger.Error("processVideoGeneration: ошибка сохранения видео: %s", saveResult.Error)
		response.Message += "\n\n⚠️ Видео сгенерировано, но не удалось сохранить."
	}

	return response, nil
}

// extractVideoPrompt извлекает промпт для генерации видео из текста
func (m *Model) extractVideoPrompt(userText, modelResponse string) string {
	// Приоритет 1: Ищем описание в ответе модели после ключевых фраз
	modelResponseLower := strings.ToLower(modelResponse)
	triggers := []string{"генерирую видео:", "creating video:", "video:", "описание:"}

	for _, trigger := range triggers {
		if strings.Contains(modelResponseLower, trigger) {
			parts := strings.Split(modelResponse, trigger)
			if len(parts) > 1 {
				description := strings.TrimSpace(strings.Split(parts[1], "\n")[0])
				if description != "" && len(description) > 5 {
					return description
				}
			}
		}
	}

	// Приоритет 2: Очищаем запрос пользователя от команд
	prompt := userText
	cleanWords := []string{
		"создай видео", "сгенерируй видео", "нарисуй видео", "покажи видео",
		"сделай видео", "создай", "сгенерируй", "нарисуй",
		"create video", "generate video", "make video",
	}

	userTextLower := strings.ToLower(userText)
	for _, word := range cleanWords {
		userTextLower = strings.ReplaceAll(userTextLower, word, "")
	}
	prompt = strings.TrimSpace(userTextLower)

	// Удаляем параметры
	prompt = strings.Split(prompt, "вертикал")[0]
	prompt = strings.Split(prompt, "горизонтал")[0]
	prompt = strings.Split(prompt, "квадрат")[0]
	prompt = strings.Split(prompt, "секунд")[0]
	prompt = strings.TrimSpace(prompt)

	if prompt == "" || len(prompt) < 3 {
		prompt = "beautiful cinematic scene"
	}

	return prompt
}

// isVideoEnabled проверяет включена ли генерация видео в конфигурации агента
func (m *Model) isVideoEnabled(config *GoogleAgentConfig) bool {
	if config == nil || config.SystemInstruction == nil {
		return false
	}

	// Проверяем наличие инструкций по видео в system_instruction
	sysInstr := fmt.Sprintf("%v", config.SystemInstruction)
	return strings.Contains(sysInstr, "ГЕНЕРАЦИЯ ВИДЕО") || strings.Contains(sysInstr, "VIDEO GENERATION")
}

// getStringField извлекает строковое значение из map
func getStringField(m map[string]interface{}, key string) string {
	if val, ok := m[key].(string); ok {
		return val
	}
	return ""
}

// processImageGeneration автоматически генерирует изображение если модель включила Image
// и обнаружены ключевые слова в запросе пользователя
func (m *Model) processImageGeneration(userID uint32, userText string, response model.AssistResponse, agentConfig *GoogleAgentConfig, provider create.ProviderType) (model.AssistResponse, error) {
	// Проверяем включена ли генерация изображений в конфигурации
	if !agentConfig.Image {
		return response, nil
	}

	// Проверяем есть ли ключевые слова для генерации изображения
	shouldGenerate := false
	userTextLower := strings.ToLower(userText)
	imageKeywords := []string{
		"нарисуй", "изобрази", "сгенерируй изображение", "создай изображение",
		"нарисуй картинку", "создай картинку", "покажи картинку",
		"draw", "generate image", "create image",
	}

	for _, keyword := range imageKeywords {
		if strings.Contains(userTextLower, keyword) {
			shouldGenerate = true
			break
		}
	}

	if !shouldGenerate {
		return response, nil
	}

	//logger.Debug("processImageGeneration: начинаем генерацию изображения", userID)

	// Извлекаем промпт для генерации (из запроса пользователя или ответа модели)
	prompt := m.extractImagePrompt(userText, response.Message)

	// Извлекаем aspect ratio если указан
	aspectRatio := "1:1" // По умолчанию квадрат для изображений
	if strings.Contains(userTextLower, "вертикал") || strings.Contains(userTextLower, "9:16") {
		aspectRatio = "9:16"
	} else if strings.Contains(userTextLower, "горизонтал") || strings.Contains(userTextLower, "16:9") {
		aspectRatio = "16:9"
	}

	//logger.Debug("processImageGeneration: параметры - prompt='%s', aspect=%s", prompt, aspectRatio)

	// Генерируем изображение через Google Imagen API
	imageData, mimeType, err := m.client.GenerateImage(prompt, aspectRatio)
	if err != nil {
		//logger.Error("processImageGeneration: ошибка генерации изображения: %v", err)
		response.Message += fmt.Sprintf("\n\n⚠️ К сожалению, не удалось сгенерировать изображение: %v", err)
		return response, err
	}

	//logger.Debug("processImageGeneration: изображение успешно сгенерировано: %d bytes, %s", len(imageData), mimeType)

	// Определяем расширение файла из MIME type
	ext := "png"
	if strings.Contains(mimeType, "jpeg") || strings.Contains(mimeType, "jpg") {
		ext = "jpg"
	}

	fileName := fmt.Sprintf("image_%d_%d.%s", userID, time.Now().Unix(), ext)

	// Кодируем в base64 для передачи в save_image_data
	imageBase64 := base64.StdEncoding.EncodeToString(imageData)

	// Сохраняем через action handler
	args := fmt.Sprintf(`{"image_data":"%s","file_name":"%s"}`,
		imageBase64, fileName)

	result := m.actionHandler.RunAction(m.ctx, "save_image_data", args, provider, userID)

	// Парсим результат сохранения
	var saveResult struct {
		Success bool   `json:"success"`
		URL     string `json:"url"`
		Error   string `json:"error"`
	}

	// Пробуем распарсить как JSON
	if err := json.Unmarshal([]byte(result), &saveResult); err != nil {
		// Если не JSON, возможно это просто URL
		saveResult.URL = strings.TrimSpace(result)
		saveResult.Success = saveResult.URL != "" && !strings.Contains(saveResult.URL, "error")
	}

	if saveResult.Success && saveResult.URL != "" {
		//logger.Debug("processImageGeneration: изображение сохранено: URL=%s", saveResult.URL)

		// Удаляем все fake URL из send_files (example.com, placeholder и т.д.)
		cleanedFiles := []model.File{}
		for _, file := range response.Action.SendFiles {
			// Пропускаем fake URL
			if !strings.Contains(file.URL, "example.com") &&
				!strings.Contains(file.URL, "placeholder") &&
				!(strings.HasPrefix(file.URL, "http://") && file.Type == "photo") {
				cleanedFiles = append(cleanedFiles, file)
				//} else {
				//	logger.Debug("processImageGeneration: удалён fake URL: %s", file.URL)
			}
		}
		response.Action.SendFiles = cleanedFiles

		// Добавляем реальное изображение
		response.Action.SendFiles = append(response.Action.SendFiles, model.File{
			Type:     "photo",
			URL:      saveResult.URL,
			FileName: fileName,
			Caption:  response.Message, // Используем message модели как caption
		})

		// Очищаем message чтобы не дублировать в caption
		if response.Message != "" {
			response.Message = ""
		}

		//logger.Debug("processImageGeneration: добавлено реальное изображение в send_files")
	} else {
		//logger.Error("processImageGeneration: ошибка сохранения изображения: %s", saveResult.Error)
		response.Message += "\n\n⚠️ Изображение сгенерировано, но не удалось сохранить."
	}

	return response, nil
}

// extractImagePrompt извлекает промпт для генерации изображения из текста
func (m *Model) extractImagePrompt(userText, modelResponse string) string {
	// Приоритет 1: Ищем описание в ответе модели
	modelResponseLower := strings.ToLower(modelResponse)
	triggers := []string{"создаю изображение:", "генерирую:", "drawing:", "creating image:", "описание:"}

	for _, trigger := range triggers {
		if strings.Contains(modelResponseLower, trigger) {
			parts := strings.Split(modelResponse, trigger)
			if len(parts) > 1 {
				description := strings.TrimSpace(strings.Split(parts[1], "\n")[0])
				if description != "" && len(description) > 5 {
					return description
				}
			}
		}
	}

	// Приоритет 2: Очищаем запрос пользователя от команд
	prompt := userText
	cleanWords := []string{
		"нарисуй", "изобрази", "сгенерируй изображение", "создай изображение",
		"нарисуй картинку", "создай картинку", "покажи картинку",
		"draw", "generate image", "create image", "мне", "пожалуйста",
	}

	for _, word := range cleanWords {
		prompt = strings.ReplaceAll(strings.ToLower(prompt), strings.ToLower(word), "")
	}

	prompt = strings.TrimSpace(prompt)

	// Если после очистки промпт слишком короткий, используем оригинал
	if len(prompt) < 5 {
		return userText
	}

	return prompt
}

// extractJSONFromMarkdown извлекает JSON из markdown блока ```json...``` если он есть
// Возвращает очищенный JSON для парсинга (без markdown)
func extractJSONFromMarkdown(text string) string {
	// Проверяем наличие markdown блока
	if strings.HasPrefix(strings.TrimSpace(text), "```") {
		// Удаляем открывающий блок ```json или ```
		lines := strings.Split(text, "\n")
		if len(lines) > 0 {
			// Пропускаем первую строку если это ```json или ```
			start := 0
			if strings.HasPrefix(strings.TrimSpace(lines[0]), "```") {
				start = 1
			}

			// Пропускаем последнюю строку если это ```
			end := len(lines)
			if end > start && strings.TrimSpace(lines[end-1]) == "```" {
				end--
			}

			// Объединяем оставшиеся строки
			if start < end {
				return strings.Join(lines[start:end], "\n")
			}
		}
	}

	return text
}

type ragResp struct {
	contextText string
	err         error
	history     []GoogleContent
	resp        *GoogleRespModel
	realuserID  uint64
	// Метрики производительности
	embeddingDuration     time.Duration
	searchDuration        time.Duration
	historyLoadDuration   time.Duration
	responderLoadDuration time.Duration
	cacheHit              bool
}

func (m *Model) applyRAG(userID uint32, dialogID uint64, text string, ch chan<- ragResp) {
	defer close(ch)

	result := ragResp{}

	// === 1. Получаем real_user_id ===
	var realuserID uint64
	if m.universalModel != nil {
		var err error
		realuserID, err = m.universalModel.GetRealUserID(userID)
		if err != nil {
			//logger.Warn("applyRAG: не удалось получить real_user_id: %v, используем userID", err, userID)
			realuserID = uint64(userID)
		}
	} else {
		realuserID = uint64(userID)
	}
	result.realuserID = realuserID

	// === 2. Загружаем историю диалога (параллельно с эмбеддингами) ===
	historyStart := time.Now()
	var history []GoogleContent
	if cachedHistory, found := m.getDialogHistoryFromCache(dialogID); found {
		history = cachedHistory
		//logger.Debug("applyRAG: история загружена из кэша (%d сообщений)", len(history), userID)
	} else {
		// Получаем respId для загрузки истории из БД
		respId, err := m.GetRespIdBydialogID(dialogID)
		if err != nil {
			select {
			case <-m.ctx.Done():
			case ch <- ragResp{err: fmt.Errorf("applyRAG: респондент не найден для dialogID %d: %w", dialogID, err)}:
			}
			return
		}

		// Получаем респондента для проверки конфигурации
		_, ok := m.responders.Load(respId)
		if !ok {
			select {
			case <-m.ctx.Done():
			case ch <- ragResp{err: fmt.Errorf("applyRAG: респондент не найден в кэше для respId %d", respId)}:
			}
			return
		}

		// Загружаем историю из БД
		dbHistory, err := m.ConvertDialogToGoogleFormat(dialogID)
		if err != nil {
			//logger.Warn("applyRAG: не удалось загрузить историю диалога %d из БД: %v, используем пустую историю", dialogID, err, userID)
			history = []GoogleContent{}
		} else {
			history = dbHistory
			//logger.Debug("applyRAG: история загружена из БД (%d сообщений)", len(history), userID)
		}

		// Ограничиваем историю
		maxMessages := int(create.DialogHistoryLimit)
		if len(history) > maxMessages {
			history = history[len(history)-maxMessages:]
		}

		// Сохраняем в кэш
		cache := m.getOrCreateDialogCache(dialogID)
		cache.Contents = history
	}
	result.history = history
	result.historyLoadDuration = time.Since(historyStart)

	// === 3. Получаем респондента ===
	responderStart := time.Now()
	respId, err := m.GetRespIdBydialogID(dialogID)
	if err != nil {
		select {
		case <-m.ctx.Done():
		case ch <- ragResp{err: fmt.Errorf("applyRAG: респондент не найден для dialogID %d: %w", dialogID, err)}:
		}
		return
	}

	respVal, ok := m.responders.Load(respId)
	if !ok {
		select {
		case <-m.ctx.Done():
		case ch <- ragResp{err: fmt.Errorf("applyRAG: респондент не найден в кэше для respId %d", respId)}:
		}
		return
	}
	resp := respVal.(*GoogleRespModel)

	if resp.AgentConfig == nil {
		select {
		case <-m.ctx.Done():
		case ch <- ragResp{err: fmt.Errorf("applyRAG: конфигурация агента не загружена")}:
		}
		return
	}
	result.resp = resp
	result.responderLoadDuration = time.Since(responderStart)

	// === 4. Проверяем нужен ли RAG ===
	if !resp.AgentConfig.HasVector || len(resp.AgentConfig.VectorIds) == 0 || text == "" {
		//logger.Debug("applyRAG: RAG не требуется (HasVector=%v, VectorIds=%d, text=%q)",
		//	resp.AgentConfig.HasVector, len(resp.AgentConfig.VectorIds), text != "", userID)
		// Отправляем результат без RAG контекста
		select {
		case <-m.ctx.Done():
		case ch <- result:
		}
		return
	}

	// === 5. Генерируем эмбеддинг запроса ===
	embeddingStart := time.Now()
	queryEmbedding, err := m.GenerateEmbedding(userID, text)
	result.embeddingDuration = time.Since(embeddingStart)

	if err != nil {
		//logger.Warn("applyRAG: ошибка генерации эмбеддинга: %v, продолжаем без RAG", err, userID)
		result.err = fmt.Errorf("ошибка генерации эмбеддинга для RAG: %v", err)
		select {
		case <-m.ctx.Done():
		case ch <- result:
		}
		return
	}

	// Проверяем был ли cache hit (через логи GenerateEmbedding)
	// Эта информация уже залогирована в GenerateEmbedding

	// === 6. Ищем похожие документы ===
	searchStart := time.Now()
	relevantDocs, err := m.searchSimilarEmbeddings(resp.AgentConfig.ModelId, queryEmbedding, create.SimilarEmbeddingsLimit)
	result.searchDuration = time.Since(searchStart)

	if err != nil {
		//logger.Warn("applyRAG: ошибка поиска похожих эмбеддингов: %v, продолжаем без RAG", err, userID)
		result.err = fmt.Errorf("ошибка поиска похожих эмбеддингов для RAG: %v", err)
		select {
		case <-m.ctx.Done():
		case ch <- result:
		}
		return
	}

	// === 7. Формируем обогащённый контекст ===
	if len(relevantDocs) > 0 {
		var relevantChunks []string
		for _, doc := range relevantDocs {
			relevantChunks = append(relevantChunks, doc.Content)
		}

		contextText := strings.Join(relevantChunks, "\n\n---\n\n")
		enhancedText := fmt.Sprintf(`Relevant knowledge base context:
%s
---
User query: %s`, contextText, text)

		result.contextText = enhancedText

		//totalDuration := time.Since(totalStart)
		//logger.Debug("[USER:%d] ⚡ applyRAG завершён за %v | История: %v | Респондент: %v | Эмбеддинг: %v | Поиск: %v | Найдено документов: %d (%d символов)",
		//	userID, totalDuration, result.historyLoadDuration, result.responderLoadDuration,
		//	result.embeddingDuration, result.searchDuration, len(relevantDocs), len(contextText))
		//} else {
		//	logger.Debug("applyRAG: похожие документы не найдены", userID)
	}

	// Отправляем результат
	select {
	case <-m.ctx.Done():
	case ch <- result:
	}
}

// RequestStreaming выполняет запрос с потоковой передачей через SSE (Server-Sent Events)
// Использует Google Gemini streamGenerateContent API для получения ответов в реальном времени
// onDelta вызывается для каждого delta-события, в финальной дельте передаются данные о токенах
func (m *Model) RequestStreaming(userID uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...model.FileUpload) error {
	if text == "" && len(files) == 0 {
		return fmt.Errorf("пустое сообщение и нет файлов")
	}

	// ============================================================================
	// ОПТИМИЗАЦИЯ: Запускаем applyRAG как можно раньше для параллельного выполнения
	// всех тяжёлых операций (загрузка истории, получение респондента, эмбеддинги)
	// ============================================================================
	ragCh := make(chan ragResp, 1)
	go m.applyRAG(userID, dialogID, text, ragCh)

	// Ждём результат RAG из горутины
	// Он содержит: history, resp, realuserID, contextText и метрики производительности
	var ragResult ragResp
	select {
	case <-m.ctx.Done():
		return fmt.Errorf("контекст отменён")
	case ragResult = <-ragCh:
		if ragResult.err != nil {
			// Критическая ошибка (не удалось загрузить историю или респондента)
			return fmt.Errorf("ошибка в applyRAG: %w", ragResult.err)
		}
	case <-time.After(create.ApplayRAGTimeaut): // Увеличенный таймаут для тяжёлых операций
		return fmt.Errorf("таймаут ожидания результата applyRAG")
	}

	// Используем данные из applyRAG
	history := ragResult.history
	resp := ragResult.resp

	// Создаём callback для выполнения функций через MCP action handler.
	// ВАЖНО: определяем ПОСЛЕ получения resp из applyRAG, чтобы иметь доступ к
	// resp.Assist.Provider и userID для вызова RunAction.
	onToolCall := func(toolCalls []interface{}) ([]interface{}, error) {
		//logger.Debug("🔧 [RequestStreaming/Google] ВЫЗВАН onToolCall! Количество tool calls: %d", len(toolCalls), userID)
		var toolOutputs []interface{}
		for _, toolCall := range toolCalls {
			toolCallMap, ok := toolCall.(map[string]interface{})
			if !ok {
				continue
			}
			callID, _ := toolCallMap["call_id"].(string)
			functionName, _ := toolCallMap["name"].(string)
			arguments, _ := toolCallMap["arguments"].(string)

			var result string
			if m.actionHandler == nil {
				result = `{"error": "action handler not initialized"}`
			} else {
				result = m.actionHandler.RunAction(m.ctx, functionName, arguments, resp.Assist.Provider, userID)
			}

			//logger.Debug("🔧 [Google] Выполнена функция %s → %s", functionName, result, userID)
			toolOutputs = append(toolOutputs, map[string]interface{}{
				"call_id": callID,
				"name":    functionName,
				"content": result,
			})
		}
		//logger.Debug("🔧 [RequestStreaming/Google] ЗАВЕРШЁН! Возвращаю %d результатов", len(toolOutputs), userID)
		return toolOutputs, nil
	}

	// Обновляем TTL респондента
	resp.TTL = time.Now().Add(m.UserModelTTl)

	// Формируем enhancedText
	enhancedText := text

	// Если RAG нашёл контекст - используем его
	if ragResult.contextText != "" {
		enhancedText = ragResult.contextText
		//logger.Info("[USER:%d] RAG: добавлено контекста (%d символов)", userID, len(ragResult.contextText))
	}

	// Ждём результат RAG из горутины (он может прийти раньше, позже или вообще не прийти если что-то пошло по дуге)
	ragContent := ""
	select {
	case <-m.ctx.Done():
	case ragRes := <-ragCh:
		if ragRes.err != nil {
			//logger.Warn("RAG error: %v, продолжаем без RAG", ragRes.err, userID)
		} else if ragRes.contextText != "" {
			//logger.Debug("RAG результат получен, добавлено контекста: %d символов", len(ragRes.contextText), userID)
			ragContent = ragRes.contextText
		}
	case <-time.After(10 * time.Second): // Таймаут на RAG, чтобы не ждать слишком долго
	}

	// Если RAG результат получен - добавляем его в начало enhancedText
	if ragContent != "" {
		enhancedText = ragContent + "\n" + enhancedText
	}

	// Добавляем новое сообщение пользователя (с обогащённым текстом если был RAG)
	userMessage := m.createUserMessage(enhancedText, files)
	history = append(history, userMessage)

	// Сохраняем в кэш
	m.addMessageToCache(dialogID, userMessage)

	// ВАЖНО: Формируем payload ПОСЛЕ всех модификаций history!
	// Сначала добавляем конфигурацию агента
	payload := map[string]interface{}{}

	if resp.AgentConfig.SystemInstruction != nil {
		payload["system_instruction"] = resp.AgentConfig.SystemInstruction
	}

	if resp.AgentConfig.GenerationConfig != nil {
		payload["generationConfig"] = resp.AgentConfig.GenerationConfig
	}

	hasTools := len(resp.AgentConfig.Tools) > 0

	if hasTools {
		payload["tools"] = resp.AgentConfig.Tools

		if genConfig, ok := payload["generationConfig"].(map[string]interface{}); ok {
			delete(genConfig, "response_schema")
			delete(genConfig, "response_mime_type")
		}

		// Добавляем JSON reminder в начало истории
		// Инструкции по инструментам (calendar, sheets и пр.) приходят от MCP через FetchSystemPrompt —
		// здесь только базовое напоминание о формате JSON-ответа.
		jsonReminderText := "IMPORTANT: All responses MUST be strictly in JSON format according to schema:\n" + create.GoogleSchemaJSON + "\n\nNever respond with plain text!"

		jsonReminderMessage := GoogleContent{
			Role: "user",
			Parts: []map[string]interface{}{
				{
					"text": jsonReminderText,
				},
			},
		}
		jsonReminderResponse := GoogleContent{
			Role: "model",
			Parts: []map[string]interface{}{
				{
					"text": `{"message":"Understood, all my responses will be strictly in JSON format","action":{"send_files":[]},"target":false,"operator":false}`,
				},
			},
		}

		// Вставляем напоминание в начало истории (после первых 2 сообщений если есть, иначе в начало)
		if len(history) > 2 {
			// Вставляем после первых 2 сообщений (чтобы не нарушить начальный контекст)
			history = append([]GoogleContent{history[0], history[1], jsonReminderMessage, jsonReminderResponse}, history[2:]...)
		} else {
			// Вставляем в самое начало
			history = append([]GoogleContent{jsonReminderMessage, jsonReminderResponse}, history...)
		}

	} else {
		// Если нет tools, можно безопасно использовать response_schema для гарантированного JSON
		if payload["generationConfig"] == nil {
			payload["generationConfig"] = map[string]interface{}{}
		}

		genConfig := payload["generationConfig"].(map[string]interface{})
		genConfig["response_mime_type"] = "application/json"
		genConfig["response_schema"] = create.ParseModelSchemaJSON(false) // false = БЕЗ additionalProperties для Google
	}

	payload["contents"] = history

	// Вызываем стриминг API
	fullText, usageMetadata, functionCalls, err := m.sendToGeminiAPIStreaming(resp.AgentConfig.ModelName, payload, func(delta string) error {
		if onDelta != nil {
			return onDelta(delta, false) // done=false для промежуточных дельт
		}
		return nil
	}, userID)

	if err != nil {
		return fmt.Errorf("ошибка запроса к Gemini API: %w", err)
	}

	// MULTI-TURN CONVERSATION: Если есть function calls БЕЗ текста - выполнить функции и повторить запрос
	if len(functionCalls) > 0 && strings.TrimSpace(fullText) == "" {
		//logger.Debug("Обнаружен вызов функций без текста, начинаем multi-turn conversation", userID)

		// Добавляем model response в историю со ВСЕМИ функциями
		modelResponseParts := make([]map[string]interface{}, len(functionCalls))
		for i, fc := range functionCalls {
			modelResponseParts[i] = map[string]interface{}{"functionCall": fc}
		}

		history = append(history, GoogleContent{
			Role:  "model",
			Parts: modelResponseParts,
		})

		// CALLBACK-АРХИТЕКТУРА: Если указан onToolCall - используем его для выполнения функций
		if onToolCall != nil {
			//logger.Debug("🔧 [RequestStreaming] Обнаружено %d function calls, вызываю onToolCall...", len(functionCalls), userID)

			// Преобразуем functionCalls в формат совместимый с OpenAI (для единообразия)
			var toolCalls []interface{}
			for i, fc := range functionCalls {
				functionName, _ := fc["name"].(string)

				// Сериализуем аргументы в JSON строку (как в OpenAI)
				argsJSON, _ := json.Marshal(fc["args"])

				toolCall := map[string]interface{}{
					"call_id":   fmt.Sprintf("gemini-fc-%d-%d", time.Now().UnixNano(), i),
					"name":      functionName,
					"arguments": string(argsJSON),
				}
				toolCalls = append(toolCalls, toolCall)
			}

			// Вызываем callback для выполнения функций
			toolOutputs, err := onToolCall(toolCalls)
			if err != nil {
				return fmt.Errorf("ошибка выполнения функций: %w", err)
			}
			//logger.Debug("✅ [RequestStreaming] onToolCall вернул %d результатов", len(toolOutputs), userID)

			// Отправляем результаты функций клиенту через streaming
			if onDelta != nil {
				for i, output := range toolOutputs {
					if outputMap, ok := output.(map[string]interface{}); ok {
						callID, _ := outputMap["call_id"].(string)
						content, _ := outputMap["content"].(string)

						// Формируем JSON событие с результатом функции
						functionResult := map[string]interface{}{
							"type":      "function_result",
							"call_id":   callID,
							"name":      toolCalls[i].(map[string]interface{})["name"],
							"content":   content,
							"timestamp": time.Now().Format(time.RFC3339),
						}

						resultJSON, err := json.Marshal(functionResult)
						if err == nil {
							// Отправляем результат клиенту через streaming
							if streamErr := onDelta(string(resultJSON), false); streamErr != nil {
								//logger.Error("[RequestStreaming] Ошибка при отправке результата функции клиенту: %v", streamErr, userID)
								//} else {
								//	logger.Debug("[RequestStreaming] Результат функции отправлен клиенту: call_id=%s", callID, userID)
							}
						}
					}
				}
			}

			// Добавляем результаты функций в историю для повторного запроса
			for i, output := range toolOutputs {
				if outputMap, ok := output.(map[string]interface{}); ok {
					content, _ := outputMap["content"].(string)

					// Парсим content как JSON если возможно
					var contentJSON interface{}
					if err := json.Unmarshal([]byte(content), &contentJSON); err == nil {
						// Добавляем результат функции в историю (в правильном формате для Google Gemini)
						history = append(history, GoogleContent{
							Role: "user",
							Parts: []map[string]interface{}{
								{
									"functionResponse": map[string]interface{}{
										"name":     functionCalls[i]["name"],
										"response": contentJSON,
									},
								},
							},
						})
					} else {
						// Если не JSON - добавляем как строку
						history = append(history, GoogleContent{
							Role: "user",
							Parts: []map[string]interface{}{
								{
									"functionResponse": map[string]interface{}{
										"name":     functionCalls[i]["name"],
										"response": map[string]interface{}{"result": content},
									},
								},
							},
						})
					}

					//logger.Debug("Функция %s выполнена и добавлена в историю", userID, functionCalls[i]["name"])
				}
			}
		} else {
			// СТАРАЯ СИНХРОННАЯ АРХИТЕКТУРА: Если callback не указан - используем handleFunctionCall напрямую
			//logger.Debug("onToolCall не указан, используем синхронную обработку функций", userID)

			for _, fc := range functionCalls {
				result, err := m.handleFunctionCall(fc, resp.Assist.Provider, resp.Assist.UserID)
				if err != nil {
					//logger.Warn("Ошибка обработки function call: %v", userID, err)
					continue
				}

				// Добавляем результат функции в историю (в правильном формате для Google Gemini)
				history = append(history, GoogleContent{
					Role: "user",
					Parts: []map[string]interface{}{
						{
							"functionResponse": map[string]interface{}{
								"name":     fc["name"],
								"response": result,
							},
						},
					},
				})

				//logger.Debug("Функция %s выполнена и добавлена в историю", userID, fc["name"])
			}
		}

		// Обновляем payload с результатами функций
		payload["contents"] = history

		// Повторяем запрос к Gemini (модель должна вернуть текст с результатами)
		//logger.Debug("Отправляем повторный запрос к Gemini с результатами функций", userID)
		fullText, usageMetadata, _, err = m.sendToGeminiAPIStreaming(resp.AgentConfig.ModelName, payload, func(delta string) error {
			if onDelta != nil {
				return onDelta(delta, false)
			}
			return nil
		}, userID)

		if err != nil {
			return fmt.Errorf("ошибка повторного запроса к Gemini API: %w", err)
		}

		//logger.Debug("Получен финальный ответ после выполнения функций: len=%d", userID, len(fullText))
	}

	// Очищаем fullText от markdown-обёрток (Google иногда добавляет ```json ... ```)
	cleanedText := fullText
	cleanedText = strings.TrimSpace(cleanedText)

	// Удаляем ```json в начале и ``` в конце
	if strings.HasPrefix(cleanedText, "```json") {
		cleanedText = strings.TrimPrefix(cleanedText, "```json")
		cleanedText = strings.TrimSpace(cleanedText)
	} else if strings.HasPrefix(cleanedText, "```") {
		cleanedText = strings.TrimPrefix(cleanedText, "```")
		cleanedText = strings.TrimSpace(cleanedText)
	}

	if strings.HasSuffix(cleanedText, "```") {
		cleanedText = strings.TrimSuffix(cleanedText, "```")
		cleanedText = strings.TrimSpace(cleanedText)
	}

	// Парсим финальный ответ из накопленного текста
	var assistResponse model.AssistResponse

	// Google Gemini может возвращать как JSON (с системным промптом), так и обычный текст
	if len(cleanedText) > 0 && cleanedText[0] == '{' {
		// Пытаемся распарсить как JSON
		if err := json.Unmarshal([]byte(cleanedText), &assistResponse); err != nil {
			//logger.Warn("Не удалось распарсить JSON ответ (длина=%d, ошибка=%v), используем как текст",
			//	len(cleanedText), err, userID)
			// JSON невалидный - используем как обычный текст
			assistResponse = model.AssistResponse{
				Message: cleanedText,
				Action: model.Action{
					SendFiles: []model.File{},
				},
				Meta:     false,
				Operator: false,
			}
		}
	} else {
		// Это обычный текст, не JSON
		assistResponse = model.AssistResponse{
			Message: cleanedText,
			Action: model.Action{
				SendFiles: []model.File{},
			},
			Meta:     false,
			Operator: false,
		}
	}

	if assistResponse.Message == "" && cleanedText != "" {
		assistResponse.Message = cleanedText
	}

	// Обработка автоматической генерации видео и изображений (если включены)
	if userID > 0 && text != "" {
		assistResponse, err = m.processVideoGeneration(userID, text, assistResponse, resp.AgentConfig, resp.Assist.Provider)
		if err != nil {
			//logger.Warn("Ошибка обработки генерации видео: %v", err)
		}

		assistResponse, err = m.processImageGeneration(userID, text, assistResponse, resp.AgentConfig, resp.Assist.Provider)
		if err != nil {
			//logger.Warn("Ошибка обработки генерации изображения: %v", err)
		}
	}

	// Сохраняем ответ модели в кэш
	modelMessage := m.createModelMessage(assistResponse)
	m.addMessageToCache(dialogID, modelMessage)

	// Сериализуем финальный ответ обратно в JSON для отправки клиенту
	responseJSON, err := json.Marshal(assistResponse)
	if err != nil {
		return fmt.Errorf("ошибка сериализации ответа: %w", err)
	}

	// ВАЖНО: Сначала отправляем информацию о токенах с done=false (если есть)
	if usageMetadata != nil && onDelta != nil {
		// Извлекаем данные о токенах из Google формата
		promptTokenCount := 0
		candidatesTokenCount := 0
		totalTokenCount := 0
		cachedContentTokenCount := 0
		thoughtsTokenCount := 0

		if val, ok := usageMetadata["promptTokenCount"].(float64); ok {
			promptTokenCount = int(val)
		}
		if val, ok := usageMetadata["candidatesTokenCount"].(float64); ok {
			candidatesTokenCount = int(val)
		}
		if val, ok := usageMetadata["totalTokenCount"].(float64); ok {
			totalTokenCount = int(val)
		}
		if val, ok := usageMetadata["cachedContentTokenCount"].(float64); ok {
			cachedContentTokenCount = int(val)
		}
		if val, ok := usageMetadata["thoughtsTokenCount"].(float64); ok {
			thoughtsTokenCount = int(val)
		}

		// Логируем использование токенов
		//if cachedContentTokenCount > 0 {
		//	logger.Info("[TOKEN USAGE] Prompt: %d | Cached: %d (💰 экономия!) | Output: %d | Total: %d",
		//		promptTokenCount, cachedContentTokenCount, candidatesTokenCount, totalTokenCount, userID)
		//} else {
		//	logger.Info("[TOKEN USAGE] Prompt: %d | Output: %d | Total: %d",
		//		promptTokenCount, candidatesTokenCount, totalTokenCount, userID)
		//}

		// Преобразуем в OpenAI-совместимый формат для клиента
		// Клиент ожидает: input_tokens, output_tokens, total_tokens
		openAIUsage := map[string]interface{}{
			"input_tokens":  promptTokenCount,
			"output_tokens": candidatesTokenCount,
			"total_tokens":  totalTokenCount,
		}

		// Добавляем input_tokens_details если есть кэшированный контент
		if cachedContentTokenCount > 0 {
			openAIUsage["input_tokens_details"] = map[string]interface{}{
				"cached_tokens": cachedContentTokenCount,
			}
		}

		// Добавляем output_tokens_details если есть reasoning tokens (thoughtsTokenCount)
		if thoughtsTokenCount > 0 {
			openAIUsage["output_tokens_details"] = map[string]interface{}{
				"reasoning_tokens": thoughtsTokenCount,
			}
		}

		// Формируем событие в OpenAI-совместимом формате
		tokenUsage := map[string]interface{}{
			"type":  "token_usage",
			"usage": openAIUsage,
		}

		if usageJSON, err := json.Marshal(tokenUsage); err == nil {
			if streamErr := onDelta(string(usageJSON), false); streamErr != nil {
				//logger.Warn("[RequestStreaming] Ошибка при отправке token_usage: %v", streamErr, userID)
			}
		}
	}

	// Отправляем финальный ответ с done=true (это самая важная отправка!)
	if onDelta != nil {
		if err := onDelta(string(responseJSON), true); err != nil {
			return err
		}
	}

	return nil
}
