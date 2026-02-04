package google

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
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
func (m *GoogleModel) Request(userId uint32, modelId string, dialogId uint64, text string, files ...model.FileUpload) (model.AssistResponse, error) {
	var emptyResponse model.AssistResponse

	if text == "" && len(files) == 0 {
		return emptyResponse, fmt.Errorf("пустое сообщение и нет файлов")
	}

	// Получаем или создаём кэш диалога
	// Если кэш не найден - загружаем историю из БД и создаём кэш
	var history []GoogleContent

	if cachedHistory, found := m.getDialogHistoryFromCache(dialogId); found {
		// Используем историю из кэша
		history = cachedHistory
		//logger.Debug("Использована история из кэша для диалога %d", dialogId)
	} else {
		// Кэш не найден - загружаем из БД (первичная загрузка)
		//logger.Debug("Кэш не найден, загружаю историю из БД для диалога %d", dialogId)

		// Получаем или создаём респондента (загружает конфигурацию)
		resp, err := m.GetOrCreateResponder(dialogId, userId)
		if err != nil {
			return emptyResponse, fmt.Errorf("ошибка получения респондента: %w", err)
		}

		if resp.AgentConfig == nil {
			return emptyResponse, fmt.Errorf("конфигурация агента не загружена")
		}

		// Загружаем историю из БД
		dbHistory, err := m.ConvertDialogToGoogleFormat(dialogId)
		if err != nil {
			logger.Warn("Не удалось загрузить историю диалога %d из БД: %v, начинаем с пустой истории", dialogId, err)
			history = []GoogleContent{}
		} else {
			history = dbHistory
			//logger.Debug("Загружено %d сообщений из БД для диалога %d", len(history), dialogId)
		}

		// Применяем ограничение на количество сообщений
		maxMessages := int(create.GoogleDialogHistoryLimit)
		if len(history) > maxMessages {
			// Оставляем только последние maxMessages сообщений
			history = history[len(history)-maxMessages:]
			//logger.Debug("Ограничено количество сообщений в истории диалога %d до %d (было %d)",
			//	dialogId, maxMessages, len(history))
		}

		// Сохраняем в кэш (getOrCreateDialogCache обновит ExpireAt)
		cache := m.getOrCreateDialogCache(dialogId)
		cache.Contents = history
	}

	// Обновляем ExpireAt для текущего диалога (продлится на GoogleDialogLiveTimeout)
	m.getOrCreateDialogCache(dialogId)

	// Проверяем конфигурацию агента (нужна для RAG и отправки запроса)
	resp, err := m.GetOrCreateResponder(dialogId, userId)
	if err != nil {
		return emptyResponse, fmt.Errorf("ошибка получения конфигурации: %w", err)
	}

	// RAG: Semantic Search в MariaDB Vector Store
	// Если есть VectorIds - используем SearchSimilarDocuments для обогащения контекста
	enhancedText := text
	if resp.AgentConfig.HasVector && len(resp.AgentConfig.VectorIds) > 0 && text != "" {
		//logger.Debug("RAG активирован: найдено %d векторных хранилищ для modelId=%d",
		//	len(resp.AgentConfig.VectorIds), resp.AgentConfig.ModelId, userId)

		// Выполняем semantic search через MariaDB Vector Store
		// 1. Генерируем эмбеддинг запроса через Google Embedding API
		queryEmbedding, err := m.GenerateEmbedding(text)
		if err != nil {
			logger.Warn("Ошибка генерации эмбеддинга для RAG: %v, продолжаем без RAG", err, userId)
		} else {
			// 2. Используем MariaDB VEC_Distance_Cosine для поиска похожих документов
			relevantDocs, err := m.searchSimilarEmbeddings(resp.AgentConfig.ModelId, queryEmbedding, 3)
			if err != nil {
				logger.Warn("SearchSimilarEmbeddings failed для modelId=%d: %v, продолжаем без RAG",
					resp.AgentConfig.ModelId, err, userId)
			} else if len(relevantDocs) > 0 {
				// Извлекаем контент из найденных документов
				var relevantChunks []string
				for _, doc := range relevantDocs {
					relevantChunks = append(relevantChunks, doc.Content)
				}

				// Обогащаем запрос найденным контекстом
				contextText := strings.Join(relevantChunks, "\n\n---\n\n")
				enhancedText = fmt.Sprintf(`Релевантная информация из базы знаний:
%s

---

Вопрос пользователя: %s`, contextText, text)

				logger.Info("RAG: добавлено %d документов из Vector Store (итого %d символов контекста)",
					len(relevantDocs), len(contextText), userId)
			}
		}
	}

	// Добавляем новое сообщение пользователя (с обогащённым текстом если был RAG)
	userMessage := m.createUserMessage(enhancedText, files)
	history = append(history, userMessage)

	// Сохраняем в кэш
	m.addMessageToCache(dialogId, userMessage)

	// ВАЖНО: Формируем payload ПОСЛЕ всех модификаций history!
	// Сначала добавляем конфигурацию агента
	payload := map[string]interface{}{}

	if resp.AgentConfig.SystemInstruction != nil {
		payload["system_instruction"] = resp.AgentConfig.SystemInstruction
	}

	if resp.AgentConfig.GenerationConfig != nil {
		payload["generationConfig"] = resp.AgentConfig.GenerationConfig
	}

	// Проверяем наличие tools перед добавлением response_schema
	// ВАЖНО: response_schema и google_search несовместимы!
	hasTools := len(resp.AgentConfig.Tools) > 0

	if hasTools {
		payload["tools"] = resp.AgentConfig.Tools

		// КРИТИЧЕСКИ ВАЖНО: Удаляем response_schema и response_mime_type из generationConfig
		// если используются tools (особенно google_search), иначе поиск не работает!
		if genConfig, ok := payload["generationConfig"].(map[string]interface{}); ok {
			delete(genConfig, "response_schema")
			delete(genConfig, "response_mime_type")
			//logger.Debug("[Googlecreate.Request] Удалены response_schema и response_mime_type из-за наличия tools")
		}

		// ВАЖНО: Добавляем напоминание о JSON формате в начало истории диалога
		// Поскольку response_schema удален, модель может забыть про JSON
		// Вставляем системное сообщение с напоминанием в начало истории
		jsonReminderText := "ВАЖНО: Все твои ответы ДОЛЖНЫ быть строго в JSON формате согласно схеме:\n" + create.GoogleSchemaJSON + "\n\nНикогда не отвечай обычным текстом!"

		// Проверяем наличие google_search и добавляем инструкцию
		hasGoogleSearch := false
		if resp.AgentConfig.WebSearch {
			hasGoogleSearch = true
			jsonReminderText += "\n\nУ ТЕБЯ ЕСТЬ ДОСТУП К GOOGLE SEARCH!\n" +
				"- Когда пользователь спрашивает о ТЕКУЩИХ событиях, погоде, новостях - ОБЯЗАТЕЛЬНО используй google_search!\n" +
				"- НЕ ОТКАЗЫВАЙ говоря 'у меня нет доступа к интернету' - это НЕПРАВДА, у тебя есть google_search!\n" +
				"- Просто вызови функцию google_search с запросом и получишь результаты из интернета."
		}

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
					"text": fmt.Sprintf(`{"message":"Понял, все мои ответы будут строго в JSON формате%s","action":{"send_files":[]},"target":false,"operator":false}`,
						func() string {
							if hasGoogleSearch {
								return " и я буду активно использовать google_search для актуальной информации"
							}
							return ""
						}()),
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
		genConfig["response_schema"] = create.ParseGoogleSchemaJSON()
	}

	// Устанавливаем contents ПОСЛЕ всех модификаций history
	payload["contents"] = history

	// 4. Отправляем запрос
	response, err := m.sendToGeminiAPI(resp.AgentConfig.ModelName, payload)
	if err != nil {
		return emptyResponse, fmt.Errorf("ошибка запроса к Gemini API: %w", err)
	}

	// 5. Парсим ответ (с обработкой function calls)
	assistResponse, err := m.parseGeminiResponseWithFunctionHandling(response, history, payload, resp.AgentConfig.ModelName)
	if err != nil {
		return emptyResponse, fmt.Errorf("ошибка парсинга ответа: %w", err)
	}

	// 6. Обрабатываем автоматическую генерацию видео (если включена и есть запрос)
	if userId > 0 && text != "" {
		assistResponse, err = m.processVideoGeneration(userId, text, assistResponse, resp.AgentConfig)
		if err != nil {
			logger.Warn("Ошибка обработки генерации видео: %v", err)
		}
	}

	// 6.1. Обрабатываем автоматическую генерацию изображений (если включена и есть запрос)
	if userId > 0 && text != "" {
		assistResponse, err = m.processImageGeneration(userId, text, assistResponse, resp.AgentConfig)
		if err != nil {
			logger.Warn("Ошибка обработки генерации изображения: %v", err)
		}
	}

	// 7. Сохраняем ответ модели в кэш диалога
	modelMessage := m.createModelMessage(assistResponse)
	m.addMessageToCache(dialogId, modelMessage)

	// 8. История сохраняется автоматически через Endpoint.SaveDialog
	// (вызывается из startpoint)
	//logger.Debug("assistResponse %+v", assistResponse)
	return assistResponse, nil
}

