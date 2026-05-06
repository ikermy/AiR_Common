package create

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
)

// MistralSchemaJSON - JSON Schema для структурированных ответов Mistral Agent
const MistralSchemaJSON = `{
	"type": "object",
	"properties": {
		"message": {
			"type": "string",
			"description": "Текстовое сообщение для пользователя"
		},
		"action": {
			"type": "object",
			"properties": {
				"send_files": {
					"type": "array",
					"items": {
						"type": "object",
						"properties": {
							"type": {
								"type": "string",
								"enum": ["photo", "video", "audio", "doc"],
								"description": "Тип файла"
							},
							"Url": {
								"type": "string",
								"description": "URL файла"
							},
							"file_name": {
								"type": "string",
								"description": "Имя файла"
							},
							"caption": {
								"type": "string",
								"description": "Подпись к файлу"
							}
						},
						"required": ["type", "Url", "file_name", "caption"]
					}
				}
			},
			"required": ["send_files"]
		},
		"target": {
			"type": "boolean",
			"description": "Достигнута ли цель диалога"
		},
		"operator": {
			"type": "boolean",
			"description": "Требуется ли подключение оператора"
		}
	},
	"required": ["message", "action", "target", "operator"]
}`

// MistralLibrary представляет библиотеку документов Mistral
type MistralLibrary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// MistralAgentClient клиент для работы с Mistral Agents API
type MistralAgentClient struct {
	apiKey         string
	url            string
	ctx            context.Context
	universalModel *UniversalModel // Ссылка на UniversalModel для доступа к GetRealUserID
}

// deleteMistralModel удаляет Mistral Agent (с поддержкой WS сообщений)
func (m *UniversalModel) deleteMistralModel(userId uint32, modelData *UserModelRecord, deleteFiles bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Удаление Mistral агента...")
	}

	// Удаляем агента через API
	if m.mistralClient != nil {
		if err := m.mistralClient.deleteAgent(modelData.AssistId); err != nil {
			//logger.Error("ошибка удаления Mistral агента %s: %v", modelData.AssistId, err, userId)
			// Продолжаем удаление из БД даже если не удалось удалить из API
			if progressCallback != nil {
				progressCallback(fmt.Sprintf("⚠️ Не удалось удалить агент из Mistral API: %v", err))
			}
		} else {
			if progressCallback != nil {
				progressCallback(fmt.Sprintf("✅ Mistral агент %s удалён из API", modelData.AssistId))
			}
		}

		// Удаляем файлы только если deleteFiles = true
		if deleteFiles && len(modelData.FileIds) > 0 {
			if progressCallback != nil {
				progressCallback(fmt.Sprintf("🔄 Удаление документов из Mistral (%d файлов)...", len(modelData.FileIds)))
			}

			// Получаем library_id из БД
			provider := ProviderMistral
			modelJSON, err := m.ReadModel(userId, &provider)
			if err != nil {
				//logger.Error("Ошибка получения данных модели для удаления файлов: %v", err, userId)
			} else if modelJSON != nil && len(modelJSON.VecIds.VectorId) > 0 {
				libraryID := modelJSON.VecIds.VectorId[0]

				// Удаляем каждый документ из библиотеки
				for i, file := range modelData.FileIds {
					if err := m.mistralClient.DeleteDocumentFromLibrary(libraryID, file.ID); err != nil {
						//logger.Error("Ошибка удаления документа %s из библиотеки: %v", file.ID, err, userId)
					}

					// Отправляем прогресс каждые 5 файлов
					if progressCallback != nil && (i+1)%5 == 0 {
						progressCallback(fmt.Sprintf("🔄 Удалено %d из %d документов...", i+1, len(modelData.FileIds)))
					}
				}

				// После удаления всех документов удаляем саму библиотеку
				if progressCallback != nil {
					progressCallback("🔄 Удаление библиотеки Mistral...")
				}

				if err := m.mistralClient.DeleteLibrary(libraryID); err != nil {
					//logger.Error("Ошибка удаления библиотеки %s: %v", libraryID, err, userId)
					if progressCallback != nil {
						progressCallback(fmt.Sprintf("⚠️ Не удалось удалить библиотеку: %v", err))
					}
				} else {
					if progressCallback != nil {
						progressCallback("✅ Библиотека удалена")
					}
				}
			}
		}
	} else {
		//logger.Warn("Mistral клиент не инициализирован, пропускаем удаление из API", userId)
		if progressCallback != nil {
			progressCallback("⚠️ Mistral клиент не инициализирован, удаляем только из БД")
		}
	}

	if progressCallback != nil {
		progressCallback("✅ Mistral агент и файлы удалены из API")
	}

	//logger.Debug("Mistral модель успешно удалена из API", userId)
	return nil
}

