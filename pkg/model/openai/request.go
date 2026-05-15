package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// applyRAG выполняет все тяжёлые подготовительные операции параллельно в фоновой горутине:
//   - Загрузка истории диалога из кэша или БД
//   - Поиск респондента в sync.Map
//   - Генерация эмбеддинга запроса (если RAG включён)
//   - Семантический поиск похожих документов в MariaDB
//
// Результат отправляется в канал ch. Канал закрывается в конце горутины.
// При критической ошибке (не найден respModel) — поле err заполнено.
// При некритических ошибках (эмбеддинг/поиск) — продолжаем без RAG.
func (m *Model) applyRAG(userId uint32, dialogID uint64, text string, ch chan<- openaiRagResp) {
	defer close(ch)

	//totalStart := time.Now()
	result := openaiRagResp{}

	// === 1. Получаем real_user_id ===
	var realUserID uint64
	if m.universalModel != nil {
		var err error
		realUserID, err = m.universalModel.GetRealUserID(userId)
		if err != nil {
			//logger.Warn("applyRAG: не удалось получить real_user_id: %v, используем userId", err, userId)
			realUserID = uint64(userId)
		}
	} else {
		realUserID = uint64(userId)
	}
	result.realUserID = realUserID

	// === 2 + 3. Параллельно: загрузка истории и поиск respModel ===
	// Используем два канала и WaitGroup для параллельного выполнения обеих операций.
	type historyResult struct {
		history []ChatMessage
		dur     time.Duration
	}
	type responderResult struct {
		resp *RespModel
		dur  time.Duration
	}

	historyCh := make(chan historyResult, 1)
	responderCh := make(chan responderResult, 1)

	// Горутина загрузки истории
	go func() {
		start := time.Now()
		var history []ChatMessage

		if cachedHistory, found := m.getDialogHistoryFromCache(dialogID); found {
			history = cachedHistory
			//logger.Debug("applyRAG: история загружена из кэша (%d сообщений)", len(history), userId)
		} else {
			dbHistory, err := m.ConvertDialogToOpenAIFormat(dialogID)
			if err != nil {
				//logger.Warn("applyRAG: не удалось загрузить историю диалога %d из БД: %v, используем пустую историю", dialogID, err, userId)
				history = []ChatMessage{}
			} else {
				history = dbHistory
				//logger.Debug("applyRAG: история загружена из БД (%d сообщений)", len(history), userId)
			}

			// Ограничиваем историю
			maxMessages := int(create.DialogHistoryLimit)
			if len(history) > maxMessages {
				history = history[len(history)-maxMessages:]
			}

			// Сохраняем в кэш
			cache := m.getOrCreateDialogCache(dialogID)
			cache.Messages = history
		}

		historyCh <- historyResult{history: history, dur: time.Since(start)}
	}()

	// Горутина поиска respModel
	go func() {
		start := time.Now()
		var found *RespModel

		m.responders.Range(func(key, value interface{}) bool {
			rm := value.(*RespModel)
			if rm.Chan != nil && rm.Chan.DialogID == dialogID {
				found = rm
				return false
			}
			return true
		})

		responderCh <- responderResult{resp: found, dur: time.Since(start)}
	}()

	// Собираем результаты (обе горутины уже запущены параллельно)
	hRes := <-historyCh
	rRes := <-responderCh

	result.history = hRes.history
	result.historyLoadDuration = hRes.dur
	result.responderLoadDuration = rRes.dur

	// === 4. Проверяем что respModel найден (критично) ===
	if rRes.resp == nil {
		select {
		case <-m.ctx.Done():
		case ch <- openaiRagResp{err: fmt.Errorf("applyRAG: respModel не найден для dialogID %d", dialogID)}:
		}
		return
	}

	resp := rRes.resp
	if resp.AgentConfig == nil {
		select {
		case <-m.ctx.Done():
		case ch <- openaiRagResp{err: fmt.Errorf("applyRAG: конфигурация агента не загружена для dialogID %d", dialogID)}:
		}
		return
	}
	result.respModel = resp

	// === 5. Проверяем нужен ли RAG ===
	if !resp.AgentConfig.Search || text == "" {
		//logger.Debug("applyRAG: RAG не требуется (Search=%v, text=%q)", resp.AgentConfig.Search, text != "", userId)
		select {
		case <-m.ctx.Done():
		case ch <- result:
		}
		return
	}

	// === 6. Генерируем эмбеддинг запроса ===
	embeddingStart := time.Now()
	queryEmbedding, err := m.GenerateEmbedding(text)
	result.embeddingDuration = time.Since(embeddingStart)

	if err != nil {
		//logger.Warn("applyRAG: ошибка генерации эмбеддинга: %v, продолжаем без RAG", err, userId)
		select {
		case <-m.ctx.Done():
		case ch <- result:
		}
		return
	}

	// === 7. Ищем похожие документы в MariaDB ===
	searchStart := time.Now()
	relevantDocs, err := m.searchSimilarEmbeddings(resp.AgentConfig.ModelId, queryEmbedding, create.SimilarEmbeddingsLimit)
	result.searchDuration = time.Since(searchStart)

	if err != nil {
		//logger.Warn("applyRAG: ошибка поиска похожих эмбеддингов: %v, продолжаем без RAG", err, userId)
		select {
		case <-m.ctx.Done():
		case ch <- result:
		}
		return
	}

	// === 8. Формируем обогащённый контекст ===
	if len(relevantDocs) > 0 {
		var relevantChunks []string
		for _, doc := range relevantDocs {
			relevantChunks = append(relevantChunks, doc.Content)
		}

		contextText := strings.Join(relevantChunks, "\n\n---\n\n")
		result.contextText = fmt.Sprintf("Релевантная информация из базы знаний:\n%s\n\n---\n\nВопрос пользователя: %s",
			contextText, text)

		//totalDuration := time.Since(totalStart)
		//logger.Info("[USER:%d] ⚡ applyRAG завершён за %v | История: %v | Респондент: %v | Эмбеддинг: %v | Поиск: %v | Найдено документов: %d (%d символов)",
		//	userId, totalDuration, result.historyLoadDuration, result.responderLoadDuration,
		//	result.embeddingDuration, result.searchDuration, len(relevantDocs), len(contextText))
		//} else {
		//	logger.Debug("applyRAG: похожие документы не найдены", userId)
	}

	select {
	case <-m.ctx.Done():
	case ch <- result:
	}
}