// ConvertDialogToGoogleFormat конвертирует историю из БД в формат Google Gemini
func (m *GoogleModel) ConvertDialogToGoogleFormat(dialogId uint64) ([]GoogleContent, error) {
	// Читаем историю из БД
	dialogData, err := m.db.ReadDialog(dialogId, create.GoogleDialogHistoryLimit)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения диалога: %w", err)
	}

	if len(dialogData) == 0 {
		return []GoogleContent{}, nil // Пустая история
	}

	var messages []DialogMessage

	type DialogDataWrapperArray struct {
		Data []string `json:"Data"` // Массив JSON строк
	}

	type DialogDataWrapperString struct {
		Data string `json:"Data"` // Строка JSON (с двойной экранизацией)
	}

	// Сначала пытаемся распарсить как структуру с полем Data (массив строк)
	var wrapperArray DialogDataWrapperArray
	if err := json.Unmarshal(dialogData, &wrapperArray); err == nil && len(wrapperArray.Data) > 0 {
		// Успешно распарсили как структуру с полем Data (массив строк)
		for i, jsonStr := range wrapperArray.Data {
			var msg DialogMessage
			if err := json.Unmarshal([]byte(jsonStr), &msg); err != nil {
				logger.Warn("Ошибка парсинга сообщения %d: %v (jsonStr: %.100s)", i, err, jsonStr)
				continue
			}
			messages = append(messages, msg)
		}
	} else {
		// Пытаемся распарсить как структуру с полем Data (строка JSON)
		var wrapperString DialogDataWrapperString
		if err := json.Unmarshal(dialogData, &wrapperString); err == nil && len(wrapperString.Data) > 0 {
			// Распарсиваем строку как массив строк JSON
			var stringArray []string
			if err := json.Unmarshal([]byte(wrapperString.Data), &stringArray); err == nil && len(stringArray) > 0 {
				for i, jsonStr := range stringArray {
					var msg DialogMessage
					if err := json.Unmarshal([]byte(jsonStr), &msg); err != nil {
						logger.Warn("Ошибка парсинга сообщения %d: %v (jsonStr: %.100s)", i, err, jsonStr)
						continue
					}
					messages = append(messages, msg)
				}
			}
		} else {
			// Пытаемся распарсить как массив строк напрямую (каждая строка - JSON объект)
			var stringArray []string
			err = json.Unmarshal(dialogData, &stringArray)
			if err == nil && len(stringArray) > 0 {
				// Успешно распарсили как массив строк
				for i, jsonStr := range stringArray {
					var msg DialogMessage
					if err := json.Unmarshal([]byte(jsonStr), &msg); err != nil {
						logger.Warn("Ошибка парсинга сообщения %d: %v (jsonStr: %.100s)", i, err, jsonStr)
						continue
					}
					messages = append(messages, msg)
				}
			} else {
				// Пытаемся распарсить как массив объектов
				if err := json.Unmarshal(dialogData, &messages); err != nil {
					// Если ошибка - пытаемся распарсить как один объект
					var singleMessage DialogMessage
					if singleErr := json.Unmarshal(dialogData, &singleMessage); singleErr != nil {
						return nil, fmt.Errorf("ошибка парсинга истории (не структура Data, не массив строк, не массив, не объект): %w", err)
					}
					// Если распарсилось как один объект - оборачиваем в массив
					messages = []DialogMessage{singleMessage}
				}
			}
		}
	}

	var contents []GoogleContent
	for _, msg := range messages {
		// Определяем роль (используем GetCreator для нормализации)
		role := "user"
		creator := msg.GetCreator()
		if creator == "assistant" || creator == "model" {
			role = "model"
		}

		// Извлекаем текст сообщения
		var messageText string
		if msgInterface, ok := msg.Message["message"]; ok {
			if msgStr, ok := msgInterface.(string); ok {
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
func (m *GoogleModel) createUserMessage(text string, files []model.FileUpload) GoogleContent {
	parts := []map[string]interface{}{
		{"text": text},
	}

	// TODO: Добавить поддержку файлов если нужно
	// for _, file := range files {
	//     parts = append(parts, map[string]interface{}{
	//         "inline_data": map[string]string{
	//             "mime_type": file.MimeType,
	//             "data":      base64.StdEncoding.EncodeToString(file.Data),
	//         },
	//     })
	// }

	return GoogleContent{
		Role:  "user",
		Parts: parts,
	}
}

// createModelMessage создаёт сообщение модели в формате Google Gemini
func (m *GoogleModel) createModelMessage(assistResponse model.AssistResponse) GoogleContent {
	// Извлекаем текстовое сообщение
	messageText := assistResponse.Message
	if messageText == "" {
		messageText = "(пустой ответ)"
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
func (m *GoogleModel) sendToGeminiAPI(modelName string, payload map[string]interface{}) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s",
		m.client.GetUrl(), modelName, m.client.GetAPIKey())

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return responseBody, nil
}

// parseGeminiResponse парсит ответ от Google Gemini API
func (m *GoogleModel) parseGeminiResponse(responseBody []byte) (model.AssistResponse, error) {
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

	// Если есть function calls, обрабатываем их
	if len(functionCalls) > 0 {
		//logger.Debug("Получено %d function calls для обработки", len(functionCalls))

		for _, fc := range functionCalls {
			result, err := m.handleFunctionCall(fc)
			if err != nil {
				logger.Warn("Ошибка обработки function call: %v", err)
				continue
			}

			// Если нет текстового ответа, используем результат функции как ответ
			if len(textParts) == 0 {
				// result содержит распарсенный JSON: {"output": "..."}
				if output, ok := result["output"].(string); ok {
					textParts = append(textParts, output)
				} else if result, ok := result["result"].(string); ok {
					// Fallback для случая когда результат не распарсился
					textParts = append(textParts, result)
				}
			}

			// Проверяем это generate_video
			if action, ok := result["action"].(string); ok && action == "generate_video" {
				// Сохраняем параметры для последующей генерации
				logger.Debug("Обнаружен запрос на генерацию видео: %+v", result)
				// TODO: Можно добавить в контекст для обработки после ответа
			}
		}
	}

	// Объединяем текстовые части
	fullText := strings.Join(textParts, "\n")

	if fullText == "" {
		return emptyResponse, fmt.Errorf("получен пустой текст от модели")
	}

	// Пытаемся распарсить как JSON (если модель вернула структурированный ответ)
	var assistResp model.AssistResponse

	// Сначала распарсиваем в raw map для проверки структуры
	var rawResp map[string]interface{}
	jsonText := fullText

	// Пытаемся распарсить JSON напрямую
	err := json.Unmarshal([]byte(jsonText), &rawResp)

	// Если не получилось - пытаемся извлечь из markdown блока
	if err != nil {
		jsonText = extractJSONFromMarkdown(fullText)
		err = json.Unmarshal([]byte(jsonText), &rawResp)
	}

	if err == nil {
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

// parseGeminiResponseWithFunctionHandling парсит ответ и обрабатывает function calls через multi-turn conversation
// Если модель вызывает функцию без текста, отправляем результат обратно модели для продолжения
func (m *GoogleModel) parseGeminiResponseWithFunctionHandling(responseBody []byte, history []GoogleContent,
	payload map[string]interface{}, modelName string) (model.AssistResponse, error) {

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
			result, err := m.handleFunctionCall(fc)
			if err != nil {
				logger.Warn("Ошибка обработки function call: %v", err)
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
		response, err := m.sendToGeminiAPI(modelName, payload)
		if err != nil {
			return emptyResponse, fmt.Errorf("ошибка повторного запроса к Gemini API: %w", err)
		}

		// Рекурсивно парсим ответ (модель должна вернуть текст)
		return m.parseGeminiResponseWithFunctionHandling(response, history, payload, modelName)
	}

	// Если есть function calls И текст - обрабатываем функции (но текст используем как ответ)
	if len(functionCalls) > 0 && len(textParts) > 0 {
		//logger.Debug("Модель вернула текст и вызвала функции")
		for _, fc := range functionCalls {
			result, err := m.handleFunctionCall(fc)
			if err != nil {
				logger.Warn("Ошибка обработки function call: %v", err)
				continue
			}

			// Проверяем это generate_video
			if action, ok := result["action"].(string); ok && action == "generate_video" {
				logger.Debug("Обнаружен запрос на генерацию видео: %+v", result)
			}
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
func (m *GoogleModel) handleFunctionCall(functionCall map[string]interface{}) (map[string]interface{}, error) {
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
		result := m.actionHandler.RunAction(m.ctx, functionName, string(argsJSON))

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

// mergeResponses объединяет несколько ответов в один (если модель вернула несколько частей)
func (m *GoogleModel) mergeResponses(responses []model.AssistResponse) model.AssistResponse {
	if len(responses) == 1 {
		return responses[0]
	}

	var merged model.AssistResponse
	var messages []string
	var allFiles []model.File

	for _, resp := range responses {
		if resp.Message != "" {
			messages = append(messages, resp.Message)
		}
		if len(resp.Action.SendFiles) > 0 {
			allFiles = append(allFiles, resp.Action.SendFiles...)
		}
		// Берём последние значения meta и operator
		merged.Meta = resp.Meta
		merged.Operator = resp.Operator
	}

	if len(messages) > 0 {
		merged.Message = strings.Join(messages, "\n\n")
	}

	if len(allFiles) > 0 {
		// Убираем дубликаты файлов
		uniqueFiles := make(map[string]model.File)
		for _, file := range allFiles {
			uniqueFiles[file.URL] = file
		}

		for _, file := range uniqueFiles {
			merged.Action.SendFiles = append(merged.Action.SendFiles, file)
		}
	}

	return merged
}

// processVideoGeneration автоматически генерирует видео если модель вызвала generate_video
// или если в промпте агента включен флаг Video и обнаружены ключевые слова
func (m *GoogleModel) processVideoGeneration(userId uint32, userText string, response model.AssistResponse, agentConfig *GoogleAgentConfig) (model.AssistResponse, error) {
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

	logger.Info("processVideoGeneration: начинаем генерацию видео", userId)

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

	logger.Info("processVideoGeneration: параметры - prompt='%s', aspect=%s, duration=%d", prompt, aspectRatio, duration)

	// Генерируем видео через клиент
	videoData, mimeType, err := m.client.GenerateVideo(prompt, aspectRatio, duration)
	if err != nil {
		logger.Error("processVideoGeneration: ошибка генерации видео: %v", err)
		response.Message += fmt.Sprintf("\n\n⚠️ К сожалению, не удалось сгенерировать видео: %v", err)
		return response, err
	}

	logger.Info("processVideoGeneration: видео успешно сгенерировано: %d bytes, %s", len(videoData), mimeType)

	// Сохраняем видео через save_image_data (используем тот же механизм)
	// TODO: Можно создать отдельный save_video_data endpoint
	fileName := fmt.Sprintf("video_%d_%d.mp4", userId, time.Now().Unix())

	// Кодируем в base64 для передачи
	videoBase64 := base64.StdEncoding.EncodeToString(videoData)

	args := fmt.Sprintf(`{"user_id":"%d","image_data":"%s","file_name":"%s"}`,
		userId, videoBase64, fileName)

	result := m.actionHandler.RunAction(m.ctx, "save_image_data", args)

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
		logger.Info("processVideoGeneration: видео сохранено: URL=%s", saveResult.URL)

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
		logger.Error("processVideoGeneration: ошибка сохранения видео: %s", saveResult.Error)
		response.Message += "\n\n⚠️ Видео сгенерировано, но не удалось сохранить."
	}

	return response, nil
}

// extractVideoPrompt извлекает промпт для генерации видео из текста
func (m *GoogleModel) extractVideoPrompt(userText, modelResponse string) string {
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
func (m *GoogleModel) isVideoEnabled(config *GoogleAgentConfig) bool {
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
func (m *GoogleModel) processImageGeneration(userId uint32, userText string, response model.AssistResponse, agentConfig *GoogleAgentConfig) (model.AssistResponse, error) {
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

	logger.Info("processImageGeneration: начинаем генерацию изображения", userId)

	// Извлекаем промпт для генерации (из запроса пользователя или ответа модели)
	prompt := m.extractImagePrompt(userText, response.Message)

	// Извлекаем aspect ratio если указан
	aspectRatio := "1:1" // По умолчанию квадрат для изображений
	if strings.Contains(userTextLower, "вертикал") || strings.Contains(userTextLower, "9:16") {
		aspectRatio = "9:16"
	} else if strings.Contains(userTextLower, "горизонтал") || strings.Contains(userTextLower, "16:9") {
		aspectRatio = "16:9"
	}

	logger.Info("processImageGeneration: параметры - prompt='%s', aspect=%s", prompt, aspectRatio)

	// Генерируем изображение через Google Imagen API
	imageData, mimeType, err := m.client.GenerateImage(prompt, aspectRatio)
	if err != nil {
		logger.Error("processImageGeneration: ошибка генерации изображения: %v", err)
		response.Message += fmt.Sprintf("\n\n⚠️ К сожалению, не удалось сгенерировать изображение: %v", err)
		return response, err
	}

	logger.Info("processImageGeneration: изображение успешно сгенерировано: %d bytes, %s", len(imageData), mimeType)

	// Определяем расширение файла из MIME type
	ext := "png"
	if strings.Contains(mimeType, "jpeg") || strings.Contains(mimeType, "jpg") {
		ext = "jpg"
	}

	fileName := fmt.Sprintf("image_%d_%d.%s", userId, time.Now().Unix(), ext)

	// Кодируем в base64 для передачи в save_image_data
	imageBase64 := base64.StdEncoding.EncodeToString(imageData)

	// Сохраняем через action handler
	args := fmt.Sprintf(`{"user_id":"%d","image_data":"%s","file_name":"%s"}`,
		userId, imageBase64, fileName)

	result := m.actionHandler.RunAction(m.ctx, "save_image_data", args)

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
		logger.Info("processImageGeneration: изображение сохранено: URL=%s", saveResult.URL)

		// Удаляем все fake URL из send_files (example.com, placeholder и т.д.)
		cleanedFiles := []model.File{}
		for _, file := range response.Action.SendFiles {
			// Пропускаем fake URL
			if !strings.Contains(file.URL, "example.com") &&
				!strings.Contains(file.URL, "placeholder") &&
				!(strings.HasPrefix(file.URL, "http://") && file.Type == "photo") {
				cleanedFiles = append(cleanedFiles, file)
			} else {
				logger.Info("processImageGeneration: удалён fake URL: %s", file.URL)
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

		logger.Info("processImageGeneration: добавлено реальное изображение в send_files")
	} else {
		logger.Error("processImageGeneration: ошибка сохранения изображения: %s", saveResult.Error)
		response.Message += "\n\n⚠️ Изображение сгенерировано, но не удалось сохранить."
	}

	return response, nil
}

// extractImagePrompt извлекает промпт для генерации изображения из текста
func (m *GoogleModel) extractImagePrompt(userText, modelResponse string) string {
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
