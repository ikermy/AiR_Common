package mistral

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// Request выполняет запрос к Mistral модели, используя историю диалога как контекст
func (m *MistralModel) Request(userId uint32, modelId string, dialogId uint64, text string, files ...model.FileUpload) (model.AssistResponse, error) {
	var emptyResponse model.AssistResponse

	if text != "" && len(files) > 0 {
		return emptyResponse, fmt.Errorf("пустое сообщение и нет файлов")
	}

	// Ищем RespModel по dialogId в Chan
	var respModel *RespModel
	m.responders.Range(func(key, value interface{}) bool {
		rm := value.(*RespModel)

		if rm.Chan != nil && rm.Chan.DialogId == dialogId {
			respModel = rm
			return false // Прекращаем поиск
		}
		return true // Продолжаем поиск
	})

	if respModel == nil {
		return emptyResponse, fmt.Errorf("RespModel не найден для dialogId %d", dialogId)
	}

	// Получаем контекст диалога из памяти
	if respModel.Context == nil {
		return emptyResponse, fmt.Errorf("контекст диалога не найден для dialogId %d", dialogId)
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

	// Вызываем Mistral API через Conversations API (поддерживает встроенные tools)
	// ВАЖНО: Conversations API НЕ поддерживает file_ids в inputs!
	// - Аудио файлы: уже транскрибированы через TranscribeAudio (текст в userContent)
	// - Документы: должны быть добавлены в document_library агента заранее
	// - Изображения: генерируются через image_generation tool (встроенный)

	// Игнорируем переданные файлы - они не могут быть использованы
	if len(files) > 0 {
		logger.Debug("Получено %d файлов, но они игнорируются (Conversations API не поддерживает file_ids)", len(files), userId)
	}

	// Используем Conversations API для всех запросов
	var convResp ConversationResponse
	var err error

	if respModel.ConversationId == "" {
		// Первый запрос - создаём новый conversation
		// ВАЖНО: Mistral Conversations API НЕ поддерживает file_ids в inputs!
		// Для аудио файлов используется транскрибированный текст (TranscribeAudio уже выполнен)
		// Для документов файлы должны быть в document_library агента
		inputs := []map[string]interface{}{
			{
				"role":    "user",
				"content": text,
				"object":  "entry",
				"type":    "message.input",
			},
		}

		//logger.Debug("Создание нового conversation для агента %s", modelId, userId)
		convResp, err = m.client.StartConversation(modelId, inputs)
		if err != nil {
			return emptyResponse, fmt.Errorf("ошибка создания conversation: %w", err)
		}

		// Сохраняем conversation_id в RespModel
		respModel.ConversationId = convResp.ConversationID
		//logger.Debug("Conversation создан, ID=%s", respModel.ConversationId, userId)

		// Сохраняем conversation_id в БД сразу
		m.saveConversationId(respModel.Chan.DialogId, respModel.ConversationId)
	} else {
		// Продолжаем существующий conversation
		// Отправляем только текст (Conversations API не поддерживает file_ids)
		convResp, err = m.client.ContinueConversation(respModel.ConversationId, text)
		if err != nil {
			// Проверяем на ошибку 404 - агент не найден (был пересоздан с новыми параметрами)
			if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "was not found") {
				logger.Warn("Агент для conversation %s не найден (404), возможно агент был пересоздан. Создаём новый conversation", respModel.ConversationId, userId, userId)

				// Сбрасываем старый conversation_id
				respModel.ConversationId = ""

				// Создаём новый conversation с текущим сообщением
				inputs := []map[string]interface{}{
					{
						"role":    "user",
						"content": text,
						"object":  "entry",
						"type":    "message.input",
					},
				}

				convResp, err = m.client.StartConversation(modelId, inputs)
				if err != nil {
					return emptyResponse, fmt.Errorf("ошибка создания нового conversation после 404: %w", err)
				}

				respModel.ConversationId = convResp.ConversationID
				logger.Debug("Создан новый conversation после 404, ID=%s", respModel.ConversationId, userId)

				// Сохраняем новый conversation_id в БД
				m.saveConversationId(respModel.Chan.DialogId, respModel.ConversationId)
			} else if strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "Failed to create conversation response") {
				// Проверяем на ошибку 503 - conversation в сломанном состоянии
				logger.Warn("Conversation %s в сломанном состоянии (503), создаём новый", respModel.ConversationId, userId)

				// Сбрасываем старый conversation_id
				respModel.ConversationId = ""

				// Создаём новый conversation с текущим сообщением
				inputs := []map[string]interface{}{
					{
						"role":    "user",
						"content": text,
						"object":  "entry",
						"type":    "message.input",
					},
				}

				convResp, err = m.client.StartConversation(modelId, inputs)
				if err != nil {
					return emptyResponse, fmt.Errorf("ошибка создания нового conversation после 503: %w", err)
				}

				respModel.ConversationId = convResp.ConversationID
				logger.Debug("Создан новый conversation после 503, ID=%s", respModel.ConversationId, userId)

				// Сохраняем новый conversation_id в БД
				m.saveConversationId(respModel.Chan.DialogId, respModel.ConversationId)
			} else if strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "Internal Server Error") {
				logger.Warn("API вернул 500, сбрасываем conversation_id и создаём новый для dialogId %d", respModel.Chan.DialogId, userId)

				// Сбрасываем conversation_id
				respModel.ConversationId = ""
				m.saveConversationId(respModel.Chan.DialogId, "")

				// Создаём новый conversation
				inputs := []map[string]interface{}{
					{
						"type":    "user",
						"content": text,
					},
				}

				convResp, err = m.client.StartConversation(modelId, inputs)
				if err != nil {
					return emptyResponse, fmt.Errorf("ошибка создания нового conversation после 500: %w", err)
				}

				// Сохраняем новый conversation_id
				respModel.ConversationId = convResp.ConversationID
				m.saveConversationId(respModel.Chan.DialogId, respModel.ConversationId)
				logger.Info("Создан новый conversation после 500: %s", respModel.ConversationId, userId)
			} else {
				return emptyResponse, fmt.Errorf("ошибка продолжения conversation: %w", err)
			}
		}
	}

	// Обновляем conversation_id если API вернул новый
	if convResp.ConversationID != "" && convResp.ConversationID != respModel.ConversationId {
		respModel.ConversationId = convResp.ConversationID
		//logger.Debug("Conversation ID обновлён: %s", respModel.ConversationId, userId)
		// Сохраняем обновлённый conversation_id в БД
		m.saveConversationId(respModel.Chan.DialogId, respModel.ConversationId)
	}

	// Преобразуем ConversationResponse в Response
	response := ParseConversationResponse(convResp)

	// Обрабатываем ответ
	assistResponse := m.processResponse(response, respModel.RealUserId)

	// Если была вызвана функция, выполняем её и получаем финальный ответ от агента
	if response.HasFunc && m.actionHandler != nil && response.FuncName != "" {
		//logger.Debug("Обнаружен вызов функции: %s", response.FuncName, userId)

		funcResult := m.actionHandler.RunAction(m.ctx, response.FuncName, response.FuncArgs)

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
		//logger.Debug("Отправляем результат функции %s агенту", response.FuncName, userId)

		var finalResponse Response
		if respModel.ConversationId != "" {
			// Используем Conversations API с правильным форматом для function result
			// Отправляем результат с type: "function.result" и tool_call_id согласно документации Mistral
			convResp, err := m.client.SendFunctionResult(respModel.ConversationId, response.ToolCallID, funcResult)
			if err != nil {
				// Проверяем на ошибку 503 - conversation в сломанном состоянии
				if strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "Failed to create conversation response") {
					logger.Warn("Conversation %s сломан (503) после функции, сбрасываем", respModel.ConversationId, userId)

					// Сбрасываем conversation_id - при следующем запросе создастся новый
					respModel.ConversationId = ""
					m.saveConversationId(respModel.Chan.DialogId, "")
				}

				logger.Warn("Ошибка получения ответа после функции: %v", err, userId)
			}

			// Обновляем conversation_id (может измениться)
			if convResp.ConversationID != respModel.ConversationId {
				respModel.ConversationId = convResp.ConversationID
				//logger.Debug("Conversation ID обновлён после функции: %s", respModel.ConversationId, userId)
				// Сохраняем обновлённый conversation_id в БД
				m.saveConversationId(respModel.Chan.DialogId, respModel.ConversationId)
			}
			finalResponse = ParseConversationResponse(convResp)
		} else {
			// conversation_id был сброшен (после ошибки 503), создаём НОВЫЙ conversation
			logger.Warn("conversation_id пустой после ошибки, создаём новый conversation для отправки результата функции", userId)

			// Создаём новый conversation с результатом функции
			inputs := []map[string]interface{}{
				{
					"role":    "user",
					"content": fmt.Sprintf("Результат выполнения функции %s: %s", response.FuncName, funcResult),
					"object":  "entry",
					"type":    "message.input",
				},
			}

			newConvResp, err := m.client.StartConversation(modelId, inputs)
			if err != nil {
				logger.Error("Ошибка создания нового conversation после функции: %v", err, userId)
				// Оставляем текущий assistResponse
				finalResponse = Response{}
			} else {
				respModel.ConversationId = newConvResp.ConversationID
				//logger.Debug("Создан новый conversation после функции, ID=%s", respModel.ConversationId, userId)
				m.saveConversationId(respModel.Chan.DialogId, respModel.ConversationId)

				finalResponse = ParseConversationResponse(newConvResp)
			}
		}

		// Обновляем response и assistResponse ТОЛЬКО если получен финальный ответ
		if finalResponse.Message != "" || finalResponse.HasFunc {
			//logger.Debug("RAW ответ агента: Message='%s', HasFunc=%v, FuncName='%s'", finalResponse.Message, finalResponse.HasFunc, finalResponse.FuncName, userId)

			response = finalResponse
			assistResponse = m.processResponse(finalResponse, respModel.RealUserId)
		}
	} // Конец обработки функций

	// Добавляем ответ ассистента в контекст только если он не пустой
	if assistResponse.Message != "" {
		assistantMessage := Message{
			Type:      "assistant",
			Content:   assistResponse.Message,
			Timestamp: time.Now(),
		}

		respModel.Context.Messages = append(respModel.Context.Messages, assistantMessage)
		respModel.Context.LastUsed = time.Now()
	} else {
		logger.Warn("Получен пустой ответ от ассистента, не добавляем в контекст", userId)
	}

	return assistResponse, nil
}