// Request выполняет синхронный запрос к модели (с буферизацией streaming ответов)
func (m *Model) Request(userId uint32, dialogID uint64, text string, files ...model.FileUpload) (model.AssistResponse, error) {
	return model.StreamingToSync(text, files, func(onDelta func(string, bool) error, files ...model.FileUpload) error {
		return m.RequestStreaming(userId, dialogID, text, onDelta, files...)
	})
}

// RequestStreaming выполняет запрос с потоковой передачей delta-событий
// Использует Responses API (новый подход OpenAI с поддержкой file_search, code_interpreter, web_search)
func (m *Model) RequestStreaming(userId uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...model.FileUpload) error {
	if text == "" && len(files) == 0 {
		return fmt.Errorf("пустое сообщение и нет файлов")
	}

	// ============================================================================
	// ОПТИМИЗАЦИЯ: Запускаем applyRAG как можно раньше для параллельного выполнения
	// всех тяжёлых операций (загрузка истории, поиск respModel, эмбеддинги, поиск в БД)
	// ============================================================================
	ragCh := make(chan openaiRagResp, 1)
	go m.applyRAG(userId, dialogID, text, ragCh)

	// Ждём результат из горутины — содержит history, respModel, realUserID, contextText и метрики
	var ragResult openaiRagResp
	select {
	case <-m.ctx.Done():
		return fmt.Errorf("контекст отменён")
	case ragResult = <-ragCh:
		if ragResult.err != nil {
			return fmt.Errorf("ошибка в applyRAG: %w", ragResult.err)
		}
	case <-time.After(create.ApplayRAGTimeaut):
		return fmt.Errorf("таймаут ожидания результата applyRAG")
	}

	// Извлекаем данные из результата applyRAG
	respModel := ragResult.respModel
	history := ragResult.history

	// Обновляем TTL при каждом запросе
	respModel.TTL = time.Now().Add(m.UserModelTTl)

	// Формируем enhancedText (с RAG-контекстом если он есть)
	enhancedText := text
	if ragResult.contextText != "" {
		enhancedText = ragResult.contextText
		//logger.Debug("[USER:%d] RAG: добавлен контекст (%d символов)", userId, len(ragResult.contextText))
	}

	// Создаем сообщение пользователя с поддержкой файлов
	// ВАЖНО: В историю сохраняем ОРИГИНАЛЬНЫЙ text (без RAG контекста)
	// RAG контекст (enhancedText) используется только в input для текущего запроса
	userMessage := m.createUserMessageWithFiles(text, files, userId)
	history = append(history, userMessage)
	m.addMessageToCache(dialogID, userMessage)

	// Формируем input для Responses API
	// Responses API принимает одно input сообщение, история добавляется в instructions
	var conversationContext strings.Builder

	// Добавляем историю диалога как контекст в instructions
	if len(history) > 1 { // Если есть история кроме текущего сообщения
		conversationContext.WriteString("\n\n## ИСТОРИЯ ДИАЛОГА:\n")
		for i, msg := range history[:len(history)-1] { // Все кроме последнего
			role := "Пользователь"
			if msg.Role == "assistant" {
				role = "Ассистент"
			}
			conversationContext.WriteString(fmt.Sprintf("%d. %s: %s\n", i+1, role, msg.Content))
		}
	}

	// Формируем input - последнее сообщение пользователя
	// Используем enhancedText который может содержать RAG контекст из Vector Store
	input := enhancedText

	// Временно модифицируем SystemPrompt для включения истории
	originalSystemPrompt := respModel.AgentConfig.SystemPrompt
	if conversationContext.Len() > 0 {
		respModel.AgentConfig.SystemPrompt = originalSystemPrompt + conversationContext.String()
	}

	// Wrapper для onDelta - обрабатывает как текстовые дельты, так и JSON события function calls
	wrappedOnDelta := func(delta string) error {
		if onDelta != nil {
			// Проверяем, является ли delta JSON событием (начинается с '{')
			// События function calls приходят как JSON: {"type":"response.output_item.added", ...}
			// Текстовые дельты приходят как обычные строки
			if len(delta) > 0 && delta[0] == '{' {
				// Это JSON событие - парсим для определения типа
				var event map[string]interface{}
				if err := json.Unmarshal([]byte(delta), &event); err == nil {
					eventType, _ := event["type"].(string)

					// Проверяем, является ли это событием function call
					if strings.HasPrefix(eventType, "response.output_item.") ||
						strings.HasPrefix(eventType, "response.function_call_arguments.") {
						// Это событие function call - отправляем как есть (уже JSON)
						return onDelta(delta, false)
					}
				}
			}

			// Обычная текстовая дельта
			return onDelta(delta, false)
		}
		return nil
	}

	// Создаём обработчик вызовов функций
	onToolCall := func(toolCalls []interface{}) ([]interface{}, error) {
		//logger.Debug("🔧 [onToolCall] ВЫЗВАН! Количество tool calls: %d", len(toolCalls), userId)

		var toolOutputs []interface{}

		for _, toolCall := range toolCalls {
			toolCallMap, ok := toolCall.(map[string]interface{})
			if !ok {
				//logger.Warn("[onToolCall] Некорректный формат tool call #%d (ожидается map[string]interface{}): тип=%T, значение=%+v",
				//	i, toolCall, toolCall, userId)
				continue
			}

			callID, hasCallID := toolCallMap["call_id"].(string)
			functionName, hasFunctionName := toolCallMap["name"].(string)
			arguments, hasArguments := toolCallMap["arguments"].(string)

			// Проверяем наличие обязательных полей
			if !hasCallID || !hasFunctionName {
				//logger.Warn("[onToolCall] Tool call #%d пропущен: отсутствуют обязательные поля (call_id=%v, name=%v, map=%+v)",
				//	i, hasCallID, hasFunctionName, toolCallMap, userId)
				continue
			}

			if !hasArguments {
				//logger.Debug("[onToolCall] Tool call #%d: аргументы отсутствуют, используем пустую строку", i, userId)
				arguments = ""
			}

			//logger.Debug("🔧 [onToolCall] Tool call #%d: function=%s, call_id=%s, args_length=%d",
			//	i, functionName, callID, len(arguments), userId)

			// Выполняем функцию через action handler
			var result string
			if m.actionHandler != nil {
				//logger.Debug("[onToolCall] Вызываю action handler для функции '%s'...", functionName, userId)
				result = m.actionHandler.RunAction(m.ctx, functionName, arguments, create.ProviderOpenAI, userId)
				//logger.Debug("✅ [onToolCall] Получен результат от action handler для '%s': %s",
				//	functionName, result, userId)
			} else {
				result = `{"error": "action handler not initialized"}`
				//logger.Error("[RequestStreaming] Action handler не инициализирован", userId)
			}

			// Формируем tool output
			// Для Responses API используем формат с role: "tool"
			toolOutput := map[string]interface{}{
				"call_id": callID,
				"content": result,
			}

			toolOutputs = append(toolOutputs, toolOutput)
		}

		//logger.Debug("🔧 [onToolCall] ЗАВЕРШЁН! Возвращаю %d результатов", len(toolOutputs), userId)
		return toolOutputs, nil
	}

	// Вызываем Responses API с обработчиком функций
	_, fullText, err := m.client.CreateResponse(
		m.ctx,
		input,
		respModel.AgentConfig,
		wrappedOnDelta,
		onToolCall,
		userId,
	)

	// Восстанавливаем оригинальный SystemPrompt
	respModel.AgentConfig.SystemPrompt = originalSystemPrompt

	if err != nil {
		return fmt.Errorf("ошибка запроса к Responses API: %w", err)
	}

	// Логируем полученный текст для отладки
	//logger.Debug("CreateResponse вернул fullText (длина=%d): '%s'", len(fullText), fullText, userId)

	// Responses API с response_format возвращает JSON как текст
	// Парсим JSON чтобы извлечь реальную структуру AssistResponse
	var assistResponse model.AssistResponse
	if err := json.Unmarshal([]byte(fullText), &assistResponse); err != nil {
		// Если парсинг не удался - возможно модель вернула просто текст
		//logger.Warn("Не удалось распарсить JSON ответ (длина=%d, ошибка=%v), fullText='%s'",
		//	len(fullText), err, fullText, userId)
		assistResponse = model.AssistResponse{
			Message: fullText,
			Action: model.Action{
				SendFiles: []model.File{},
			},
			Meta:     false,
			Operator: false,
		}
	}

	// Если Message пустое, но есть fullText - используем fullText
	if assistResponse.Message == "" && fullText != "" {
		assistResponse.Message = fullText
	}

	// Сериализуем обратно в JSON для совместимости с startpoint.go
	responseJSON, err := json.Marshal(assistResponse)
	if err != nil {
		return fmt.Errorf("ошибка сериализации ответа: %w", err)
	}

	// Добавляем ответ ассистента в кэш (сохраняем только текст сообщения)
	assistantMessage := ChatMessage{
		Role:    "assistant",
		Content: assistResponse.Message,
	}
	m.addMessageToCache(dialogID, assistantMessage)

	// Вызываем callback с done=true и полным JSON
	if onDelta != nil {
		if err := onDelta(string(responseJSON), true); err != nil {
			//logger.Warn("Ошибка в onDelta callback: %v", err, userId)
		}
	}

	return nil
}

