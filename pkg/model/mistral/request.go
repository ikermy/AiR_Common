package mistral

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// createConversationInputs создаёт структуру inputs для Mistral Conversations API StartConversation
// Консолидирует повторяющийся код инициализации inputs для разных случаев ошибок
func createConversationInputs(content interface{}) []map[string]interface{} {
	return []map[string]interface{}{
		{
			"role":    "user",
			"content": content,
			"object":  "entry",
			"type":    "message.input",
		},
	}
}

// prepareUserContent подготавливает userContent для отправки в Mistral API
// Возвращает либо простой текст, либо структурированный контент с изображениями
// Консолидирует дублирующееся преобразование text+files в userContent
func prepareUserContent(text string, files []model.FileUpload) interface{} {
	// Проверяем наличие изображений с URL
	var hasImageURLs bool
	for _, file := range files {
		if file.HasURL() && file.IsImageMimeType() {
			hasImageURLs = true
			break
		}
	}

	// Если есть изображения - формируем content с parts, иначе только текст
	if hasImageURLs {
		// Формируем content как массив parts (text + image_url)
		contentParts := []map[string]interface{}{
			{"type": "text", "text": text},
		}
		for _, file := range files {
			if file.HasURL() && file.IsImageMimeType() {
				contentParts = append(contentParts, map[string]interface{}{
					"type":      "image_url",
					"image_url": file.URL,
				})
				//logger.Debug("Добавлено изображение по URL: %s", file.URL, userID)
			}
		}
		return contentParts
	}
	// Простой текстовый контент
	return text
}