// processResponse обрабатывает ответ от Mistral
func (m *MistralModel) processResponse(response Response, realUserId uint64) model.AssistResponse {
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

		// Проверяем наличие realUserId
		if realUserId == 0 {
			logger.Warn("processResponse: realUserId не установлен, пропускаем сохранение изображений")
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
				logger.Error("processResponse: ошибка скачивания изображения %s: %v", img.FileID, err)
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
			args := fmt.Sprintf(`{"user_id":"%d","image_data":"%s","file_name":"%s"}`,
				realUserId, base64Encode(imageData), fileName)

			result := m.actionHandler.RunAction(m.ctx, "save_image_data", args)

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
				logger.Warn("processResponse: ошибка удаления временного файла %s: %v", img.FileID, err)
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
		} else {
			logger.Warn("processResponse: ошибка парсинга JSON: %v", err)
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

// loadLibraryIdFromDB загружает ID библиотеки из БД ОДИН РАЗ при создании RespModel
// Избегает повторных запросов к БД при каждом сообщении
func (m *MistralModel) loadLibraryIdFromDB(userId uint32) (string, error) {
	// Получаем все модели пользователя
	userModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		return "", fmt.Errorf("не удалось получить модели пользователя: %w", err)
	}

	// Ищем модель Mistral
	for i := range userModels {
		if userModels[i].Provider == create.ProviderMistral {
			// Десериализуем AllIds для получения VectorId
			if len(userModels[i].AllIds) > 0 {
				var vecIds create.VecIds
				if err := json.Unmarshal(userModels[i].AllIds, &vecIds); err != nil {
					return "", fmt.Errorf("не удалось распарсить AllIds: %w", err)
				}
				if len(vecIds.VectorId) > 0 {
					return vecIds.VectorId[0], nil
				}
			}
		}
	}

	return "", fmt.Errorf("библиотека не найдена для пользователя %d", userId)
}

// base64Encode кодирует данные в base64
func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
