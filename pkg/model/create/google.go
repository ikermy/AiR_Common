package create

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
)

// GoogleSchemaJSON - JSON Schema для структурированных ответов Gemini Agent
// Используется в response_schema для форсирования JSON формата ответов
const GoogleSchemaJSON = `{
	"type": "object",
	"properties": {
		"message": {
			"type": "string",
			"description": "Текстовое сообщение для пользователя. Оставь пустым (\"\") если отправляешь файлы с caption!"
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
							"url": {
								"type": "string",
								"description": "URL файла"
							},
							"file_name": {
								"type": "string",
								"description": "Имя файла"
							},
							"caption": {
								"type": "string",
								"description": "Подпись к файлу - используй это поле для сообщения пользователю при отправке файлов"
							}
						},
						"required": ["type", "url", "file_name", "caption"]
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
	"required": ["action", "target", "operator"]
}`

// GoogleModel представляет информацию о модели Gemini
type GoogleModel struct {
	Name                       string   `json:"name"`
	BaseModelID                string   `json:"baseModelId"`
	Version                    string   `json:"version"`
	DisplayName                string   `json:"displayName"`
	Description                string   `json:"description"`
	InputTokenLimit            int      `json:"inputTokenLimit"`
	OutputTokenLimit           int      `json:"outputTokenLimit"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	Temperature                float64  `json:"temperature,omitempty"`
	TopP                       float64  `json:"topP,omitempty"`
	TopK                       int      `json:"topK,omitempty"`
}

// GoogleModelsResponse представляет ответ от API со списком моделей
type GoogleModelsResponse struct {
	Models        []GoogleModel `json:"models"`
	NextPageToken string        `json:"nextPageToken,omitempty"`
}

// GoogleAgentClient клиент для работы с Google Gemini API
type GoogleAgentClient struct {
	apiKey         string
	url            string
	ctx            context.Context
	universalModel *UniversalModel // Ссылка на universalModel для доступа к GetRealUserID
}

// NewGoogleAgentClient создаёт новый экземпляр GoogleAgentClient с API ключом
func NewGoogleAgentClient(ctx context.Context, apiKey string) *GoogleAgentClient {
	return &GoogleAgentClient{
		apiKey: apiKey,
		url:    mode.GoogleAgentsURL,
		ctx:    ctx,
	}
}

// SetUniversalModel устанавливает ссылку на UniversalModel (используется после создания)
func (m *GoogleAgentClient) SetUniversalModel(um *UniversalModel) {
	m.universalModel = um
}

// createGoogleAgent создает нового Gemini агента с указанными параметрами
func (m *GoogleAgentClient) createGoogleAgent(modelData *UniversalModelData, userId uint32, fileIDs []Ids) (UMCR, error) {
	if modelData == nil {
		return UMCR{}, fmt.Errorf("modelData не может быть nil")
	}

	if modelData.GptType == nil || modelData.GptType.Name == "" {
		return UMCR{}, fmt.Errorf("modelData.GptType.Name не может быть пустым")
	}

	// Получаем реальный user_id через universalModel
	realUserId, err := m.universalModel.GetRealUserID(userId)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка получения реального user_id: %v", err)
	}

	// Формируем enhancedPrompt динамически в зависимости от возможностей модели
	enhancedPrompt := modelData.Prompt + "\n\n"

	// Напоминание о необходимости получить актуальное время с сервера для ВСЕХ моделей
	enhancedPrompt += fmt.Sprintf("ТЕКУЩЕЕ ВРЕМЯ:\n"+
		"ВАЖНО: Для получения актуальной даты и времени используй функцию get_current_time(user_id=\"%d\")\n"+
		"НЕ используй свои внутренние знания о дате - они УСТАРЕЛИ!\n\n", realUserId)

	// Добавляем важное напоминание - только для активных функций
	if modelData.MetaAction != "" || modelData.Operator {
		enhancedPrompt += "ВАЖНОЕ НАПОМИНАНИЕ:\n" +
			"В КАЖДОМ ответе ты ОБЯЗАН:\n"

		if modelData.MetaAction != "" {
			enhancedPrompt += "1. Проверить условие достижения ЦЕЛИ (из твоих инструкций выше) и правильно установить target\n"
		}

		if modelData.Operator {
			enhancedPrompt += "2. Проверить нужен ли оператор (из твоих инструкций выше) и правильно установить operator\n"
		}

		enhancedPrompt += "3. НЕ ИГНОРИРУЙ эти проверки!\n\n"
	}

	// Добавляем инструкции по работе с файлами (оптимизированная версия)
	if modelData.S3 {
		enhancedPrompt += "S3: get_s3_files, create_file\n" +
			"Типы: .jpg/.png=photo, .mp4=video, .mp3=audio, остальное=doc\n" +
			"При отправке файлов: используй caption (НЕ message)\n\n"
	}

	// Добавляем инструкции по Code Interpreter только если Interpreter включен
	if modelData.Interpreter {
		enhancedPrompt += "CODE INTERPRETER:\n" +
			"Ты можешь выполнять код для:\n" +
			"- Анализа данных и вычислений\n" +
			"- Создания графиков и визуализаций\n" +
			"- Обработки файлов (CSV, Excel, JSON и т.д.)\n" +
			"Используй code execution когда это необходимо\n\n"
	}

	// Добавляем инструкции по генерации видео только если Video включен
	if modelData.Video {
		enhancedPrompt += "ГЕНЕРАЦИЯ ВИДЕО:\n" +
			"Когда пользователь просит создать/сгенерировать/нарисовать видео:\n" +
			"1. Опиши в своём текстовом ответе что ты создаёшь\n" +
			"2. Система АВТОМАТИЧЕСКИ сгенерирует и отправит видео пользователю\n" +
			"3. Можешь указать параметры: длительность (4-8 сек), соотношение сторон (16:9, 9:16, 1:1)\n" +
			"4. НЕ добавляй видео файлы в send_files - они добавятся автоматически!\n" +
			"5. Просто ответь пользователю что создаёшь видео с описанием\n\n"
	}

	// Добавляем инструкции по ВЕБ-ПОИСКУ
	if modelData.WebSearch {
		enhancedPrompt += "ВЕБ-ПОИСК (Google Search):\n" +
			"У тебя есть доступ к актуальной информации в интернете через инструмент Google Search.\n" +
			"🔍 ОБЯЗАТЕЛЬНО используй google_search когда:\n" +
			"   - Пользователь спрашивает о текущих событиях, погоде, новостях, курсах валют\n" +
			"   - Запрашивает информацию, которой нет в твоей базе знаний (данные после октября 2023)\n" +
			"   - Просит актуальные факты о компаниях, людях, местах\n" +
			"   - Спрашивает \"что в интернете\", \"найди информацию\", \"погугли\"\n" +
			"1. ВСЕГДА используй поиск для запросов с датами, временем, актуальной статистикой\n" +
			"2. После получения результатов поиска - обобщи их в понятном виде\n" +
			"3. Указывай источники информации когда это уместно\n" +
			"4. Если ты НЕ УВЕРЕН в информации - ИСПОЛЬЗУЙ ПОИСК вместо отказа!\n\n"
	}

	// Добавляем инструкции по GOOGLE CALENDAR (оптимизированная версия)
	if modelData.GOAuth.HasCalendar() {
		enhancedPrompt += fmt.Sprintf("CALENDAR: user_id=\"%d\"\n"+
			"Функции: calendar_create_event, calendar_list_events, calendar_delete_event\n"+
			"RFC3339: \"2026-02-05T15:00:00+03:00\"\n"+
			"ВСЕГДА вызывай get_current_time ПЕРЕД расчётом дат!\n"+
			"После операции - подтверди действие с деталями\n\n",
			realUserId)
	}

	// Добавляем инструкции по GOOGLE SHEETS (оптимизированная версия)
	if modelData.GOAuth.HasSheets() {
		enhancedPrompt += fmt.Sprintf("SHEETS: user_id=\"%d\"\n"+
			"spreadsheet_id из промпта (ПОЛНЫЙ ID ~40 символов, НЕ название!)\n"+
			"Функции: sheets_read_range (чтение), sheets_write_range (запись), sheets_append_range (добавление)\n"+
			"После вызова функции - обработай результат и покажи данные пользователю\n\n", realUserId)
	}

	// Добавляем инструкции по ГЕНЕРАЦИИ ИЗОБРАЖЕНИЙ
	if modelData.Image {
		enhancedPrompt += "ГЕНЕРАЦИЯ ИЗОБРАЖЕНИЙ:\n" +
			"Когда пользователь просит создать/нарисовать/сгенерировать изображение:\n" +
			"1. Подробно опиши в своём ответе (в поле message), что ты создаешь.\n" +
			"2. Система АВТОМАТИЧЕСКИ сгенерирует изображение на основе твоего описания и добавит его в send_files.\n" +
			"3. ВАЖНО: НЕ добавляй изображения в send_files самостоятельно! Оставь send_files пустым [].\n" +
			"4. НЕ придумывай fake URL (example.com и т.д.) - система сама добавит реальный URL после генерации.\n" +
			"5. Просто опиши что создаёшь в поле message, и система сделает всё остальное.\n\n"
	}

	// Добавляем инструкции по полям target и operator
	enhancedPrompt += "ПРАВИЛА для полей JSON ответа:\n\n"

	// Инструкции по target
	if modelData.MetaAction != "" {
		enhancedPrompt += "**target** (boolean) - Достигнута ли ЦЕЛЬ диалога:\n" +
			"  Проверяй условие достижения цели из СВОИХ ИНСТРУКЦИЙ ВЫШЕ\n" +
			"  Если условие ТОЧНО выполнено → target: true\n" +
			"  Если условие НЕ выполнено → target: false\n\n"
	} else {
		enhancedPrompt += "**target**: ВСЕГДА false (цели нет)\n\n"
	}

	// Инструкции по operator
	if modelData.Operator {
		enhancedPrompt += "**operator** (boolean) - Требуется ли оператор:\n" +
			"  Проверяй условие вызова оператора из СВОИХ ИНСТРУКЦИЙ ВЫШЕ\n" +
			"  Если пользователь просит оператора → operator: true\n" +
			"  Во всех остальных случаях → operator: false\n\n"
	} else {
		enhancedPrompt += "**operator**: ВСЕГДА false (вызов оператора отключен)\n\n"
	}

	// Финальная инструкция по формату ответа
	enhancedPrompt += "ВАЖНО: Твой ответ ДОЛЖЕН быть валидным JSON в следующем формате:\n" +
		GoogleSchemaJSON + "\n\n" +
		"Всегда возвращай ответ строго в этом JSON формате."

	// Формируем payload для создания агента
	// В Google Gemini API используется system_instruction для промпта
	payload := map[string]interface{}{
		"system_instruction": map[string]interface{}{
			"parts": []map[string]interface{}{
				{
					"text": enhancedPrompt,
				},
			},
		},
	}

	// Добавляем generation_config с response_schema если нет tools
	// ВАЖНО: response_schema на этапе создания агента применяется только если нет function_declarations
	// При запросах (request.go) schema добавляется в зависимости от наличия tools в данном запросе
	hasTools := modelData.S3 || modelData.Interpreter || modelData.WebSearch || modelData.GOAuth.HasCalendar() || modelData.GOAuth.HasSheets()

	if !hasTools {
		// Только без tools можем добавить response_schema при создании
		payload["generation_config"] = map[string]interface{}{
			"response_mime_type": "application/json",
			"response_schema":    ParseGoogleSchemaJSON(),
		}
	}

	// ============================================================================
	// ФОРМИРОВАНИЕ TOOLS
	// ============================================================================
	// Инициализируем слайс инструментов
	var googleTools []map[string]interface{}

	// 1. Добавляем Веб-поиск (Google Search)
	// ВАЖНО: В новых версиях API используется просто "google_search" вместо "google_search_retrieval"
	if modelData.WebSearch {
		googleTools = append(googleTools, map[string]interface{}{
			"google_search": map[string]interface{}{},
		})
	}

	// 2. Добавляем S3 (Function Calling)
	if modelData.S3 {
		googleTools = append(googleTools, map[string]interface{}{
			"function_declarations": []map[string]interface{}{
				{
					"name":        "get_s3_files",
					"description": fmt.Sprintf("Получает список файлов пользователя из S3. ВАЖНО: user_id должен быть СТРОКОЙ \"%d\"", realUserId),
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserId),
							},
						},
						"required": []string{"user_id"},
					},
				},
				{
					"name":        "create_file",
					"description": "Создает новый файл в S3 хранилище пользователя",
					"parameters": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserId),
							},
							"content": map[string]interface{}{
								"type":        "string",
								"description": "Содержимое файла",
							},
							"file_name": map[string]interface{}{
								"type":        "string",
								"description": "Имя файла с расширением",
							},
						},
						"required": []string{"user_id", "content", "file_name"},
					},
				},
			},
		})
	}

	// 3. Добавляем Google Calendar (Function Calling)
	if modelData.GOAuth.HasCalendar() {
		calendarFunctions := []map[string]interface{}{
			{
				"name":        "calendar_create_event",
				"description": "Создает новое событие в Google Calendar пользователя",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserId),
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
							"description": "Время начала в RFC3339 формате (например: '2026-02-04T10:00:00Z')",
						},
						"end_time": map[string]interface{}{
							"type":        "string",
							"description": "Время окончания в RFC3339 формате",
						},
						"location": map[string]interface{}{
							"type":        "string",
							"description": "Место проведения (опционально)",
						},
						"attendees": map[string]interface{}{
							"type":        "array",
							"description": "Email адреса участников (опционально)",
							"items": map[string]interface{}{
								"type": "string",
							},
						},
					},
					"required": []string{"user_id", "title", "start_time", "end_time"},
				},
			},
			{
				"name":        "calendar_list_events",
				"description": "Получает список событий из Google Calendar пользователя",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserId),
						},
						"time_min": map[string]interface{}{
							"type":        "string",
							"description": "Начало периода в RFC3339 (опционально, по умолчанию - текущее время)",
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
			{
				"name":        "calendar_delete_event",
				"description": "Удаляет событие из Google Calendar",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserId),
						},
						"event_id": map[string]interface{}{
							"type":        "string",
							"description": "ID события для удаления",
						},
					},
					"required": []string{"user_id", "event_id"},
				},
			},
		}

		// Если уже есть function_declarations (например, от S3), добавляем к ним
		if modelData.S3 {
			// Находим существующий блок с function_declarations и добавляем функции Calendar
			for i, tool := range googleTools {
				if funcDecls, ok := tool["function_declarations"].([]map[string]interface{}); ok {
					googleTools[i]["function_declarations"] = append(funcDecls, calendarFunctions...)
					break
				}
			}
		} else {
			// Создаем новый блок function_declarations
			googleTools = append(googleTools, map[string]interface{}{
				"function_declarations": calendarFunctions,
			})
		}
	}

	// 4. Добавляем Google Sheets (Function Calling)
	if modelData.GOAuth.HasSheets() {
		sheetsFunctions := []map[string]interface{}{
			{
				"name":        "sheets_read_range",
				"description": "Читает данные из указанного диапазона в Google Sheets",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserId),
						},
						"spreadsheet_id": map[string]interface{}{
							"type":        "string",
							"description": "ID таблицы Google Sheets (из URL)",
						},
						"range": map[string]interface{}{
							"type":        "string",
							"description": "Диапазон для чтения (например: 'Sheet1!A1:D10')",
						},
					},
					"required": []string{"user_id", "spreadsheet_id", "range"},
				},
			},
			{
				"name":        "sheets_write_range",
				"description": "Записывает данные в указанный диапазон Google Sheets",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserId),
						},
						"spreadsheet_id": map[string]interface{}{
							"type":        "string",
							"description": "ID таблицы Google Sheets",
						},
						"range": map[string]interface{}{
							"type":        "string",
							"description": "Начальная ячейка для записи (например: 'Sheet1!A1')",
						},
						"values": map[string]interface{}{
							"type":        "array",
							"description": "Двумерный массив значений для записи",
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
			{
				"name":        "sheets_append_range",
				"description": "Добавляет данные в конец таблицы Google Sheets",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserId),
						},
						"spreadsheet_id": map[string]interface{}{
							"type":        "string",
							"description": "ID таблицы Google Sheets",
						},
						"range": map[string]interface{}{
							"type":        "string",
							"description": "Диапазон колонок для добавления (например: 'Sheet1!A:D')",
						},
						"values": map[string]interface{}{
							"type":        "array",
							"description": "Двумерный массив значений для добавления",
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
			{
				"name":        "sheets_create_spreadsheet",
				"description": "Создает новую таблицу Google Sheets",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserId),
						},
						"title": map[string]interface{}{
							"type":        "string",
							"description": "Название новой таблицы",
						},
						"sheet_names": map[string]interface{}{
							"type":        "array",
							"description": "Названия листов (опционально)",
							"items": map[string]interface{}{
								"type": "string",
							},
						},
					},
					"required": []string{"user_id", "title"},
				},
			},
		}

		// Добавляем к существующим function_declarations или создаем новый блок
		foundFuncDecls := false
		for i, tool := range googleTools {
			if funcDecls, ok := tool["function_declarations"].([]map[string]interface{}); ok {
				googleTools[i]["function_declarations"] = append(funcDecls, sheetsFunctions...)
				foundFuncDecls = true
				break
			}
		}
		if !foundFuncDecls {
			googleTools = append(googleTools, map[string]interface{}{
				"function_declarations": sheetsFunctions,
			})
		}
	}

	// 5. Добавляем Code Interpreter (только если нет S3 и нет GOAuth)
	// ВАЖНО: Google Gemini НЕ поддерживает одновременное использование
	// function_declarations и code_execution в одном запросе
	hasAnyFunctionDeclarations := modelData.S3 || modelData.GOAuth.Enabled()
	if modelData.Interpreter && !hasAnyFunctionDeclarations {
		googleTools = append(googleTools, map[string]interface{}{
			"code_execution": map[string]interface{}{},
		})
	}

	// Присваиваем собранные инструменты в payload
	if len(googleTools) > 0 {
		payload["tools"] = googleTools
	}

	// Google Gemini API не требует создания агента через отдельный endpoint
	// Вместо этого мы используем модель напрямую с system_instruction
	// Агентом является комбинация: model_name + system_instruction + tools
	// Поэтому AssistID будет составным идентификатором: "models/{model_name}"

	// Формируем AssistID как путь к модели
	agentID := fmt.Sprintf("models/%s", modelData.GptType.Name)

	// Проверяем доступность модели через тестовый запрос
	testURL := fmt.Sprintf("%s/%s:generateContent?key=%s", m.url, agentID, m.apiKey)

	// Формируем тестовый payload для проверки конфигурации
	testPayload := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]interface{}{
					{"text": "test"},
				},
			},
		},
	}

	// Добавляем нашу конфигурацию
	if sysInstr, ok := payload["system_instruction"]; ok {
		testPayload["system_instruction"] = sysInstr
	}
	if genConfig, ok := payload["generation_config"]; ok {
		testPayload["generationConfig"] = genConfig
	}
	if tools, ok := payload["tools"]; ok {
		testPayload["tools"] = tools
	}

	body, err := json.Marshal(testPayload)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка сериализации тестового запроса: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, testURL, bytes.NewBuffer(body))
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка создания POST запроса: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return UMCR{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	// Проверяем, что ответ валидный
	var response map[string]interface{}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return UMCR{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	// Проверяем наличие candidates в ответе (признак успешной конфигурации)
	if _, ok := response["candidates"]; !ok {
		return UMCR{}, fmt.Errorf("модель не вернула candidates, возможно конфигурация некорректна: %s", string(responseBody))
	}

	// Для Google моделей AllIds всегда nil (пустое поле Ids в БД)
	// Конфигурация модели не сохраняется в БД, только имя модели в AssistID
	// Эмбеддинги хранятся в отдельной таблице vector_embeddings

	return UMCR{
		AssistID: modelData.GptType.Name, // "просто имя модели например gemini-2.5-flash"
		AllIds:   nil,                    // Для Google моделей Ids всегда пустой (NULL в БД)
		Provider: ProviderGoogle,
	}, nil
}

// deleteGoogleAgent удаляет Google Gemini агента по ID
// Примечание: Google Gemini использует модели напрямую, без создания отдельных агентов
// Поэтому "удаление" агента - это просто удаление записи из БД
func (m *GoogleAgentClient) DeleteGoogleAgent(agentID string) error {
	if agentID == "" {
		return fmt.Errorf("agentID не может быть пустым")
	}

	// Google Gemini не требует удаления через API, так как мы используем публичные модели
	// Агент существует только как конфигурация в БД
	logger.Info("Google Gemini агент %s помечен для удаления (конфигурация будет удалена из БД)", agentID)

	// Если это tuned model (начинается с "tunedModels/"), пытаемся удалить
	if strings.HasPrefix(agentID, "tunedModels/") {
		deleteURL := fmt.Sprintf("%s/%s?key=%s", m.url, agentID, m.apiKey)

		req, err := http.NewRequestWithContext(m.ctx, http.MethodDelete, deleteURL, nil)
		if err != nil {
			return fmt.Errorf("ошибка создания DELETE запроса: %v", err)
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("ошибка HTTP запроса: %v", err)
		}
		defer resp.Body.Close()

		responseBody, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
		}

		logger.Info("Tuned model %s успешно удалён", agentID)
	}

	return nil
}

// ListModels получает список доступных моделей Google Gemini
// Возвращает список моделей, поддерживающих generateContent
func (m *GoogleAgentClient) GetModelsList() ([]GoogleModel, error) {
	listURL := fmt.Sprintf("%s/models?key=%s", m.url, m.apiKey)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания GET запроса: %v", err)
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

	var modelsResp GoogleModelsResponse
	if err := json.Unmarshal(responseBody, &modelsResp); err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	// Фильтруем модели, поддерживающие generateContent
	var validModels []GoogleModel
	for _, model := range modelsResp.Models {
		for _, method := range model.SupportedGenerationMethods {
			if method == "generateContent" {
				validModels = append(validModels, model)
				break
			}
		}
	}

	logger.Info("Получено %d моделей Google Gemini, из них %d поддерживают generateContent",
		len(modelsResp.Models), len(validModels))

	return validModels, nil
}

// GetModelInfo получает информацию о конкретной модели
func (m *GoogleAgentClient) GetModelInfo(modelName string) (*GoogleModel, error) {
	// Если modelName не содержит префикс "models/", добавляем его
	if !strings.HasPrefix(modelName, "models/") {
		modelName = "models/" + modelName
	}

	getURL := fmt.Sprintf("%s/%s?key=%s", m.url, modelName, m.apiKey)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, getURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания GET запроса: %v", err)
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

	var model GoogleModel
	if err := json.Unmarshal(responseBody, &model); err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return &model, nil
}

// ============================================================================
// VIDEO GENERATION - Генерация видео через Google Veo/Imagen 3
// Документация: https://ai.google.dev/gemini-api/docs/vision
// ============================================================================

// GenerateVideo генерирует видео по текстовому описанию
// Параметры:
// - prompt: текстовое описание видео
// - aspectRatio: "16:9", "9:16", "1:1" (по умолчанию "16:9")
// - duration: длительность в секундах 4-8 (по умолчанию 4)
// Возвращает: данные видео, MIME тип, ошибку
func (m *GoogleAgentClient) GenerateVideo(prompt string, aspectRatio string, duration int) ([]byte, string, error) {
	if prompt == "" {
		return nil, "", fmt.Errorf("пустой промпт для генерации видео")
	}

	// Валидация параметров
	if aspectRatio == "" {
		aspectRatio = "16:9"
	}
	if duration <= 0 || duration > 8 {
		duration = 4
	}

	// Получаем доступные модели
	//models, err := m.ListModels()
	//if err != nil {
	//	return nil, "", fmt.Errorf("не удалось получить список моделей: %w", err)
	//}

	// Ищем модель с поддержкой видео
	//var videoModel string
	//for _, model := range models {
	//	modelName := strings.TrimPrefix(Name, "models/")
	//	// Проверяем модели с поддержкой мультимодальности
	//	if strings.Contains(modelName, "gemini-2") || strings.Contains(modelName, "gemini-1.5-pro") {
	//		videoModel = modelName
	//		break
	//	}
	//}
	//
	//if videoModel == "" {
	//	return nil, "", fmt.Errorf("не найдена модель с поддержкой генерации видео")
	//}

	videoModel := "veo-3.1-fast-generate-preview"

	logger.Info("Используется модель для генерации видео: %s", videoModel)

	// Формируем расширенный промпт для генерации видео
	videoPrompt := fmt.Sprintf(`Generate a high-quality video based on this description: %s

Technical requirements:
- Duration: %d seconds
- Aspect ratio: %s
- High quality, smooth motion
- Cinematic style, professional look
- Rich details and vibrant colors

Please create a visually stunning video that captures the essence of the description.`,
		prompt, duration, aspectRatio)

	// Формируем запрос
	payload := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]interface{}{
					{
						"text": videoPrompt,
					},
				},
			},
		},
		"generationConfig": map[string]interface{}{
			"temperature":     0.9,
			"topK":            40,
			"topP":            0.95,
			"maxOutputTokens": 2048,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	// URL для генерации
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", m.url, videoModel, m.apiKey)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, "", fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	// Парсим ответ
	var videoResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text       string `json:"text,omitempty"`
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"` // base64
					} `json:"inlineData,omitempty"`
					FileData struct {
						FileURI  string `json:"fileUri"`
						MimeType string `json:"mimeType"`
					} `json:"fileData,omitempty"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.Unmarshal(responseBody, &videoResp); err != nil {
		return nil, "", fmt.Errorf("ошибка парсинга ответа: %v", err)
	}

	if len(videoResp.Candidates) == 0 || len(videoResp.Candidates[0].Content.Parts) == 0 {
		return nil, "", fmt.Errorf("получен пустой ответ от модели")
	}

	// Ищем видео в ответе
	for _, part := range videoResp.Candidates[0].Content.Parts {
		// Проверяем inline_data (base64)
		if part.InlineData.Data != "" && strings.HasPrefix(part.InlineData.MimeType, "video/") {
			// Декодируем base64
			videoData, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if err != nil {
				return nil, "", fmt.Errorf("ошибка декодирования base64: %v", err)
			}
			logger.Info("Видео успешно сгенерировано (inline_data), размер: %d bytes, mime: %s",
				len(videoData), part.InlineData.MimeType)
			return videoData, part.InlineData.MimeType, nil
		}

		// Проверяем file_data (URI)
		if part.FileData.FileURI != "" && strings.HasPrefix(part.FileData.MimeType, "video/") {
			videoData, err := m.DownloadVideoFromURI(part.FileData.FileURI)
			if err != nil {
				return nil, "", fmt.Errorf("ошибка скачивания видео: %v", err)
			}
			logger.Info("Видео успешно сгенерировано (file_uri), размер: %d bytes, mime: %s",
				len(videoData), part.FileData.MimeType)
			return videoData, part.FileData.MimeType, nil
		}
	}

	// Если видео не найдено, возвращаем информативное сообщение
	logger.Warn("Видео не найдено в ответе модели %s. Возможно модель не поддерживает генерацию видео или требуется другой промпт.", videoModel)
	return nil, "", fmt.Errorf("модель %s не сгенерировала видео. Попробуйте более подробное описание или используйте другую модель", videoModel)
}

// DownloadVideoFromURI скачивает видео по URI из Google File API
func (m *GoogleAgentClient) DownloadVideoFromURI(fileURI string) ([]byte, error) {
	if fileURI == "" {
		return nil, fmt.Errorf("пустой URI файла")
	}

	// Добавляем API ключ к запросу
	downloadURL := fmt.Sprintf("%s?key=%s", fileURI, m.apiKey)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания запроса: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	videoData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения видео: %v", err)
	}

	logger.Info("Видео успешно скачано с URI, размер: %d bytes", len(videoData))

	return videoData, nil
}

// GetAPIKey возвращает API ключ (используется в google/files.go)
func (m *GoogleAgentClient) GetAPIKey() string {
	return m.apiKey
}

// GetUrl возвращает API ключ (используется где то..)
func (m *GoogleAgentClient) GetUrl() string {
	return m.url
}

// ============================================================================
// AUDIO TRANSCRIPTION - Транскрибация аудио через Google Gemini
// Документация: https://ai.google.dev/gemini-api/docs/audio
// ============================================================================

// GoogleAudioResponse представляет ответ с транскрибацией
type GoogleAudioResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// TranscribeAudio транскрибирует аудио файл в текст
// Google Gemini поддерживает: MP3, WAV, FLAC, AAC, OGG, и другие форматы
// Для файлов до 20MB использует inline_data (base64)
func (m *GoogleAgentClient) TranscribeAudio(audioData []byte, mimeType string) (string, error) {
	if len(audioData) == 0 {
		return "", fmt.Errorf("пустые аудиоданные")
	}

	// Определяем mime type если не указан
	if mimeType == "" {
		mimeType = "audio/mpeg" // По умолчанию MP3
	}

	// Кодируем аудио в base64
	audioBase64 := base64.StdEncoding.EncodeToString(audioData)

	audioModel := "gemini-2.5-flash-lite"

	// Формируем запрос
	payload := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]interface{}{
					{
						"text": "Транскрибируй это аудио в текст. Верни только текст без дополнительных комментариев.",
					},
					{
						"inline_data": map[string]string{
							"mime_type": mimeType,
							"data":      audioBase64,
						},
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	// Отправляем запрос
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", m.url, audioModel, m.apiKey)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	// Парсим ответ
	var audioResp GoogleAudioResponse
	if err := json.Unmarshal(responseBody, &audioResp); err != nil {
		return "", fmt.Errorf("ошибка парсинга ответа: %v", err)
	}

	if len(audioResp.Candidates) == 0 || len(audioResp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("получен пустой ответ от модели")
	}

	transcription := audioResp.Candidates[0].Content.Parts[0].Text

	if transcription == "" {
		return "", fmt.Errorf("получен пустой текст транскрибации")
	}

	//logger.Debug("Успешная транскрибация аудио, длина текста: %d символов", len(transcription))

	return transcription, nil
}

// TranscribeAudioFile транскрибирует аудио файл используя File API (для больших файлов > 20MB)
func (m *GoogleAgentClient) TranscribeAudioFile(fileURI string) (string, error) {
	if fileURI == "" {
		return "", fmt.Errorf("пустой URI файла")
	}

	audioModel := "gemini-2.5-flash-lite"

	// Формируем запрос с file_data
	payload := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]interface{}{
					{
						"text": "Транскрибируй это аудио в текст. Верни только текст без дополнительных комментариев.",
					},
					{
						"file_data": map[string]string{
							"file_uri": fileURI,
						},
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", m.url, audioModel, m.apiKey)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	var audioResp GoogleAudioResponse
	if err := json.Unmarshal(responseBody, &audioResp); err != nil {
		return "", fmt.Errorf("ошибка парсинга ответа: %v", err)
	}

	if len(audioResp.Candidates) == 0 || len(audioResp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("получен пустой ответ от модели")
	}

	transcription := audioResp.Candidates[0].Content.Parts[0].Text

	logger.Info("Успешная транскрибация файла, длина текста: %d символов", len(transcription))

	return transcription, nil
}

// UploadAudioFile загружает аудио файл в Google File API для последующей транскрибации
// Возвращает URI файла для использования в TranscribeAudioFile
func (m *GoogleAgentClient) UploadAudioFile(fileName string, audioData []byte, mimeType string) (string, error) {
	if len(audioData) == 0 {
		return "", fmt.Errorf("пустые аудиоданные")
	}

	// URL для загрузки файлов
	//uploadURL := fmt.Sprintf("https://generativelanguage.googleapis.com/upload/v1beta/files?key=%s", m.apiKey)
	uploadURL := fmt.Sprintf("%s/files?key=%s", m.url, m.apiKey)

	// Создаем multipart запрос
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	// Добавляем metadata
	metadata := map[string]interface{}{
		"file": map[string]string{
			"display_name": fileName,
		},
	}
	metadataJSON, _ := json.Marshal(metadata)

	if err := writer.WriteField("metadata", string(metadataJSON)); err != nil {
		return "", fmt.Errorf("ошибка добавления metadata: %v", err)
	}

	// Добавляем файл
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return "", fmt.Errorf("ошибка создания form file: %v", err)
	}

	if _, err := part.Write(audioData); err != nil {
		return "", fmt.Errorf("ошибка записи данных файла: %v", err)
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("ошибка закрытия writer: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, uploadURL, &requestBody)
	if err != nil {
		return "", fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Goog-Upload-Protocol", "multipart")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	var uploadResp struct {
		File struct {
			Name string `json:"name"`
			URI  string `json:"uri"`
		} `json:"file"`
	}

	if err := json.Unmarshal(responseBody, &uploadResp); err != nil {
		return "", fmt.Errorf("ошибка парсинга ответа: %v", err)
	}

	if uploadResp.File.URI == "" {
		return "", fmt.Errorf("не получен URI загруженного файла")
	}

	logger.Info("Аудио файл успешно загружен: %s (URI: %s)", fileName, uploadResp.File.URI)

	return uploadResp.File.URI, nil
}

// DeleteAudioFile удаляет загруженный аудио файл из Google File API
func (m *GoogleAgentClient) DeleteAudioFile(fileName string) error {
	if fileName == "" {
		return fmt.Errorf("пустое имя файла")
	}

	deleteURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/%s?key=%s", fileName, m.apiKey)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodDelete, deleteURL, nil)
	if err != nil {
		return fmt.Errorf("ошибка создания DELETE запроса: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	logger.Info("Аудио файл %s успешно удалён", fileName)

	return nil
}

// ============================================================================
// EMBEDDING API - Генерация векторных эмбеддингов
// Документация: https://ai.google.dev/api/embeddings
// ============================================================================

// GenerateGoogleEmbedding - публичная функция для генерации эмбеддингов через Google API
// Используется как в updateGoogleModelInPlace, так и в GoogleGenerateEmbedding()
// Это единая точка для генерации эмбеддингов, избегающая дублирования кода
func GenerateGoogleEmbedding(ctx context.Context, apiKey, text string) ([]float32, error) {
	return generateGoogleEmbedding(ctx, apiKey, text)
}

// generateGoogleEmbedding - внутренняя функция для генерации эмбеддингов через Google API
func generateGoogleEmbedding(ctx context.Context, apiKey, text string) ([]float32, error) {
	if text == "" {
		return nil, fmt.Errorf("текст не может быть пустым")
	}

	embedURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/text-embedding-004:embedContent?key=%s", apiKey)

	payload := map[string]interface{}{
		"content": map[string]interface{}{
			"parts": []map[string]interface{}{
				{"text": text},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ошибка сериализации: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, embedURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("ошибка создания запроса: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка HTTP запроса: %w", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения ответа: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		logger.Error("generateGoogleEmbedding: API вернул %d: %s", resp.StatusCode, string(responseBody))
		return nil, fmt.Errorf("API вернул %d: %s", resp.StatusCode, string(responseBody))
	}

	var embedResp struct {
		Embedding struct {
			Values []float32 `json:"values"`
		} `json:"embedding"`
	}

	if err := json.Unmarshal(responseBody, &embedResp); err != nil {
		return nil, fmt.Errorf("ошибка парсинга ответа: %w", err)
	}

	if len(embedResp.Embedding.Values) == 0 {
		return nil, fmt.Errorf("API вернул пустой эмбеддинг")
	}

	//logger.Debug("generateGoogleEmbedding: создан эмбеддинг размерности %d", len(embedResp.Embedding.Values))
	return embedResp.Embedding.Values, nil
}

// updateGoogleModelInPlace обновляет модель google
func (m *UniversalModel) updateGoogleModelInPlace(userId uint32, existing, updated *UniversalModelData) error {
	if m.googleClient == nil {
		return fmt.Errorf("google клиент не инициализирован")
	}

	// Получаем все модели пользователя и находим нужную (нужен ModelId для работы с эмбеддингами)
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

	assistId := existingModelData.AssistId
	if assistId == "" {
		return fmt.Errorf("assistId для Google модели отсутствует")
	}

	modelId := existingModelData.ModelId
	if modelId == 0 {
		return fmt.Errorf("modelId для Google модели отсутствует")
	}

	// ============================================================================
	// УПРАВЛЕНИЕ ВЕКТОРНЫМ ХРАНИЛИЩЕМ В БД
	// ============================================================================
	// ВАЖНО: Эмбеддинги привязаны к конкретной модели через model_id
	// При удалении модели эмбеддинги удаляются автоматически (ON DELETE CASCADE)

	// Случай 1: Флаг Search отключён (Search: true → false)
	// Действие: Удалить ВСЕ эмбеддинги этой модели из БД
	if !updated.Search && existing.Search {
		logger.Info("Search отключён для modelId=%d, удаляем все эмбеддинги из БД", modelId)

		if err := m.db.DeleteAllModelEmbeddings(modelId); err != nil {
			logger.Warn("Не удалось удалить эмбеддинги для modelId=%d: %v", modelId, err)
		} else {
			logger.Info("Все эмбеддинги удалены для modelId=%d", modelId)
		}

		// Очищаем VectorIds (они всегда пустые для Google)
		updated.VecIds.VectorId = []string{}
		updated.VecIds.FileIds = []Ids{}
	} else if updated.Search {
		// Случай 2: Search включён - управляем эмбеддингами

		// Проверяем, изменились ли файлы
		filesChanged := !filesEqual(existing.FileIds, updated.FileIds)

		if filesChanged {
			logger.Info("Файлы изменились для modelId=%d, обновляем векторное хранилище в БД", modelId)

			// 2.1. Удаляем все старые эмбеддинги модели
			if len(existing.FileIds) > 0 {
				if err := m.db.DeleteAllModelEmbeddings(modelId); err != nil {
					logger.Warn("Не удалось удалить эмбеддинги для modelId=%d: %v", modelId, err)
				}
			}

			// 2.2. Добавляем новые файлы как эмбеддинги в БД
			if len(updated.FileIds) > 0 {
				logger.Info("Добавляем %d новых файлов как эмбеддинги в БД для modelId=%d", len(updated.FileIds), modelId)

				// Добавляем каждый файл как документ с эмбеддингом в MariaDB
				for idx, fileID := range updated.FileIds {
					if fileID.ID == "" {
						continue
					}

					// fileID.ID это URI файла в Google Files API
					fileURI := fileID.ID
					downloadURL := fmt.Sprintf("%s?key=%s", fileURI, m.googleClient.apiKey)

					fileReq, err := http.NewRequestWithContext(m.ctx, http.MethodGet, downloadURL, nil)
					if err != nil {
						logger.Warn("Не удалось создать запрос для файла %s: %v", fileURI, err)
						continue
					}

					fileResp, err := http.DefaultClient.Do(fileReq)
					if err != nil {
						logger.Warn("Не удалось скачать файл %s: %v", fileURI, err)
						continue
					}

					if fileResp.StatusCode != http.StatusOK {
						fileResp.Body.Close()
						logger.Warn("Ошибка скачивания файла %s: статус %d", fileURI, fileResp.StatusCode)
						continue
					}

					fileContent, err := io.ReadAll(fileResp.Body)
					fileResp.Body.Close()

					if err != nil {
						logger.Warn("Не удалось прочитать содержимое файла %s: %v", fileURI, err)
						continue
					}

					// Генерируем эмбеддинг через Google Embedding API
					docName := fmt.Sprintf("document_%d", idx+1)
					if fileID.Name != "" {
						docName = fileID.Name
					}

					content := string(fileContent)

					// Генерируем эмбеддинг через функцию GenerateGoogleEmbedding
					embedding, err := GenerateGoogleEmbedding(m.ctx, m.googleClient.apiKey, content)
					if err != nil {
						logger.Warn("Не удалось сгенерировать эмбеддинг для файла %s: %v", docName, err)
						continue
					}

					// Сохраняем в БД с привязкой к modelId
					docID := fmt.Sprintf("doc_%d_%d", modelId, time.Now().UnixNano())
					metadata := DocumentMetadata{
						Source:    "file_upload",
						FileName:  docName,
						FileID:    fileID.ID,
						CreatedAt: time.Now().Format(time.RFC3339),
					}

					if err := m.db.SaveEmbedding(userId, modelId, docID, docName, content, embedding, metadata); err != nil {
						logger.Warn("Не удалось сохранить эмбеддинг для файла %s: %v", docName, err)
					} else {
						logger.Info("Документ '%s' успешно добавлен в векторное хранилище БД для modelId=%d", docName, modelId)
					}
				}

				// Обновляем VectorIds - всегда пустой (эмбеддинги привязаны к modelId в БД)
				updated.VecIds.VectorId = []string{}
			} else {
				// Файлы удалены - очищаем VectorIds
				updated.VecIds.VectorId = []string{}
			}
		} else {
			// Файлы не изменились - сохраняем существующие FileIds
			updated.FileIds = existing.FileIds
			// VectorIds очищаем (эмбеддинги привязаны к modelId в БД)
			updated.VecIds.VectorId = []string{}
		}
	} else {
		// Случай 3: Search не был включён и не включается сейчас
		// Сохраняем существующие FileIds если не изменились
		if filesEqual(existing.FileIds, updated.FileIds) {
			updated.FileIds = existing.FileIds
		}
		updated.VecIds.VectorId = []string{}
	}

	// ============================================================================
	// ОБНОВЛЕНИЕ КОНФИГУРАЦИИ В БД
	// ============================================================================

	// Для Google Gemini нет нужды создавать/удалять агента в API - его нет в классическом понимании
	// Конфигурация (System Instruction, GenerationConfig, Tools) хранится локально в БД
	// и применяется при каждом запросе к Gemini API

	// Устанавливаем GptType из существующей модели если не указан
	if updated.GptType == nil {
		updated.GptType = existing.GptType
	}

	// Формируем UMCR для сохранения в БД (без вызова API)
	umcr := UMCR{
		Provider: ProviderGoogle,
		AssistID: assistId, // Сохраняем существующий assistId (название модели)
		AllIds:   nil,      // AllIds не используется для Google (конфигурация в Data)
	}

	// Сохраняем обновленные данные в БД
	if err := m.SaveModel(userId, umcr, updated); err != nil {
		return fmt.Errorf("ошибка сохранения обновленной модели в БД: %w", err)
	}

	logger.Info("Google модель успешно обновлена (без вызова API)", userId)
	return nil
}

// deleteGoogleModel удаляет модель google
func (m *UniversalModel) deleteGoogleModel(userId uint32, modelData *UserModelRecord, deleteFiles bool, progressCallback func(string)) error {
	if m.googleClient == nil {
		return fmt.Errorf("google клиент не инициализирован")
	} // по приколу

	if progressCallback != nil {
		progressCallback(fmt.Sprintf("✅ Google агент %s 'удалён' из API", modelData.AssistId)) // на самом деле не удаляется
	}

	return nil
}

// createGoogleModel создает модель Google — обёртка для парсинга JSON и делегирования клиенту
// ПРИМЕЧАНИЕ: fileIDs игнорируются для Google моделей, так как Google API не хранит файлы.
// Вместо этого документы загружаются как эмбеддинги в нашу БД через UploadDocumentWithEmbedding().
func (m *UniversalModel) createGoogleModel(userId uint32, modelData *UniversalModelData, fileIDs []Ids) (UMCR, error) {
	if m.googleClient == nil {
		return UMCR{}, fmt.Errorf("google клиент не инициализирован")
	}

	if modelData == nil {
		return UMCR{}, fmt.Errorf("modelData не может быть nil")
	}

	if modelData.Prompt == "" {
		return UMCR{}, fmt.Errorf("поле 'prompt' отсутствует или пустое")
	}

	logger.Info("Создание Google модели: name=%s (fileIDs игнорируются)", modelData.Name, userId)

	// Делегируем создание клиенту
	umcr, err := m.googleClient.createGoogleAgent(modelData, userId, fileIDs)
	if err != nil {
		return UMCR{}, err
	}

	return umcr, nil
}

// ParseGoogleSchemaJSON парсит константу GoogleSchemaJSON в map[string]interface{}
// для использования в response_schema Google Gemini API
func ParseGoogleSchemaJSON() map[string]interface{} {
	var schema map[string]interface{}
	err := json.Unmarshal([]byte(GoogleSchemaJSON), &schema)
	if err != nil {
		// Это не должно произойти, т.к. GoogleSchemaJSON - валидный JSON
		logger.Error("[ParseGoogleSchemaJSON] Ошибка парсинга GoogleSchemaJSON: %v", err)
		return map[string]interface{}{} // Возвращаем пустую схему в крайнем случае
	}
	return schema
}

// GenerateImage генерирует изображение через Google Gemini API с Imagen 3
// ВАЖНО: Google Gemini 2.0+ поддерживает встроенную генерацию изображений
// Возвращает: imageData (PNG bytes), mimeType, error
func (m *GoogleAgentClient) GenerateImage(prompt string, aspectRatio string) ([]byte, string, error) {
	if prompt == "" {
		return nil, "", fmt.Errorf("prompt не может быть пустым")
	}

	// Используем Gemini Flash для генерации изображений (встроенная поддержка Imagen 3)
	// Документация: https://ai.google.dev/gemini-api/docs/imagen
	modelName := "gemini-2.0-flash-exp"
	imageURL := fmt.Sprintf("%s/models/%s:generateContent?key=%s", m.url, modelName, m.apiKey)

	// Формируем расширенный промпт для генерации изображения
	enhancedPrompt := fmt.Sprintf("Generate a high-quality, detailed image: %s", prompt)

	if aspectRatio != "" {
		enhancedPrompt += fmt.Sprintf("\nAspect ratio: %s", aspectRatio)
	}

	enhancedPrompt += "\nStyle: photorealistic, high detail, vibrant colors, professional quality"

	// Формируем payload для Gemini API с запросом изображения
	payload := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]interface{}{
					{
						"text": enhancedPrompt,
					},
				},
			},
		},
		"generationConfig": map[string]interface{}{
			"temperature": 0.4,
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("ошибка сериализации payload: %w", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, imageURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, "", fmt.Errorf("ошибка создания POST запроса: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("ошибка HTTP запроса: %w", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("ошибка чтения ответа: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		logger.Error("GenerateImage: API вернул %d: %s", resp.StatusCode, string(responseBody))
		return nil, "", fmt.Errorf("API вернул %d: %s", resp.StatusCode, string(responseBody))
	}

	// Парсим ответ от Gemini API
	var geminiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text       string `json:"text,omitempty"`
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"` // base64
					} `json:"inlineData,omitempty"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.Unmarshal(responseBody, &geminiResp); err != nil {
		return nil, "", fmt.Errorf("ошибка парсинга ответа: %w", err)
	}

	if len(geminiResp.Candidates) == 0 {
		return nil, "", fmt.Errorf("API не вернул результатов")
	}

	// Ищем изображение в ответе
	for _, part := range geminiResp.Candidates[0].Content.Parts {
		if part.InlineData.Data != "" && strings.HasPrefix(part.InlineData.MimeType, "image/") {
			// Декодируем base64
			imageData, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if err != nil {
				return nil, "", fmt.Errorf("ошибка декодирования base64: %w", err)
			}

			logger.Info("GenerateImage: успешно сгенерировано изображение (%d байт, %s)", len(imageData), part.InlineData.MimeType)
			return imageData, part.InlineData.MimeType, nil
		}
	}

	// Если изображение не найдено, возвращаем ошибку
	logger.Warn("GenerateImage: модель %s не вернула изображение в ответе. Response: %s", modelName, string(responseBody))
	return nil, "", fmt.Errorf("модель не сгенерировала изображение. Возможно, нужно использовать другой промпт или модель не поддерживает генерацию изображений")
}