// Request выполняет запрос к Mistral модели, используя историю диалога как контекст
func (m *Model) Request(_ uint32, dialogID uint64, text string, files ...model.FileUpload) (model.AssistResponse, error) {
	var emptyResponse model.AssistResponse

	if text == "" && len(files) == 0 {
		return emptyResponse, fmt.Errorf("пустое сообщение и нет файлов")
	}

	// Ищем RespModel по dialogID в Chan
	var respModel *RespModel
	m.responders.Range(func(key, value interface{}) bool {
		rm := value.(*RespModel)

		if rm.Chan != nil && rm.Chan.DialogID == dialogID {
			respModel = rm
			return false // Прекращаем поиск
		}
		return true // Продолжаем поиск
	})

	if respModel == nil {
		return emptyResponse, fmt.Errorf("RespModel не найден для dialogID %d", dialogID)
	}

	// Получаем контекст диалога из памяти
	if respModel.Context == nil {
		return emptyResponse, fmt.Errorf("контекст диалога не найден для dialogID %d", dialogID)
	}

	// Обновляем TTL респондера при каждом запросе
	respModel.TTL = time.Now().Add(m.UserModelTTl)

	// Добавляем текущее сообщение в локальный контекст (для сохранения в БД)
	userMessage := Message{
		Type:      "user",
		Content:   text,
		Timestamp: time.Now(),
	}
	respModel.Context.Messages = append(respModel.Context.Messages, userMessage)
	respModel.Context.LastUsed = time.Now()

	// Формируем userContent для отправки в API
	userContent := prepareUserContent(text, files)

	// Используем Conversations API для всех запросов
	var convResp ConversationResponse
	var err error

	if respModel.ConversationId == "" {
		// Первый запрос - создаём новый conversation
		inputs := createConversationInputs(userContent)

		//logger.Debug("Создание нового conversation для агента %s", respModel.Assist.AssistId, userID)
		convResp, err = m.client.StartConversation(respModel.Assist.AssistId, inputs)
		if err != nil {
			return emptyResponse, fmt.Errorf("ошибка создания conversation: %w", err)
		}

		// Сохраняем conversation_id в RespModel
		respModel.ConversationId = convResp.ConversationID
		//logger.Debug("Conversation создан, ID=%s", respModel.ConversationId, userID)

		// Сохраняем conversation_id в БД сразу
		m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)
	} else {
		// Продолжаем существующий conversation
		// Отправляем userContent (может содержать изображения)
		convResp, err = m.client.ContinueConversation(respModel.ConversationId, userContent)
		if err != nil {
			// Проверяем на ошибку 400 с кодом 3230 - рассинхронизация вызовов функций
			if strings.Contains(err.Error(), "400") && strings.Contains(err.Error(), "Not the same number of function calls and responses") {
				//logger.Warn("Conversation %s: рассинхронизация вызовов функций (400/3230) при ContinueConversation, сбрасываем и создаём новый", respModel.ConversationId, userID)

				// Сбрасываем conversation_id
				respModel.ConversationId = ""
				m.saveConversationId(respModel.Chan.DialogID, "")

				// Создаём новый conversation с текущим сообщением пользователя
				inputs := createConversationInputs(userContent)

				convResp, err = m.client.StartConversation(respModel.Assist.AssistId, inputs)
				if err != nil {
					return emptyResponse, fmt.Errorf("ошибка создания нового conversation после 400/3230: %w", err)
				}

				respModel.ConversationId = convResp.ConversationID
				m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)
				//logger.Debug("Создан новый conversation после 400/3230 при ContinueConversation: %s", respModel.ConversationId, userID)
			} else if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "was not found") {
				// Проверяем на ошибку 404 - агент не найден (был пересоздан с новыми параметрами)
				//logger.Warn("Агент для conversation %s не найден (404), возможно агент был пересоздан. Создаём новый conversation", respModel.ConversationId, userID, userID)

				// Сбрасываем старый conversation_id
				respModel.ConversationId = ""

				// Создаём новый conversation с текущим сообщением
				inputs := createConversationInputs(userContent)

				convResp, err = m.client.StartConversation(respModel.Assist.AssistId, inputs)
				if err != nil {
					return emptyResponse, fmt.Errorf("ошибка создания нового conversation после 404: %w", err)
				}

				respModel.ConversationId = convResp.ConversationID
				//logger.Debug("Создан новый conversation после 404, ID=%s", respModel.ConversationId, userID)

				// Сохраняем новый conversation_id в БД
				m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)
			} else if strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "Failed to create conversation response") {
				// Проверяем на ошибку 503 - conversation в сломанном состоянии
				//logger.Warn("Conversation %s в сломанном состоянии (503), создаём новый", respModel.ConversationId, userID)

				// Сбрасываем старый conversation_id
				respModel.ConversationId = ""

				// Создаём новый conversation с текущим сообщением
				inputs := createConversationInputs(userContent)

				convResp, err = m.client.StartConversation(respModel.Assist.AssistId, inputs)
				if err != nil {
					return emptyResponse, fmt.Errorf("ошибка создания нового conversation после 503: %w", err)
				}

				respModel.ConversationId = convResp.ConversationID
				//logger.Debug("Создан новый conversation после 503, ID=%s", respModel.ConversationId, userID)

				// Сохраняем новый conversation_id в БД
				m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)
			} else if strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "Internal Server Error") {
				//logger.Warn("API вернул 500, сбрасываем conversation_id и создаём новый для dialogID %d", respModel.Chan.DialogID, userID)

				// Сбрасываем conversation_id
				respModel.ConversationId = ""
				m.saveConversationId(respModel.Chan.DialogID, "")

				// Создаём новый conversation
				inputs := []map[string]interface{}{
					{
						"type":    "user",
						"content": userContent,
					},
				}

				convResp, err = m.client.StartConversation(respModel.Assist.AssistId, inputs)
				if err != nil {
					return emptyResponse, fmt.Errorf("ошибка создания нового conversation после 500: %w", err)
				}

				// Сохраняем новый conversation_id
				respModel.ConversationId = convResp.ConversationID
				m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)
				//logger.Debug("Создан новый conversation после 500: %s", respModel.ConversationId, userID)
			} else {
				return emptyResponse, fmt.Errorf("ошибка продолжения conversation: %w", err)
			}
		}
	}

	// Обновляем conversation_id если API вернул новый
	if convResp.ConversationID != "" && convResp.ConversationID != respModel.ConversationId {
		respModel.ConversationId = convResp.ConversationID
		//logger.Debug("Conversation ID обновлён: %s", respModel.ConversationId, userID)
		// Сохраняем обновлённый conversation_id в БД
		m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)
	}

	// Преобразуем ConversationResponse в Response
	response := ParseConversationResponse(convResp)

	// Логируем сырой ответ для диагностики
	//logger.Debug("Mistral RAW ответ: Message='%s', HasFunc=%v, FuncName='%s'", response.Message, response.HasFunc, response.FuncName, userID)

	// Обрабатываем ответ
	assistResponse := m.processResponse(response, respModel.RealuserID, respModel.Assist.Provider)

	// Обрабатываем цепочку вызовов функций (если есть)
	// ВАЖНО: Mistral может вызывать несколько функций подряд (например: get_current_time -> sheets_write_range)
	functionCallCount := 0

	for response.HasFunc && m.actionHandler != nil && response.FuncName != "" && functionCallCount < create.MaxFunctionCalls {
		functionCallCount++
		//logger.Debug("Mistral вызвал функцию #%d: %s с аргументами: %s", functionCallCount, response.FuncName, response.FuncArgs, userID)

		funcResult := m.actionHandler.RunAction(m.ctx, response.FuncName, response.FuncArgs, respModel.Assist.Provider, respModel.Assist.UserID)
		//logger.Debug("Результат функции #%d %s: %s", functionCallCount, response.FuncName, funcResult, userID)

		// Сохраняем результат функции в контекст для истории
		toolResultMessage := Message{
			Type:      "user",
			Content:   fmt.Sprintf("[Результат функции %s]: %s", response.FuncName, funcResult),
			Timestamp: time.Now(),
		}
		respModel.Context.Messages = append(respModel.Context.Messages, toolResultMessage)
		respModel.Context.LastUsed = time.Now()

		// Отправляем результат функции обратно в conversation
		// Используем чистый результат без дополнительного форматирования
		//logger.Debug("Отправляем результат функции %s агенту", response.FuncName, userID)

		var finalResponse Response
		if respModel.ConversationId != "" {
			// Используем Conversations API с правильным форматом для function result
			// Отправляем результат с type: "function.result" и tool_call_id согласно документации Mistral
			convResp, err := m.client.SendFunctionResult(respModel.ConversationId, response.ToolCallID, funcResult)
			if err != nil {
				// Проверяем на ошибку 400 - невалидный tool_call_id или рассинхронизация вызовов функций
				if strings.Contains(err.Error(), "400") && (strings.Contains(err.Error(), "Unexpected tool call id") || strings.Contains(err.Error(), "Not the same number of function calls and responses")) {
					//logger.Warn("Conversation %s: проблема с tool_call_id или рассинхронизация (400/3230), сбрасываем и создаём новый", respModel.ConversationId, userID)

					// Сбрасываем conversation_id
					respModel.ConversationId = ""
					m.saveConversationId(respModel.Chan.DialogID, "")

					// Создаём новый conversation с контекстом последнего сообщения пользователя
					inputs := createConversationInputs(fmt.Sprintf("Результат выполнения функции %s: %s", response.FuncName, funcResult))

					newConvResp, newErr := m.client.StartConversation(respModel.Assist.AssistId, inputs)
					if newErr != nil {
						return emptyResponse, fmt.Errorf("ошибка восстановления после рассинхронизации функций: %w", newErr)
					}

					respModel.ConversationId = newConvResp.ConversationID
					m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)
					//logger.Debug("Создан новый conversation после 400/3230: %s", respModel.ConversationId, userID)

					finalResponse = ParseConversationResponse(newConvResp)
				} else if strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "Failed to create conversation response") {
					// Проверяем на ошибку 503 - conversation в сломанном состоянии
					//logger.Warn("Conversation %s сломан (503) после функции, сбрасываем", respModel.ConversationId, userID)

					// Сбрасываем conversation_id - при следующем запросе создастся новый
					respModel.ConversationId = ""
					m.saveConversationId(respModel.Chan.DialogID, "")

					// Возвращаем ошибку для повторной попытки
					return emptyResponse, fmt.Errorf("conversation сломан (503): %w", err)
				}

				// Другие ошибки - логируем и возвращаем
				return emptyResponse, fmt.Errorf("ошибка отправки результата функции %s: %w", response.FuncName, err)
			}

			// Обновляем conversation_id (может измениться)
			if convResp.ConversationID != respModel.ConversationId {
				respModel.ConversationId = convResp.ConversationID
				//logger.Debug("Conversation ID обновлён после функции: %s", respModel.ConversationId, userID)
				// Сохраняем обновлённый conversation_id в БД
				m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)
			}
			finalResponse = ParseConversationResponse(convResp)
		} else {
			// conversation_id был сброшен (после ошибки 503), создаём НОВЫЙ conversation
			//logger.Warn("conversation_id пустой после ошибки, создаём новый conversation для отправки результата функции", userID)

			// Создаём новый conversation с результатом функции
			inputs := createConversationInputs(fmt.Sprintf("Результат выполнения функции %s: %s", response.FuncName, funcResult))

			newConvResp, err := m.client.StartConversation(respModel.Assist.AssistId, inputs)
			if err != nil {
				//logger.Error("Ошибка создания нового conversation после функции: %v", err, userID)
				// Оставляем текущий assistResponse
				finalResponse = Response{}
			} else {
				respModel.ConversationId = newConvResp.ConversationID
				//logger.Debug("Создан новый conversation после функции, ID=%s", respModel.ConversationId, userID)
				m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)

				finalResponse = ParseConversationResponse(newConvResp)
			}
		}

		// Обновляем response и assistResponse ТОЛЬКО если получен финальный ответ
		if finalResponse.Message != "" || finalResponse.HasFunc {
			//logger.Debug("RAW ответ агента: Message='%s', HasFunc=%v, FuncName='%s'", finalResponse.Message, finalResponse.HasFunc, finalResponse.FuncName, userID)

			response = finalResponse
			assistResponse = m.processResponse(finalResponse, respModel.RealuserID, respModel.Assist.Provider)

			// Если это НЕ вызов функции, выходим из цикла - получен финальный ответ
			if !finalResponse.HasFunc {
				//logger.Debug("Получен финальный ответ после %d вызовов функций", functionCallCount, userID)
				break
			}
			// Если HasFunc==true, цикл продолжится и выполнит следующую функцию
		} else {
			// Логируем если финальный ответ пустой
			//logger.Warn("Mistral вернул пустой финальный ответ после функции #%d %s. Message='%s', HasFunc=%v, funcResult='%s'. Прерываем цепочку.",
			//	functionCallCount, response.FuncName, finalResponse.Message, finalResponse.HasFunc, funcResult, userID)
			break
		}
	} // Конец цикла обработки функций

	if functionCallCount >= create.MaxFunctionCalls {
		//logger.Warn("Достигнут лимит вызовов функций (%d), прерываем цепочку", create.MaxFunctionCalls, userID)
	}

	// Добавляем ответ ассистента в контекст только если он не пустой
	if assistResponse.Message != "" {
		assistantMessage := Message{
			Type:      "assistant",
			Content:   assistResponse.Message,
			Timestamp: time.Now(),
		}

		respModel.Context.Messages = append(respModel.Context.Messages, assistantMessage)
		respModel.Context.LastUsed = time.Now()
		//} else {
		//	logger.Warn("Получен пустой ответ от ассистента, не добавляем в контекст", userID)
	}

	return assistResponse, nil
}