// createUserMessageWithFiles создает сообщение пользователя с поддержкой файлов
// Для Chat Completions API: изображения через image_url, документы через file_search в tools
func (m *Model) createUserMessageWithFiles(text string, files []model.FileUpload, _ uint32) ChatMessage {
	// Если нет файлов - простое текстовое сообщение
	if len(files) == 0 {
		return ChatMessage{
			Role:    "user",
			Content: text,
		}
	}

	// Если есть файлы - формируем multi-part content
	var contentParts []interface{}

	// Добавляем текстовую часть
	if text != "" {
		contentParts = append(contentParts, map[string]interface{}{
			"type": "text",
			"text": text,
		})
	}

	// Добавляем файлы
	for _, file := range files {
		// Изображения с URL
		if file.HasURL() && file.IsImageMimeType() {
			contentParts = append(contentParts, map[string]interface{}{
				"type": "image_url",
				"image_url": map[string]interface{}{
					"url": file.URL,
				},
			})
			//logger.Debug("Добавлено изображение по URL: %s", file.URL, userId)
		} else if file.Content != nil {
			// Для code_interpreter - загружаем файл
			// TODO: Загрузка файлов для code_interpreter
			//logger.Warn("Файл %s требует загрузки для code_interpreter (не реализовано)", file.Name, userId)
		}
	}

	return ChatMessage{
		Role:    "user",
		Content: contentParts,
	}
}