// deleteAgent удаляет Mistral Agent по ID
func (m *MistralAgentClient) deleteAgent(agentID string) error {
	// Убираем /completions из URL
	baseURL := strings.Replace(m.url, "/completions", "", 1)
	deleteURL := fmt.Sprintf("%s/%s", baseURL, agentID)

	return m.executeMistralDeleteRequest(deleteURL)
}

// updateMistralModelInPlace обновляет Mistral Agent
func (m *UniversalModel) updateMistralModelInPlace(userId uint32, existing, updated *UniversalModelData) error {
	if m.mistralClient == nil {
		return fmt.Errorf("Mistral клиент не инициализирован")
	}

	// Для Mistral нужно удалить старого агента и создать нового
	// (Mistral API может не поддерживать PATCH/UPDATE агентов)

	// Получаем все модели пользователя и находим нужную
	allModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	var existingModelData *UserModelRecord
	for i := range allModels {
		if allModels[i].Provider == existing.Provider {
			existingModelData = &allModels[i]
			break
		}
	}

	if existingModelData == nil {
		return fmt.Errorf("запись модели провайдера %s не найдена для пользователя", existing.Provider)
	}

	// Проверяем, изменились ли файлы (аналогично OpenAI)
	// Если файлы не изменились - используем существующие VectorId (library_ids)
	if !slices.EqualFunc(existing.FileIds, updated.FileIds, func(a, b Ids) bool {
		return a.ID == b.ID && a.Name == b.Name
	}) {
		// Файлы изменились - библиотека уже обновлена, используем новые данные
		//logger.Debug("Файлы изменились, используем обновленные данные библиотеки", userId)
	} else {
		// Файлы не изменились - используем существующие VectorId и FileIds
		updated.VecIds.VectorId = existing.VecIds.VectorId
		updated.FileIds = existing.FileIds
	}

	// Удаляем старого агента
	if err := m.mistralClient.deleteAgent(existingModelData.AssistId); err != nil {
		//logger.Warn("Не удалось удалить старого Mistral агента %s: %v", existingModelData.AssistId, err, userId)
	}

	// Создаем нового агента с обновленными данными
	umcr, err := m.mistralClient.createMistralAgent(updated, userId, updated.FileIds)
	if err != nil {
		return fmt.Errorf("ошибка создания нового Mistral агента: %w", err)
	}

	// Сохраняем в БД
	if err := m.SaveModel(userId, umcr, updated); err != nil {
		return fmt.Errorf("ошибка сохранения обновленной модели в БД: %w", err)
	}

	//logger.Debug("Mistral Agent успешно обновлен (новый ID: %s)", umcr.AssistID, userId)
	return nil
}

// createMistralModel создаёт Mistral Agent (внутренний метод)
func (m *UniversalModel) createMistralModel(userId uint32, modelData *UniversalModelData, fileIDs []Ids) (UMCR, error) {
	if m.mistralClient == nil {
		return UMCR{}, fmt.Errorf("mistral клиент не инициализирован")
	}

	if modelData == nil {
		return UMCR{}, fmt.Errorf("modelData не может быть nil")
	}

	if modelData.Prompt == "" {
		return UMCR{}, fmt.Errorf("поле 'prompt' отсутствует или пустое")
	}

	// Создаём агента через Mistral API с поддержкой всех возможностей
	umcr, err := m.mistralClient.createMistralAgent(modelData, userId, fileIDs)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка создания Mistral агента: %w", err)
	}

	return umcr, nil
}