// processResponse обрабатывает ответ от Mistral
func (m *Model) processResponse(response Response, realuserID uint64, provider create.ProviderType) model.AssistResponse {
	messageText := strings.TrimSpace(response.Message)

	// СНАЧАЛА парсим JSON из ответа (если есть) чтобы получить красивые имена файлов
	var userFileNames []string // Красивые имена из send_files

	// Убираем markdown блок ```json ... ``` если есть
	if strings.HasPrefix(messageText, "```json") || strings.HasPrefix(messageText, "```JSON") {
		messageText = strings.TrimPrefix(messageText, "```json")
		messageText = strings.TrimPrefix(messageText, "```JSON")
		messageText = strings.TrimSpace(messageText)

		if idx := strings.LastIndex(messageText, "```"); idx != -1 {
			messageText = messageText[:idx]
			messageText = strings.TrimSpace(messageText)
		}
		//logger.Debug("processResponse: удалён markdown блок, извлечён чистый JSON")
	}

	// Попытка распарсить ответ как JSON для получения красивых имён файлов
	if messageText != "" && (strings.HasPrefix(messageText, "{") || strings.HasPrefix(messageText, "[")) {
		var structuredResponse struct {
			Action struct {
				SendFiles []struct {
					FileName string `json:"file_name"`
				} `json:"send_files"`
			} `json:"action"`
		}

		if err := json.Unmarshal([]byte(messageText), &structuredResponse); err == nil {
			// Извлекаем имена файлов из send_files
			for _, file := range structuredResponse.Action.SendFiles {
				if file.FileName != "" {
					userFileNames = append(userFileNames, file.FileName)
				}
			}
			//if len(userFileNames) > 0 {
			//	logger.Debug("processResponse: извлечено %d имён файлов из JSON: %v", len(userFileNames), userFileNames)
			//}
		}
	}

	// Обрабатываем сгенерированные изображения (если есть)
	var savedFiles []model.File // Сохранённые файлы для замены URL в send_files

	if len(response.GeneratedImages) > 0 {
		//logger.Debug("processResponse: обнаружено %d сгенерированных изображений", len(response.GeneratedImages))

		// Проверяем наличие realuserID
		if realuserID == 0 {
			//logger.Warn("processResponse: realuserID не установлен, пропускаем сохранение изображений")
			return model.AssistResponse{
				Message:  messageText,
				Meta:     false,
				Operator: false,
			}
		}

		// Скачиваем и сохраняем каждое изображение
		for idx, img := range response.GeneratedImages {
			// Скачиваем изображение через Mistral Files API
			imageData, err := m.client.DownloadFile(img.FileID)
			if err != nil {
				//logger.Error("processResponse: ошибка скачивания изображения %s: %v", img.FileID, err)
				continue
			}

			//logger.Debug("processResponse: скачано изображение %s (%d байт)", img.FileName, len(imageData))

			// Формируем УНИКАЛЬНОЕ имя файла с правильным расширением
			var fileName string

			// Используем часть file_id для уникальности (первые 8 символов)
			uniquePrefix := img.FileID
			if len(uniquePrefix) > 8 {
				uniquePrefix = uniquePrefix[:8]
			}

			// ПРИОРИТЕТ 1: Используем красивое имя из JSON send_files если доступно
			if idx < len(userFileNames) && userFileNames[idx] != "" {
				fileName = userFileNames[idx]
				//logger.Debug("processResponse: используем имя из JSON send_files: %s", fileName)
			} else if img.FileName != "" && !strings.HasPrefix(img.FileName, "image_generated") {
				// ПРИОРИТЕТ 2: Оригинальное имя от Mistral (если не generic)
				baseName := img.FileName
				if idx := strings.LastIndex(baseName, "."); idx != -1 {
					baseName = baseName[:idx]
				}
				fileName = fmt.Sprintf("%s_%s.%s", baseName, uniquePrefix, img.FileType)
			} else {
				// ПРИОРИТЕТ 3: Генерируем уникальное имя
				fileName = fmt.Sprintf("image_%s.%s", uniquePrefix, img.FileType)
			}

			// Вызываем save_image_data через action handler с base64 данными
			args := fmt.Sprintf(`{"image_data":"%s","file_name":"%s"}`,
				base64Encode(imageData), fileName)

			result := m.actionHandler.RunAction(m.ctx, "save_image_data", args, provider, uint32(realuserID))

			var saveResult struct {
				URL   string `json:"url"`
				Error string `json:"error"`
			}

			if err := json.Unmarshal([]byte(result), &saveResult); err == nil && saveResult.URL != "" {
				// Определяем тип файла
				var fileType model.FileType
				if img.FileType == "gif" {
					fileType = model.Doc // GIF отправляем как документ
				} else {
					fileType = model.Photo // PNG/JPG по умолчанию photo
				}

				savedFiles = append(savedFiles, model.File{
					Type:     fileType,
					URL:      saveResult.URL,
					FileName: fileName,
					Caption:  "", // Caption будет взят из JSON send_files при замене URL
				})

				//logger.Debug("processResponse: изображение сохранено, URL=%s", saveResult.URL)
			}
		}

		// Удаляю сгенерированные изображения из Mistral Files API
		for _, img := range response.GeneratedImages {
			if err := m.DeleteTempFile(img.FileID); err != nil {
				//logger.Warn("processResponse: ошибка удаления временного файла %s: %v", img.FileID, err)
			}
		}

		// НЕ возвращаем здесь! Продолжаем обработку JSON чтобы извлечь правильное message
		// и заменить URL в send_files на реальные
	}

	// Попытка распарсить ответ как JSON (агенты Mistral могут возвращать структурированные ответы)
	// messageText уже обработан (убран markdown) в начале функции
	if messageText != "" && (strings.HasPrefix(messageText, "{") || strings.HasPrefix(messageText, "[")) {
		//logger.Debug("processResponse: обнаружен JSON в ответе, пытаемся распарсить")

		// Удаляем реальные переносы строк и табуляции (НЕ экранированные)
		messageText = strings.ReplaceAll(messageText, "\n", "")
		messageText = strings.ReplaceAll(messageText, "\r", "")
		messageText = strings.ReplaceAll(messageText, "\t", "")

		// Заменяем экранированные последовательности на пробелы внутри JSON-строк
		messageText = strings.ReplaceAll(messageText, "\\n", " ")
		messageText = strings.ReplaceAll(messageText, "\\t", " ")

		var structuredResponse struct {
			Message string `json:"message"`
			Action  struct {
				SendFiles []model.File `json:"send_files"`
			} `json:"action"`
			Target   bool `json:"target"`
			Operator bool `json:"operator"`
		}

		if err := json.Unmarshal([]byte(messageText), &structuredResponse); err == nil {
			// Успешно распарсили JSON - извлекаем данные
			//logger.Debug("processResponse: JSON успешно распарсен, message='%s', target=%v, operator=%v, files=%d",
			//	structuredResponse.Message, structuredResponse.Target, structuredResponse.Operator, len(structuredResponse.Action.SendFiles))

			// ВАЖНО: Используем ТОЛЬКО извлечённое message, а не весь JSON!
			assistResponse := model.AssistResponse{
				Message:  structuredResponse.Message, // Только текст сообщения, БЕЗ JSON!
				Meta:     structuredResponse.Target,
				Operator: structuredResponse.Operator,
			}

			// Обрабатываем action.send_files если есть
			if len(structuredResponse.Action.SendFiles) > 0 {
				// ВАЖНО: Если есть сгенерированные изображения (response.GeneratedImages),
				// они уже были скачаны и сохранены выше.
				// Нужно заменить URL из send_files (временные Azure URL) на реальные сохранённые URL.

				// Создаём мапу сохранённых файлов для быстрого поиска по имени
				savedFilesByName := make(map[string]model.File)
				if len(savedFiles) > 0 {
					for _, file := range savedFiles {
						// Используем и оригинальное имя И имя без расширения для поиска
						savedFilesByName[file.FileName] = file

						// Также добавляем базовое имя (без расширения) для лучшего сопоставления
						if idx := strings.LastIndex(file.FileName, "."); idx != -1 {
							baseName := file.FileName[:idx]
							savedFilesByName[baseName] = file
						}
					}
				}

				// Проходим по файлам из JSON и заменяем URL если файл был сохранён
				for i := range structuredResponse.Action.SendFiles {
					fileFromJSON := &structuredResponse.Action.SendFiles[i]

					// Получаем базовое имя из JSON (без расширения для поиска)
					searchName := fileFromJSON.FileName
					if idx := strings.LastIndex(searchName, "."); idx != -1 {
						searchName = searchName[:idx]
					}

					// Ищем сохранённый файл по имени или базовому имени
					if savedFile, found := savedFilesByName[fileFromJSON.FileName]; found {
						// Файл найден по полному имени
						fileFromJSON.URL = savedFile.URL
						// Используем имя из JSON, но URL реальный
					} else if savedFile, found := savedFilesByName[searchName]; found {
						// Файл найден по базовому имени
						fileFromJSON.URL = savedFile.URL
					} else if fileFromJSON.Type == model.Photo && len(savedFiles) > 0 {
						// Это photo и есть сохранённые файлы - используем первый сохранённый
						fileFromJSON.URL = savedFiles[0].URL
						// Оставляем file_name из JSON для красивого отображения
					}
				}

				assistResponse.Action.SendFiles = structuredResponse.Action.SendFiles
				//logger.Debug("processResponse: добавлено %d файлов в Action", len(structuredResponse.Action.SendFiles))
			} else if len(savedFiles) > 0 {
				// В JSON нет send_files, но есть сохранённые изображения - используем их
				assistResponse.Action.SendFiles = savedFiles
				//logger.Debug("processResponse: использованы сохранённые файлы (%d шт)", len(savedFiles))
			}

			return assistResponse
			//} else {
			//	logger.Warn("processResponse: ошибка парсинга JSON: %v", err)
		}
		// Если парсинг не удался, продолжаем обработку как обычный текст
	}

	// Обычный текстовый ответ (не JSON)
	assistResponse := model.AssistResponse{
		Message:  response.Message,
		Meta:     false,
		Operator: false,
	}

	// Если есть вызов функции, но нет сообщения, используем пустую строку
	if response.HasFunc && response.Message == "" {
		assistResponse.Message = ""
	}

	return assistResponse
}