// ============================================================================
// DIALOG CONVERSION METHODS
// ============================================================================

// ConvertDialogToOpenAIFormat конвертирует историю диалога из БД в формат OpenAI Chat
// Используется при обработке запросов для загрузки истории диалога
// По образцу Google провайдера с поддержкой всех форматов данных
func (m *Model) ConvertDialogToOpenAIFormat(dialogID uint64) ([]ChatMessage, error) {
	// Читаем сырые данные из БД с лимитом истории
	rawData, err := m.db.ReadDialog(dialogID, create.DialogHistoryLimit)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения диалога: %w", err)
	}

	if len(rawData) == 0 {
		return nil, nil // Пустая история
	}

	// Используем базовый парсер для консолидации логики парсинга
	parsedMessages, err := model.ParseDialogHistory(rawData)
	if err != nil {
		return nil, fmt.Errorf("ошибка парсинга истории: %w", err)
	}

	var messages []ChatMessage

	// Конвертируем парсенные сообщения в формат OpenAI
	for _, msg := range parsedMessages {
		var role string
		// creator: 1 = AI, 2 = User
		if creator, ok := msg.Creator.(float64); ok {
			if creator == 1 {
				role = "assistant"
			} else {
				role = "user"
			}
		} else if creator, ok := msg.Creator.(string); ok {
			role = creator
		} else {
			role = "user"
		}

		// Извлекаем текст сообщения
		var content string
		if msgMap, ok := msg.Message.(map[string]interface{}); ok {
			if msgText, ok := msgMap["message"].(string); ok {
				content = msgText
			} else {
				// Сериализуем весь объект как JSON если нет поля message
				jsonBytes, _ := json.Marshal(msgMap)
				content = string(jsonBytes)
			}
		} else if msgStr, ok := msg.Message.(string); ok {
			content = msgStr
		}

		if content != "" {
			messages = append(messages, ChatMessage{
				Role:    role,
				Content: content,
			})
		}
	}

	return messages, nil
}