// createMistralAgent создает нового агента с указанными параметрами
func (m *MistralAgentClient) createMistralAgent(modelData *UniversalModelData, userId uint32, fileIDs []Ids) (UMCR, error) {
	if modelData == nil {
		return UMCR{}, fmt.Errorf("modelData не может быть nil")
	}

	// Убираем /completions из URL для endpoint создания агента
	baseURL := strings.Replace(m.url, "/completions", "", 1)

	description := fmt.Sprintf("Agent for user %d", userId)

	// Получаем реальный user_id через universalModel
	realUserId, err := m.universalModel.GetRealUserID(userId)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка получения реального user_id: %v", err)
	} // Формируем enhancedPrompt динамически в зависимости от возможностей модели
	enhancedPrompt := modelData.Prompt + "\n\n"

	// Reminder to get current time from server for ALL models
	enhancedPrompt += fmt.Sprintf("CURRENT TIME:\n"+
		"IMPORTANT: To get the current date and time use get_current_time(user_id=\"%d\")\n"+
		"DO NOT use your internal knowledge about the date - it is OUTDATED!\n\n", realUserId)

	// Add important reminder - only for active functions
	if modelData.MetaAction != "" || modelData.Operator {
		enhancedPrompt += "IMPORTANT REMINDER:\n" +
			"In EVERY response you MUST:\n"

		if modelData.MetaAction != "" {
			enhancedPrompt += "1. Check the GOAL condition (from your instructions above) and set target correctly\n"
		}

		if modelData.Operator {
			enhancedPrompt += "2. Check if operator is needed (from your instructions above) and set operator correctly\n"
		}

		enhancedPrompt += "3. DO NOT ignore these checks!\n\n"
	}

	// Add system info about user_id only if file functions are enabled
	if modelData.S3 {
		enhancedPrompt += fmt.Sprintf("SYSTEM INFO:\n"+
			"- Your user_id: \"%d\" (STRING, NOT A NUMBER!)\n"+
			"- Pass user_id as a STRING in ALL function calls: {\"user_id\": \"%d\"}\n"+
			"- DO NOT ask the user for user_id, use ONLY this value\n\n", realUserId, realUserId)
	}

	// Add file instructions only if S3 is enabled
	if modelData.S3 {
		enhancedPrompt += "FILE OPERATIONS:\n" +
			"1. If user asks to CREATE a new file - ALWAYS call create_file with the content first\n" +
			"2. After creating the file call get_s3_files to get the updated list with the new file\n" +
			"3. Then send the created file via send_files\n" +
			"4. If user asks to show existing files - call get_s3_files and send the needed ones\n" +
			"5. Determine file type: .jpg/.png/.gif → photo, .mp4 → video, .mp3 → audio, .txt/.pdf etc → doc\n\n"
	}

	// Add image generation instructions only if Image is enabled
	if modelData.Image {
		enhancedPrompt += "IMAGE GENERATION:\n" +
			"When user asks to draw/generate/create an image:\n" +
			"1. Describe in your text response what you are drawing\n" +
			"2. The system will AUTOMATICALLY generate and send the image to the user\n" +
			"3. DO NOT add files to send_files - they will be added automatically!\n" +
			"4. Just tell the user you are creating the image\n\n"
	}

	// Add web search instructions only if WebSearch is enabled
	if modelData.WebSearch {
		enhancedPrompt += "WEB SEARCH:\n" +
			"When user asks a question requiring up-to-date information from the internet:\n" +
			"1. The system will AUTOMATICALLY perform an internet search\n" +
			"2. Use the results to form your answer\n" +
			"3. Reference sources when appropriate\n\n"
	}

	// Add GOOGLE CALENDAR instructions
	if modelData.GOAuth.HasCalendar() {
		enhancedPrompt += "GOOGLE CALENDAR - Event management:\n" +
			"You have access to the user's Google Calendar.\n\n" +
			fmt.Sprintf("user_id for all functions: \"%d\" (string)\n\n", realUserId) +
			"Available functions:\n" +
			"- calendar_create_event - create event\n" +
			"- calendar_list_events - list events\n" +
			"- calendar_delete_event - delete event\n" +
			"- calendar_get_event - event details\n\n" +
			"IMPORTANT for time handling:\n" +
			"- Time format: RFC3339 (e.g.: \"2026-02-05T15:00:00+03:00\")\n" +
			"- ALWAYS call get_current_time BEFORE calculating dates!\n" +
			"- Default duration: 1 hour\n" +
			"- After create/delete confirm the action and show the link\n\n" +
			"CRITICAL - DELETING EVENTS:\n" +
			"When user asks to DELETE an event:\n" +
			"1. FORBIDDEN to create new events (calendar_create_event)\n" +
			"2. Deletion algorithm:\n" +
			"   a) FIRST get event list: calendar_list_events\n" +
			"   b) Find the required event_id in results\n" +
			"   c) THEN delete each: calendar_delete_event(user_id, event_id)\n" +
			"3. For \"all events today\": get via calendar_list_events, delete each one\n" +
			"4. DO NOT create events when deleting!\n\n"
	}

	// Add GOOGLE SHEETS instructions
	if modelData.GOAuth.HasSheets() {
		enhancedPrompt += "GOOGLE SHEETS - Spreadsheet operations:\n" +
			"You have access to the user's Google Sheets.\n\n" +
			fmt.Sprintf("user_id for all functions: \"%d\" (string)\n\n", realUserId) +
			"CRITICAL - ALWAYS CALL FUNCTIONS:\n" +
			"1. User asks about table data → IMMEDIATELY call sheets_read_range\n" +
			"2. Need to count rows → call sheets_read_range, count len(values)-1 (minus headers)\n" +
			"3. Need to write data → call sheets_write_range\n" +
			"4. Need to append a row → call sheets_append_range\n" +
			"5. DO NOT reason about API methods (getMaxRows, getDataRange, Google Apps Script)!\n" +
			"6. DO NOT suggest writing scripts in Google Apps Script or Python!\n" +
			"7. ACT: call the available functions RIGHT NOW!\n\n" +
			"Available functions:\n" +
			"- sheets_read_range - read data from spreadsheet\n" +
			"- sheets_write_range - write/update data\n" +
			"- sheets_append_range - append rows to the end\n" +
			"- sheets_create_spreadsheet - create new spreadsheet\n" +
			"- sheets_get_info - spreadsheet info (sheets, sizes)\n\n" +
			"IMPORTANT:\n" +
			"- spreadsheet_id comes from user's prompt or URL (between /d/ and /edit)\n" +
			"- If a table ID is given in the prompt - use IT (full ID from prompt)\n" +
			"- Range format: 'Sheet1!A1:F100' or 'Sheet1!A:F' (whole sheet)\n" +
			"- To count rows use: len(values) - 1 (subtract headers)\n" +
			"- Always read current data before writing\n" +
			"- Report result after operations (row/cell count)\n\n"
	}

	// Add file type mapping only if S3 or Image is enabled
	if modelData.S3 || modelData.Image {
		enhancedPrompt += "File type for send_files:\n" +
			"   - .jpg/.png/.gif/.webp → \"photo\"\n" +
			"   - .mp4/.avi → \"video\"\n" +
			"   - .mp3/.wav → \"audio\"\n" +
			"   - .txt/.pdf/.doc and others → \"doc\"\n\n"
	}

	// Add instructions for target and operator fields
	enhancedPrompt += "RULES for JSON response fields:\n\n"

	// target instructions
	if modelData.MetaAction != "" {
		enhancedPrompt += "**target** (boolean) - Is the dialog GOAL achieved:\n" +
			"  Check the goal condition from YOUR INSTRUCTIONS ABOVE\n" +
			"  If condition is EXACTLY met → target: true\n" +
			"  If condition is NOT met → target: false\n\n"
	} else {
		enhancedPrompt += "**target**: ALWAYS false (no goal)\n\n"
	}

	// operator instructions
	if modelData.Operator {
		enhancedPrompt += "**operator** (boolean) - Is operator required:\n" +
			"  Check the operator condition from YOUR INSTRUCTIONS ABOVE\n" +
			"  If user requests operator → operator: true\n" +
			"  In all other cases → operator: false\n\n"
	} else {
		enhancedPrompt += "**operator**: ALWAYS false (operator disabled)\n\n"
	}

	// Final instruction for response format (always)
	enhancedPrompt += "IMPORTANT: Your response MUST be valid JSON (you may wrap in ```json):\n" +
		MistralSchemaJSON + "\n\n" +
		"Always return response strictly in this JSON format. You may use markdown: ```json\\n{...}\\n```"

	payload := map[string]interface{}{
		"name":         modelData.Name,
		"model":        modelData.GptType.Name,
		"description":  description,
		"instructions": enhancedPrompt,
	}

	// ВАЖНО: Mistral API НЕ поддерживает response_format при создании агентов!
	// response_format работает только в AI Studio UI, но не через API.
	// Структурированный JSON вывод настраивается через instructions в промпте.
	// Документация: https://docs.mistral.ai/api/#tag/agents

	// Формируем массив tools (функции и built-in tools)
	var tools []map[string]interface{}

	// Добавляем функцию get_current_time ВСЕГДА (для получения актуального времени)
	// ВАЖНО: user_id передается через промпт (enhancedPrompt выше), а не через const в параметрах
	tools = append(tools,
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name": "get_current_time",
				"description": "Returns the EXACT current time and date from the server in the user's timezone. " +
					"ALWAYS use this function BEFORE calculating dates (tomorrow, next week, on Monday, etc.). " +
					"DO NOT use your internal knowledge about the date - it is OUTDATED!",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "User ID",
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
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "get_s3_files",
					"description": "Returns the list of user's available files from S3",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "User ID",
							},
						},
						"required": []string{"user_id"},
					},
				},
			},
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "create_file",
					"description": "Creates a text file and saves it to S3",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "User ID",
							},
							"content": map[string]interface{}{
								"type":        "string",
								"description": "Text content of the file",
							},
							"file_name": map[string]interface{}{
								"type":        "string",
								"description": "File name with extension (.txt, .md, etc.)",
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
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "calendar_create_event",
					"description": "Creates a new event in the user's Google Calendar",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "User ID",
							},
							"title": map[string]interface{}{
								"type":        "string",
								"description": "Event title",
							},
							"description": map[string]interface{}{
								"type":        "string",
								"description": "Event description (optional)",
							},
							"start_time": map[string]interface{}{
								"type":        "string",
								"description": "Start time in RFC3339 format",
							},
							"end_time": map[string]interface{}{
								"type":        "string",
								"description": "End time in RFC3339 format",
							},
							"location": map[string]interface{}{
								"type":        "string",
								"description": "Event location (optional)",
							},
						},
						"required": []string{"user_id", "title", "start_time", "end_time"},
					},
				},
			},
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "calendar_list_events",
					"description": "Retrieves a list of events from Google Calendar",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "User ID",
							},
							"time_min": map[string]interface{}{
								"type":        "string",
								"description": "Period start in RFC3339 (optional)",
							},
							"time_max": map[string]interface{}{
								"type":        "string",
								"description": "Period end in RFC3339 (optional)",
							},
							"max_results": map[string]interface{}{
								"type":        "integer",
								"description": "Maximum number of events (default 10)",
							},
						},
						"required": []string{"user_id"},
					},
				},
			},
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "calendar_delete_event",
					"description": "Deletes an event from Google Calendar",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "User ID",
							},
							"event_id": map[string]interface{}{
								"type":        "string",
								"description": "Event ID to delete",
							},
						},
						"required": []string{"user_id", "event_id"},
					},
				},
			},
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "calendar_get_event",
					"description": "Gets event details from Google Calendar",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "User ID",
							},
							"event_id": map[string]interface{}{
								"type":        "string",
								"description": "Event ID to get details for",
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
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "sheets_read_range",
					"description": "Reads data from the specified range in Google Sheets",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "User ID",
							},
							"spreadsheet_id": map[string]interface{}{
								"type":        "string",
								"description": "Google Sheets spreadsheet ID (from URL or prompt)",
							},
							"range": map[string]interface{}{
								"type":        "string",
								"description": "Range to read (e.g.: 'Sheet1!A:F' or 'Sheet1!A1:D10')",
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range"},
					},
				},
			},
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "sheets_write_range",
					"description": "Writes data to the specified range in Google Sheets",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "User ID",
							},
							"spreadsheet_id": map[string]interface{}{
								"type":        "string",
								"description": "Google Sheets spreadsheet ID",
							},
							"range": map[string]interface{}{
								"type":        "string",
								"description": "Starting cell for writing (e.g.: 'Sheet1!A1')",
							},
							"values": map[string]interface{}{
								"type":        "array",
								"description": "2D array of values to write",
								"items": map[string]interface{}{
									"type": "array",
									"items": map[string]interface{}{
										"type": "string",
									},
								},
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range", "values"},
					},
				},
			},
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "sheets_append_range",
					"description": "Appends data to the end of a Google Sheets spreadsheet",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "User ID",
							},
							"spreadsheet_id": map[string]interface{}{
								"type":        "string",
								"description": "Google Sheets spreadsheet ID",
							},
							"range": map[string]interface{}{
								"type":        "string",
								"description": "Column range to append to (e.g.: 'Sheet1!A:D')",
							},
							"values": map[string]interface{}{
								"type":        "array",
								"description": "2D array of values to append",
								"items": map[string]interface{}{
									"type": "array",
									"items": map[string]interface{}{
										"type": "string",
									},
								},
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range", "values"},
					},
				},
			},
		)
	}

	// Добавляем функцию lead_target если есть MetaAction
	if modelData.MetaAction != "" {
		tools = append(tools,
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "lead_target",
					"description": "Triggers a meta-action to achieve the dialog goal",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"resp_id": map[string]interface{}{
								"type":        "integer",
								"description": "Respondent ID",
							},
						},
						"required": []string{"resp_id"},
					},
				},
			},
		)
	}

	// Добавляем built-in tools (встроенные возможности Mistral)
	// Согласно документации: https://docs.mistral.ai/agents/tools/built-in/
	// ВАЖНО: Названия должны точно совпадать с API!
	if modelData.Interpreter {
		tools = append(tools, map[string]interface{}{
			"type": "code_interpreter",
		})
	}
	if modelData.Image {
		tools = append(tools, map[string]interface{}{
			"type": "image_generation",
		})
	}
	if modelData.WebSearch {
		tools = append(tools, map[string]interface{}{
			"type": "web_search",
		})
	}

	// Добавляем document_library если есть поиск по документам или загружены файлы
	if modelData.Search || len(fileIDs) > 0 || len(modelData.VecIds.VectorId) > 0 {
		documentLibraryTool := map[string]interface{}{
			"type": "document_library",
		}

		// library_ids должен быть на том же уровне что и type
		// Согласно документации: https://docs.mistral.ai/agents/tools/built-in/document_library
		if len(modelData.VecIds.VectorId) > 0 {
			documentLibraryTool["library_ids"] = modelData.VecIds.VectorId
		}

		tools = append(tools, documentLibraryTool)
	}

	// Добавляем tools в payload если есть
	if len(tools) > 0 {
		payload["tools"] = tools
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, baseURL, bytes.NewBuffer(body))
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка создания POST запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return UMCR{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	var response map[string]interface{}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return UMCR{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	// Извлекаем ID созданного агента
	agentID, ok := response["id"].(string)
	if !ok {
		return UMCR{}, fmt.Errorf("не удалось получить ID созданного агента")
	}

	// Формируем AllIds аналогично OpenAI
	// Структура: {"FileIds": [...], "VectorId": [...]}
	// Если нет файлов и библиотеки - возвращаем nil (будет NULL в БД)
	var allIds []byte

	// Проверяем есть ли хоть что-то для сохранения
	hasFiles := len(fileIDs) > 0
	hasLibrary := len(modelData.VecIds.VectorId) > 0

	if hasFiles || hasLibrary {
		// Есть данные - формируем JSON
		type VecIds struct {
			FileIds  []Ids    `json:"FileIds"`
			VectorId []string `json:"VectorId"`
		}

		vecIds := VecIds{
			FileIds:  fileIDs,                   // ID документов в библиотеке
			VectorId: modelData.VecIds.VectorId, // ID библиотеки
		}

		// Преобразуем в JSON
		var err error
		allIds, err = json.Marshal(vecIds)
		if err != nil {
			return UMCR{}, fmt.Errorf("ошибка при преобразовании vecIds в JSON: %w", err)
		}
	} else {
		// Нет данных - оставляем nil (будет NULL в БД)
		allIds = nil
	}

	return UMCR{
		AssistID: agentID,
		AllIds:   allIds,
		Provider: ProviderMistral,
	}, nil
}

// ============================================================================
// LIBRARY MANAGEMENT API - Управление постоянными библиотеками документов
// Документация: https://docs.mistral.ai/agents/tools/built-in/document_library
// ============================================================================

// executeMistralRequest выполняет HTTP запрос к Mistral API с базовой обработкой
// method: HTTP метод (GET, DELETE, POST и т.д.)
// url: полный URL запроса
// body: тело запроса (может быть nil)
// successStatuses: список допустимых статус-кодов (если nil, то только OK)
func (m *MistralAgentClient) executeMistralRequest(method, url string, body []byte, successStatuses []int) ([]byte, error) {
	var req *http.Request
	var err error

	if body != nil {
		req, err = http.NewRequestWithContext(m.ctx, method, url, bytes.NewBuffer(body))
	} else {
		req, err = http.NewRequestWithContext(m.ctx, method, url, nil)
	}

	if err != nil {
		return nil, fmt.Errorf("ошибка создания %s запроса: %w", method, err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка HTTP запроса: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения ответа: %w", err)
	}

	// Если successStatuses не указан, проверяем только OK (200)
	if successStatuses == nil {
		successStatuses = []int{http.StatusOK}
	}

	// Проверяем, является ли статус успешным
	isSuccess := false
	for _, status := range successStatuses {
		if resp.StatusCode == status {
			isSuccess = true
			break
		}
	}

	if !isSuccess {
		return nil, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return responseBody, nil
}

// executeMistralDeleteRequest удаляет через общий API (DELETE)
// Допускает статусы OK, NoContent и NotFound как успешные
func (m *MistralAgentClient) executeMistralDeleteRequest(url string) error {
	_, err := m.executeMistralRequest(http.MethodDelete, url, nil,
		[]int{http.StatusOK, http.StatusNoContent, http.StatusNotFound})
	return err
}

// executeMistralGetRequest получает данные через общий API (GET)
func (m *MistralAgentClient) executeMistralGetRequest(url string) ([]byte, error) {
	return m.executeMistralRequest(http.MethodGet, url, nil, nil)
}

// ListLibraries получает список всех библиотек
func (m *MistralAgentClient) ListLibraries() ([]MistralLibrary, error) {
	const librariesURL = "https://api.mistral.ai/v1/libraries"

	responseBody, err := m.executeMistralGetRequest(librariesURL)
	if err != nil {
		return nil, fmt.Errorf("ошибка при вызове API: %w", err)
	}

	var response struct {
		Data []MistralLibrary `json:"data"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return response.Data, nil
}

// DeleteLibrary удаляет библиотеку
func (m *MistralAgentClient) DeleteLibrary(libraryID string) error {
	url := fmt.Sprintf("https://api.mistral.ai/v1/libraries/%s", libraryID)

	return m.executeMistralDeleteRequest(url)
}

// DeleteDocumentFromLibrary удаляет документ из библиотеки
// DELETE /v1/libraries/{library_id}/documents/{document_id}
func (m *MistralAgentClient) DeleteDocumentFromLibrary(libraryID, documentID string) error {
	url := fmt.Sprintf("https://api.mistral.ai/v1/libraries/%s/documents/%s", libraryID, documentID)

	return m.executeMistralDeleteRequest(url)
}