// base64Encode кодирует данные в base64
func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// RequestStreaming выполняет запрос с потоковой передачей (TRUE STREAMING)
// Использует Mistral Conversations API в streaming режиме с Server-Sent Events (SSE)
// Поддерживает вызов функций и подсчет токенов
func (m *Model) RequestStreaming(_ uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...model.FileUpload) error {
	if text == "" && len(files) == 0 {
		return fmt.Errorf("пустое сообщение и нет файлов")
	}

	// Ищем RespModel по dialogID в Chan
	var respModel *RespModel
	m.responders.Range(func(key, value interface{}) bool {
		rm := value.(*RespModel)
		if rm.Chan != nil && rm.Chan.DialogID == dialogID {
			respModel = rm
			return false
		}
		return true
	})

	if respModel == nil {
		return fmt.Errorf("RespModel не найден для dialogID %d", dialogID)
	}

	if respModel.Context == nil {
		return fmt.Errorf("контекст диалога не найден для dialogID %d", dialogID)
	}

	// Обновляем TTL респондера при каждом запросе
	respModel.TTL = time.Now().Add(m.UserModelTTl)

	// Синхронизируем инструменты агента один раз за сессию.
	// PATCHит агент актуальными MCP-tools и сбрасывает ConversationId если конфигурация изменилась.
	if !respModel.ToolsSynced {
		m.syncAgentTools(respModel)
	}

	// Добавляем текущее сообщение в локальный контекст
	userMessage := Message{
		Type:      "user",
		Content:   text,
		Timestamp: time.Now(),
	}
	respModel.Context.Messages = append(respModel.Context.Messages, userMessage)
	respModel.Context.LastUsed = time.Now()

	// Формируем userContent для отправки в API
	userContent := prepareUserContent(text, files)

	// Wrapper для onDelta - обрабатывает как текстовые дельты, так и JSON события function calls
	wrappedOnDelta := func(delta string) error {
		if onDelta == nil {
			return nil
		}

		// Проверяем, является ли delta JSON событием (начинается с '{')
		if len(delta) > 0 && delta[0] == '{' {
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(delta), &event); err == nil {
				if eventType, ok := event["type"].(string); ok && eventType == "function_call" {
					// Это событие вызова функции - отправляем как есть (уже JSON)
					return onDelta(delta, false)
				}
			}
		}

		// Обычная текстовая дельта
		return onDelta(delta, false)
	}

	// Используем Conversations API в streaming режиме
	var convResp ConversationResponse
	var err error

	if respModel.ConversationId == "" {
		// Первый запрос - создаём новый conversation
		inputs := createConversationInputs(userContent)

		convResp, err = m.client.StartConversationStreaming(respModel.Assist.AssistId, inputs, wrappedOnDelta)
		if err != nil {
			return fmt.Errorf("ошибка создания streaming conversation: %w", err)
		}

		// Сохраняем conversation_id
		respModel.ConversationId = convResp.ConversationID
		m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)
	} else {
		// Продолжаем существующий conversation
		convResp, err = m.client.ContinueConversationStreaming(respModel.ConversationId, userContent, wrappedOnDelta)
		if err != nil {
			// Обработка ошибок - сброс и пересоздание conversation
			if strings.Contains(err.Error(), "400") || strings.Contains(err.Error(), "404") ||
				strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "500") {
				//logger.Warn("Conversation %s: ошибка %v, сбрасываем и создаём новый", respModel.ConversationId, err, userID)

				respModel.ConversationId = ""
				m.saveConversationId(respModel.Chan.DialogID, "")

				// Создаём новый conversation
				inputs := createConversationInputs(userContent)

				convResp, err = m.client.StartConversationStreaming(respModel.Assist.AssistId, inputs, wrappedOnDelta)
				if err != nil {
					return fmt.Errorf("ошибка создания нового streaming conversation: %w", err)
				}

				respModel.ConversationId = convResp.ConversationID
				m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)
			} else {
				return fmt.Errorf("ошибка продолжения streaming conversation: %w", err)
			}
		}
	}

	// Обновляем conversation_id если API вернул новый
	if convResp.ConversationID != "" && convResp.ConversationID != respModel.ConversationId {
		respModel.ConversationId = convResp.ConversationID
		m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)
	}

	// Накапливаем суммарное использование токенов по всем HTTP-стримам
	totalUsage := convResp.Usage

	// sendTotalUsage отправляет итоговое событие token_usage один раз
	sendTotalUsage := func() {
		if totalUsage == nil || onDelta == nil {
			return
		}
		tokenUsage := map[string]interface{}{
			"type": "token_usage",
			"usage": map[string]interface{}{
				"prompt_tokens":     totalUsage.PromptTokens,
				"completion_tokens": totalUsage.CompletionTokens,
				"total_tokens":      totalUsage.TotalTokens,
			},
		}
		if usageJSON, err := json.Marshal(tokenUsage); err == nil {
			if streamErr := onDelta(string(usageJSON), false); streamErr != nil {
				//logger.Warn("[RequestStreaming] Ошибка при отправке token_usage: %v", streamErr)
			}
		}
	}

	// Проверяем есть ли вызовы функций в outputs
	functionCalls := extractFunctionCalls(convResp.Outputs)

	// Если нет function calls в первом ответе - это обычный текстовый ответ
	if len(functionCalls) == 0 {
		response := ParseConversationResponse(convResp)
		assistResponse := m.processResponse(response, respModel.RealuserID, respModel.Assist.Provider)

		// Сохраняем ответ в контекст
		if assistResponse.Message != "" {
			assistantMessage := Message{
				Type:      "assistant",
				Content:   assistResponse.Message,
				Timestamp: time.Now(),
			}
			respModel.Context.Messages = append(respModel.Context.Messages, assistantMessage)
			respModel.Context.LastUsed = time.Now()
		}

		// Отправляем финальный ответ
		responseJSON, err := json.Marshal(assistResponse)
		if err != nil {
			return fmt.Errorf("ошибка сериализации ответа: %w", err)
		}

		sendTotalUsage()

		if onDelta != nil {
			if err := onDelta(string(responseJSON), true); err != nil {
				//logger.Warn("Ошибка в onDelta callback: %v", err, userID)
			}
		}

		return nil
	}

	// Обрабатываем цепочку вызовов функций (если есть)
	functionCallRound := 0
	maxRounds := 5 // Максимум 5 раундов параллельных вызовов

	for len(functionCalls) > 0 && functionCallRound < maxRounds && m.actionHandler != nil {
		functionCallRound++
		//logger.Debug("Раунд вызовов функций #%d: обнаружено %d функций", functionCallRound, len(functionCalls), userID)

		// Выполняем ВСЕ функции параллельно (или последовательно)
		functionResults := make([]map[string]interface{}, 0, len(functionCalls))

		for _, funcCall := range functionCalls {
			//logger.Debug("Вызов функции #%d в раунде %d: %s с аргументами: %s",
			//	i+1, functionCallRound, funcCall.Name, funcCall.Arguments, userID)

			funcResult := m.actionHandler.RunAction(m.ctx, funcCall.Name, funcCall.Arguments, respModel.Assist.Provider, respModel.Assist.UserID)
			//logger.Debug("Результат функции %s: %s", funcCall.Name, funcResult, userID)

			// Сохраняем результат функции
			functionResults = append(functionResults, map[string]interface{}{
				"tool_call_id": funcCall.ToolCallID,
				"result":       funcResult,
				"object":       "entry",
				"type":         "function.result",
			})

			// Сохраняем результат в контекст
			toolResultMessage := Message{
				Type:      "user",
				Content:   fmt.Sprintf("[Результат функции %s]: %s", funcCall.Name, funcResult),
				Timestamp: time.Now(),
			}
			respModel.Context.Messages = append(respModel.Context.Messages, toolResultMessage)
		}

		// Отправляем ВСЕ результаты функций одним запросом
		if respModel.ConversationId != "" && len(functionResults) > 0 {
			finalConvResp, err := m.client.SendMultipleFunctionResultsStreaming(
				respModel.ConversationId,
				functionResults,
				wrappedOnDelta,
			)
			if err != nil {
				// Проверяем на ошибки рассинхронизации или сломанного conversation
				if strings.Contains(err.Error(), "400") || strings.Contains(err.Error(), "503") ||
					strings.Contains(err.Error(), "conversation error") || strings.Contains(err.Error(), "3000") {
					//logger.Warn("Conversation %s: проблема при SendMultipleFunctionResultsStreaming (%v), сбрасываем",
					//	respModel.ConversationId, err, userID)
					respModel.ConversationId = ""
					m.saveConversationId(respModel.Chan.DialogID, "")
					break
				} else {
					//logger.Error("Ошибка SendMultipleFunctionResultsStreaming: %v", err, userID)
					return fmt.Errorf("ошибка отправки результатов функций: %w", err)
				}
			}

			// Накапливаем использование токенов из этого раунда
			if finalConvResp.Usage != nil {
				if totalUsage == nil {
					totalUsage = &TokenUsage{}
				}
				totalUsage.PromptTokens += finalConvResp.Usage.PromptTokens
				totalUsage.CompletionTokens += finalConvResp.Usage.CompletionTokens
				totalUsage.TotalTokens += finalConvResp.Usage.TotalTokens
			}

			// Обновляем conversation_id
			if finalConvResp.ConversationID != respModel.ConversationId {
				respModel.ConversationId = finalConvResp.ConversationID
				m.saveConversationId(respModel.Chan.DialogID, respModel.ConversationId)
			}

			// Проверяем есть ли еще вызовы функций
			functionCalls = extractFunctionCalls(finalConvResp.Outputs)

			// Парсим ответ для проверки наличия контента
			response := ParseConversationResponse(finalConvResp)
			assistResponse := m.processResponse(response, respModel.RealuserID, respModel.Assist.Provider)

			// Если нет больше function calls И есть текстовый ответ - это финальный ответ
			if len(functionCalls) == 0 {
				//logger.Debug("Раунд #%d завершен: получен финальный ответ (len=%d)",
				//	functionCallRound, len(assistResponse.Message), userID)

				// Сохраняем ответ в контекст если есть
				if assistResponse.Message != "" {
					assistantMessage := Message{
						Type:      "assistant",
						Content:   assistResponse.Message,
						Timestamp: time.Now(),
					}
					respModel.Context.Messages = append(respModel.Context.Messages, assistantMessage)
					respModel.Context.LastUsed = time.Now()
				}

				// Отправляем финальный ответ (даже если пустой)
				responseJSON, err := json.Marshal(assistResponse)
				if err != nil {
					return fmt.Errorf("ошибка сериализации ответа: %w", err)
				}

				sendTotalUsage()

				if onDelta != nil {
					if err := onDelta(string(responseJSON), true); err != nil {
						//logger.Warn("Ошибка в onDelta callback: %v", err, userID)
					}
				}

				return nil
			}

			// Есть новые function calls - продолжаем цикл
			//logger.Debug("Раунд #%d: обнаружено %d новых function calls, продолжаем",
			//	functionCallRound, len(functionCalls), userID)
		} else {
			// conversation_id был сброшен - выходим
			break
		}
	}

	if functionCallRound >= maxRounds {
		//logger.Warn("Достигнут лимит раундов вызовов функций (%d), прерываем цепочку", maxRounds, userID)
	}

	// Если вышли из цикла без финального ответа - отправляем пустой ответ
	//logger.Warn("Получен пустой ответ от ассистента, не добавляем в контекст", userID)

	emptyResponse := m.processResponse(Response{}, respModel.RealuserID, respModel.Assist.Provider)
	responseJSON, _ := json.Marshal(emptyResponse)

	sendTotalUsage()

	if onDelta != nil {
		if err := onDelta(string(responseJSON), true); err != nil {
			//logger.Warn("Ошибка в onDelta callback (fallback): %v", err, userID)
		}
	}

	return nil
}

// syncAgentTools синхронизирует набор инструментов Mistral Agent с текущим MCP-сервером.
// Вызывается один раз за сессию (ToolsSynced == false) перед первым запросом.
// Если инструменты изменились — вызывает PATCH /v1/agents/{id} и сбрасывает ConversationId,
// чтобы следующий разговор начался с обновлённой конфигурацией агента.
func (m *Model) syncAgentTools(respModel *RespModel) {
	defer func() { respModel.ToolsSynced = true }()

	if m.actionHandler == nil || m.client == nil || respModel.Assist.AssistId == "" {
		return
	}

	mcpProvider, ok := m.actionHandler.(model.MCPConfigProvider)
	if !ok {
		return
	}

	var tools []map[string]interface{}

	// MCP function tools — основной источник runtime-инструментов
	if mcpTools, err := mcpProvider.FetchToolsList(m.ctx, respModel.Assist.UserID, create.ProviderMistral); err == nil {
		for _, t := range mcpTools {
			tools = append(tools, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.InputSchema,
				},
			})
		}
	}

	// Нативные built-in инструменты агента (code_interpreter, image_generation, web_search, document_library)
	if m.universalModel != nil {
		if compressedData, _, err := m.db.ReadUserModelByProvider(respModel.Assist.UserID, create.ProviderMistral); err == nil && compressedData != nil {
			if modelData, err := m.universalModel.DecompressModelData(compressedData, nil); err == nil {
				if modelData.Interpreter {
					tools = append(tools, map[string]interface{}{"type": "code_interpreter"})
				}
				if modelData.Image {
					tools = append(tools, map[string]interface{}{"type": "image_generation"})
				}
				if modelData.WebSearch {
					tools = append(tools, map[string]interface{}{"type": "web_search"})
				}
				if modelData.Search || len(modelData.VecIds.VectorId) > 0 {
					documentLibraryTool := map[string]interface{}{
						"type": "document_library",
					}
					if len(modelData.VecIds.VectorId) > 0 {
						documentLibraryTool["library_ids"] = modelData.VecIds.VectorId
					}
					tools = append(tools, documentLibraryTool)
				}
			}
		}
	}

	if len(tools) == 0 {
		return
	}

	// Обновляем агент на стороне Mistral — теперь он знает об актуальных инструментах
	if err := m.client.PatchAgent(respModel.Assist.AssistId, tools); err != nil {
		// Не критично: логируем и продолжаем работу со старой конфигурацией
		return
	}

	// Сбрасываем ConversationId: старая беседа была создана с прежней конфигурацией агента
	// (без tools). Новая беседа автоматически подхватит обновлённые инструменты.
	if respModel.ConversationId != "" {
		respModel.ConversationId = ""
		m.saveConversationId(respModel.Chan.DialogID, "")
	}
}

// extractFunctionCalls извлекает все вызовы функций из outputs
func extractFunctionCalls(outputs []ConversationOutput) []struct{ Name, Arguments, ToolCallID string } {
	var calls []struct{ Name, Arguments, ToolCallID string }
	for _, output := range outputs {
		if output.Type == "function.call" && output.Name != "" {
			calls = append(calls, struct{ Name, Arguments, ToolCallID string }{
				Name:       output.Name,
				Arguments:  output.Arguments,
				ToolCallID: output.ToolCallID,
			})
		}
	}
	return calls
}
