package create

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/mode"
)

// GoogleSchemaJSON - JSON Schema для структурированных ответов Gemini Agent
// Используется в response_schema для форсирования JSON формата ответов
const GoogleSchemaJSON = `{
	"type": "object",
	"properties": {
		"message": {
			"type": "string",
			"description": "Text message for user. Leave empty (\"\") if sending files with caption!"
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
								"description": "File type"
							},
							"url": {
								"type": "string",
								"description": "File URL"
							},
							"file_name": {
								"type": "string",
								"description": "File name"
							},
							"caption": {
								"type": "string",
								"description": "File caption - use this field to message user when sending files"
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
			"description": "Is dialog goal achieved"
		},
		"operator": {
			"type": "boolean",
			"description": "Is operator connection required"
		}
	},
	"required": ["action", "target", "operator"]
}`

// GoogleAgentClient клиент для работы с Google Gemini API
type GoogleAgentClient struct {
	apiKey         string
	url            string
	ctx            context.Context
	universalModel *UniversalModel // Ссылка на universalModel для доступа к GetRealUserID
	promptFetcher  GooglePromptHintFetcher
	toolsFetcher   GoogleFunctionDeclarationsFetcher
}

// GooglePromptHintFetcher опционально получает prompt hint от внешнего MCP-источника.
type GooglePromptHintFetcher func(ctx context.Context, userId uint32, provider ProviderType) (string, error)

// GoogleFunctionDeclarationsFetcher опционально получает function declarations от внешнего MCP-источника.
type GoogleFunctionDeclarationsFetcher func(ctx context.Context, userId uint32, provider ProviderType) ([]FunctionDeclaration, error)

// ============================================================================
// TYPED STRUCTURES FOR FUNCTION DECLARATIONS
// ============================================================================

// PropertySchema описывает один параметр в JSON Schema
type PropertySchema struct {
	Type        string          `json:"type"`
	Description string          `json:"description"`
	Enum        []string        `json:"enum,omitempty"`
	Items       *PropertySchema `json:"items,omitempty"`
}

// FunctionParameters описывает параметры функции
type FunctionParameters struct {
	Type       string                    `json:"type"`
	Properties map[string]PropertySchema `json:"properties"`
	Required   []string                  `json:"required"`
}

// FunctionDeclaration описывает одну функцию для Function Calling
type FunctionDeclaration struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// GoogleTool описывает инструмент (tool) для Google Gemini API
type GoogleTool struct {
	FunctionDeclarations []FunctionDeclaration `json:"function_declarations,omitempty"`
	GoogleSearch         *struct{}             `json:"google_search,omitempty"`
}

// NewGoogleAgentClient создаёт новый экземпляр GoogleAgentClient с API ключом
func NewGoogleAgentClient(ctx context.Context, apiKey string) *GoogleAgentClient {
	return &GoogleAgentClient{
		apiKey: apiKey,
		url:    mode.GoogleAgentsURL,
		ctx:    ctx,
	}
}

// SetMCPConfigFetchers устанавливает внешние fetchers для prompt hint и function declarations.
// Используется как первый шаг миграции Google на MCP без import cycle между create и model.
func (m *GoogleAgentClient) SetMCPConfigFetchers(promptFetcher GooglePromptHintFetcher, toolsFetcher GoogleFunctionDeclarationsFetcher) {
	m.promptFetcher = promptFetcher
	m.toolsFetcher = toolsFetcher
}

// SetUniversalModel устанавливает UniversalModel для доступа к GetRealUserID в create-time операциях.
func (m *GoogleAgentClient) SetUniversalModel(um *UniversalModel) {
	m.universalModel = um
}

// ============================================================================
// UTILITY FUNCTIONS FOR HTTP REQUESTS
// ============================================================================

// executeGoogleAPIRequest выполняет POST запрос к Google API с валидацией
func executeGoogleAPIRequest(ctx context.Context, url string, payload interface{}) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ошибка сериализации payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("ошибка создания запроса: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

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
		return nil, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return responseBody, nil
}

// executeGoogleAPIGetRequest выполняет GET запрос к Google API
func executeGoogleAPIGetRequest(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания запроса: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка HTTP запроса: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения ответа: %w", err)
	}

	return responseBody, nil
}

// executeGoogleAPIDeleteRequest выполняет DELETE запрос к Google API
// Допускает статусы OK, NoContent и NotFound как успешные
func executeGoogleAPIDeleteRequest(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("ошибка создания DELETE запроса: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка HTTP запроса: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return nil
}

// createGoogleAgent создает нового Gemini агента с указанными параметрами
func (m *GoogleAgentClient) createGoogleAgent(modelData *UniversalModelData, userId uint32, _ []Ids) (UMCR, error) {
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

	// Сначала пробуем получить hint от MCP; при ошибке используем локальный legacy builder.
	enhancedPrompt := modelData.Prompt
	mcpPromptApplied := false
	if m.promptFetcher != nil {
		if hint, fetchErr := m.promptFetcher(m.ctx, userId, ProviderGoogle); fetchErr == nil && hint != "" {
			enhancedPrompt = modelData.Prompt + "\n\n" + hint
			mcpPromptApplied = true
		}
	}

	if !mcpPromptApplied {
		enhancedPrompt += "\n\n"

		// Time reminder for ALL models
		enhancedPrompt += fmt.Sprintf("CURRENT TIME:\n"+
			"IMPORTANT: Use get_current_time(user_id=\"%d\") to get actual date/time\n"+
			"DO NOT use your internal knowledge about dates - it is OUTDATED!\n\n", realUserId)

		// Important reminder - only for active functions
		if modelData.MetaAction != "" || modelData.Operator {
			enhancedPrompt += "IMPORTANT REMINDER:\n" +
				"In EVERY response you MUST:\n"

			if modelData.MetaAction != "" {
				enhancedPrompt += "1. Check GOAL condition (from your instructions above) and set target correctly\n"
			}

			if modelData.Operator {
				enhancedPrompt += "2. Check if operator needed (from your instructions above) and set operator correctly\n"
			}

			enhancedPrompt += "3. DO NOT IGNORE these checks!\n\n"
		}

		// File handling instructions (optimized version)
		if modelData.S3 {
			enhancedPrompt += "S3: get_s3_files, create_file\n" +
				"Types: .jpg/.png=photo, .mp4=video, .mp3=audio, other=doc\n" +
				"When sending files: use caption (NOT message)\n\n"
		}

		// Code Interpreter instructions only if enabled
		if modelData.Interpreter {
			enhancedPrompt += "CODE INTERPRETER:\n" +
				"You can execute code for:\n" +
				"- Data analysis and calculations\n" +
				"- Creating charts and visualizations\n" +
				"- Processing files (CSV, Excel, JSON, etc.)\n" +
				"Use code execution when necessary\n\n"
		}

		// Video generation instructions only if enabled
		if modelData.Video {
			enhancedPrompt += "VIDEO GENERATION:\n" +
				"When user asks to create/generate/draw video:\n" +
				"1. Describe in your text response what you are creating\n" +
				"2. System will AUTOMATICALLY generate and send video to user\n" +
				"3. You can specify: duration (4-8 sec), aspect ratio (16:9, 9:16, 1:1)\n" +
				"4. DO NOT add video files to send_files - they will be added automatically!\n" +
				"5. Just reply to user that you are creating video with description\n\n"
		}

		// Web search instructions
		if modelData.WebSearch {
			enhancedPrompt += "WEB SEARCH (Google VSearch):\n" +
				"You have access to current internet information via Google VSearch tool.\n" +
				"MANDATORY use google_search when:\n" +
				"   - User asks about current events, weather, news, currency rates\n" +
				"   - Requests information not in your knowledge base (data after October 2023)\n" +
				"   - Asks for current facts about companies, people, places\n" +
				"   - Says \"what's on the internet\", \"find information\", \"google it\"\n" +
				"1. ALWAYS use search for queries with dates, time, current statistics\n" +
				"2. After getting search results - summarize them clearly\n" +
				"3. Cite information sources when appropriate\n" +
				"4. If you are NOT SURE about information - USE SEARCH instead of refusing!\n\n"
		}

		// Google Calendar instructions (optimized version)
		if modelData.GOAuth.HasCalendar() {
			enhancedPrompt += fmt.Sprintf("CALENDAR: user_id=\"%d\"\n"+
				"Functions: calendar_create_event, calendar_list_events, calendar_delete_event\n"+
				"RFC3339: \"2026-02-05T15:00:00+03:00\"\n"+
				"ALWAYS call get_current_time BEFORE calculating dates!\n"+
				"After operation - confirm action with details\n\n"+
				"DELETING EVENTS:\n"+
				"DO NOT create events when deleting! Algorithm:\n"+
				"1. calendar_list_events -> get event_id\n"+
				"2. calendar_delete_event(user_id, event_id) for each\n\n",
				realUserId)
		}

		// Google Sheets instructions (optimized version)
		if modelData.GOAuth.HasSheets() {
			enhancedPrompt += fmt.Sprintf("SHEETS: user_id=\"%d\"\n"+
				"spreadsheet_id from prompt (FULL ID ~40 chars, NOT name!)\n"+
				"Functions: sheets_read_range (read), sheets_write_range (write), sheets_append_range (append)\n"+
				"After function call - process result and show data to user\n\n", realUserId)
		}

		// Image generation instructions
		if modelData.Image {
			enhancedPrompt += "IMAGE GENERATION:\n" +
				"When user asks to create/draw/generate image:\n" +
				"1. Describe in detail in your response (in message field) what you are creating.\n" +
				"2. System will AUTOMATICALLY generate image based on your description and add it to send_files.\n" +
				"3. IMPORTANT: DO NOT add images to send_files yourself! Leave send_files empty [].\n" +
				"4. DO NOT invent fake URLs (example.com etc.) - system will add real URL after generation.\n" +
				"5. Just describe what you are creating in message field, and system will do the rest.\n\n"
		}

		// JSON response field rules
		enhancedPrompt += "RULES for JSON response fields:\n\n"

		// Target field instructions
		if modelData.MetaAction != "" {
			enhancedPrompt += "**target** (boolean) - Is dialog GOAL achieved:\n" +
				"  Check goal condition from YOUR INSTRUCTIONS ABOVE\n" +
				"  If condition EXACTLY met -> target: true\n" +
				"  If condition NOT met -> target: false\n\n"
		} else {
			enhancedPrompt += "**target**: ALWAYS false (no goal)\n\n"
		}

		// Operator field instructions
		if modelData.Operator {
			enhancedPrompt += "**operator** (boolean) - Is operator required:\n" +
				"  Check operator call condition from YOUR INSTRUCTIONS ABOVE\n" +
				"  If user requests operator -> operator: true\n" +
				"  In all other cases -> operator: false\n\n"
		} else {
			enhancedPrompt += "**operator**: ALWAYS false (operator call disabled)\n\n"
		}

		// Final instruction on response format
		enhancedPrompt += "IMPORTANT: Your response MUST be valid JSON in the following format:\n" +
			GoogleSchemaJSON + "\n\n" +
			"Always return response strictly in this JSON format."
	}

	// Build payload for agent creation
	// Google Gemini API uses system_instruction for prompt
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
			"response_schema":    ParseModelSchemaJSON(false), // false = БЕЗ additionalProperties для Google
		}
	}

	// ============================================================================
	// ФОРМИРОВАНИЕ TOOLS
	// ============================================================================
	// Инициализируем слайс инструментов
	var googleTools []GoogleTool

	// 1. Добавляем Веб-поиск (Google VSearch)
	if modelData.WebSearch {
		googleTools = append(googleTools, GoogleTool{
			GoogleSearch: &struct{}{},
		})
	}

	// 2. Добавляем Function Calling инструменты: сначала MCP, при ошибке - локальный fallback.
	var allFunctions []FunctionDeclaration
	toolsFromMCP := false
	if m.toolsFetcher != nil {
		if fetched, fetchErr := m.toolsFetcher(m.ctx, userId, ProviderGoogle); fetchErr == nil {
			allFunctions = fetched
			toolsFromMCP = true
		}
	}

	if !toolsFromMCP && modelData.S3 {
		allFunctions = append(allFunctions,
			FunctionDeclaration{
				Name:        "get_s3_files",
				Description: fmt.Sprintf("Gets user's files list from S3. IMPORTANT: user_id must be STRING \"%d\"", realUserId),
				Parameters: FunctionParameters{
					Type: "object",
					Properties: map[string]PropertySchema{
						"user_id": {
							Type:        "string",
							Description: fmt.Sprintf("User ID (STRING): \"%d\"", realUserId),
						},
					},
					Required: []string{"user_id"},
				},
			},
			FunctionDeclaration{
				Name:        "create_file",
				Description: "Creates new file in user's S3 storage",
				Parameters: FunctionParameters{
					Type: "object",
					Properties: map[string]PropertySchema{
						"user_id": {
							Type:        "string",
							Description: fmt.Sprintf("User ID (STRING): \"%d\"", realUserId),
						},
						"content": {
							Type:        "string",
							Description: "File content",
						},
						"file_name": {
							Type:        "string",
							Description: "File name with extension",
						},
					},
					Required: []string{"user_id", "content", "file_name"},
				},
			},
		)
	}

	// 3. Добавляем Google Calendar (Function Calling)
	if !toolsFromMCP && modelData.GOAuth.HasCalendar() {
		allFunctions = append(allFunctions,
			FunctionDeclaration{
				Name:        "calendar_create_event",
				Description: "Creates new event in user's Google Calendar",
				Parameters: FunctionParameters{
					Type: "object",
					Properties: map[string]PropertySchema{
						"user_id": {
							Type:        "string",
							Description: fmt.Sprintf("User ID (STRING): \"%d\"", realUserId),
						},
						"title": {
							Type:        "string",
							Description: "Event title",
						},
						"description": {
							Type:        "string",
							Description: "Event description (optional)",
						},
						"start_time": {
							Type:        "string",
							Description: "Start time in RFC3339 format (e.g.: '2026-02-04T10:00:00Z')",
						},
						"end_time": {
							Type:        "string",
							Description: "End time in RFC3339 format",
						},
						"location": {
							Type:        "string",
							Description: "Event location (optional)",
						},
						"attendees": {
							Type:        "array",
							Description: "Email addresses of attendees (optional)",
							Items: &PropertySchema{
								Type: "string",
							},
						},
					},
					Required: []string{"user_id", "title", "start_time", "end_time"},
				},
			},
			FunctionDeclaration{
				Name:        "calendar_list_events",
				Description: "Gets events list from user's Google Calendar",
				Parameters: FunctionParameters{
					Type: "object",
					Properties: map[string]PropertySchema{
						"user_id": {
							Type:        "string",
							Description: fmt.Sprintf("User ID (STRING): \"%d\"", realUserId),
						},
						"time_min": {
							Type:        "string",
							Description: "Period start in RFC3339 (optional, default - current time)",
						},
						"time_max": {
							Type:        "string",
							Description: "Period end in RFC3339 (optional)",
						},
						"max_results": {
							Type:        "integer",
							Description: "Maximum number of events (default 10)",
						},
					},
					Required: []string{"user_id"},
				},
			},
			FunctionDeclaration{
				Name:        "calendar_delete_event",
				Description: "Deletes event from Google Calendar",
				Parameters: FunctionParameters{
					Type: "object",
					Properties: map[string]PropertySchema{
						"user_id": {
							Type:        "string",
							Description: fmt.Sprintf("User ID (STRING): \"%d\"", realUserId),
						},
						"event_id": {
							Type:        "string",
							Description: "Event ID to delete",
						},
					},
					Required: []string{"user_id", "event_id"},
				},
			},
		)
	}

	// 4. Добавляем Google Sheets (Function Calling)
	if !toolsFromMCP && modelData.GOAuth.HasSheets() {
		allFunctions = append(allFunctions,
			FunctionDeclaration{
				Name:        "sheets_read_range",
				Description: "Reads data from specified range in Google Sheets",
				Parameters: FunctionParameters{
					Type: "object",
					Properties: map[string]PropertySchema{
						"user_id": {
							Type:        "string",
							Description: fmt.Sprintf("User ID (STRING): \"%d\"", realUserId),
						},
						"spreadsheet_id": {
							Type:        "string",
							Description: "Google Sheets spreadsheet ID (from URL)",
						},
						"range": {
							Type:        "string",
							Description: "Range to read (e.g.: 'Sheet1!A1:D10')",
						},
					},
					Required: []string{"user_id", "spreadsheet_id", "range"},
				},
			},
			FunctionDeclaration{
				Name:        "sheets_write_range",
				Description: "Writes data to specified range in Google Sheets",
				Parameters: FunctionParameters{
					Type: "object",
					Properties: map[string]PropertySchema{
						"user_id": {
							Type:        "string",
							Description: fmt.Sprintf("User ID (STRING): \"%d\"", realUserId),
						},
						"spreadsheet_id": {
							Type:        "string",
							Description: "Google Sheets spreadsheet ID",
						},
						"range": {
							Type:        "string",
							Description: "Starting cell for writing (e.g.: 'Sheet1!A1')",
						},
						"values": {
							Type:        "array",
							Description: "2D array of values to write",
							Items: &PropertySchema{
								Type: "array",
								Items: &PropertySchema{
									Type: "string",
								},
							},
						},
					},
					Required: []string{"user_id", "spreadsheet_id", "range", "values"},
				},
			},
			FunctionDeclaration{
				Name:        "sheets_append_range",
				Description: "Appends data to the end of Google Sheets table",
				Parameters: FunctionParameters{
					Type: "object",
					Properties: map[string]PropertySchema{
						"user_id": {
							Type:        "string",
							Description: fmt.Sprintf("User ID (STRING): \"%d\"", realUserId),
						},
						"spreadsheet_id": {
							Type:        "string",
							Description: "Google Sheets spreadsheet ID",
						},
						"range": {
							Type:        "string",
							Description: "Column range for appending (e.g.: 'Sheet1!A:D')",
						},
						"values": {
							Type:        "array",
							Description: "2D array of values to append",
							Items: &PropertySchema{
								Type: "array",
								Items: &PropertySchema{
									Type: "string",
								},
							},
						},
					},
					Required: []string{"user_id", "spreadsheet_id", "range", "values"},
				},
			},
			FunctionDeclaration{
				Name:        "sheets_create_spreadsheet",
				Description: "Creates new Google Sheets spreadsheet",
				Parameters: FunctionParameters{
					Type: "object",
					Properties: map[string]PropertySchema{
						"user_id": {
							Type:        "string",
							Description: fmt.Sprintf("User ID (STRING): \"%d\"", realUserId),
						},
						"title": {
							Type:        "string",
							Description: "New spreadsheet title",
						},
						"sheet_names": {
							Type:        "array",
							Description: "Sheet names (optional)",
							Items: &PropertySchema{
								Type: "string",
							},
						},
					},
					Required: []string{"user_id", "title"},
				},
			},
		)
	}

	// Добавляем все function_declarations в один tool если есть функции
	if len(allFunctions) > 0 {
		googleTools = append(googleTools, GoogleTool{
			FunctionDeclarations: allFunctions,
		})
	}

	// 5. Добавляем Code Interpreter (только если нет function_declarations)
	// ВАЖНО: Google Gemini НЕ поддерживает одновременное использование
	// function_declarations и code_execution в одном запросе
	hasAnyFunctionDeclarations := len(allFunctions) > 0
	if modelData.Interpreter && !hasAnyFunctionDeclarations {
		// Code execution требует специального формата (не используем GoogleTool структуру)
		googleTools = append(googleTools, GoogleTool{})
	}

	// Конвертируем googleTools в формат для JSON API
	var toolsForPayload []interface{}
	for _, tool := range googleTools {
		if tool.GoogleSearch != nil {
			toolsForPayload = append(toolsForPayload, map[string]interface{}{
				"google_search": map[string]interface{}{},
			})
		} else if len(tool.FunctionDeclarations) > 0 {
			toolsForPayload = append(toolsForPayload, map[string]interface{}{
				"function_declarations": tool.FunctionDeclarations,
			})
		}
	}

	// Добавляем code_execution отдельно если нужно
	if modelData.Interpreter && !hasAnyFunctionDeclarations {
		toolsForPayload = append(toolsForPayload, map[string]interface{}{
			"code_execution": map[string]interface{}{},
		})
	}

	// Присваиваем собранные инструменты в payload
	if len(toolsForPayload) > 0 {
		payload["tools"] = toolsForPayload
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

	responseBody, err := executeGoogleAPIRequest(m.ctx, testURL, testPayload)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка API запроса: %v", err)
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
		defer func() { _ = resp.Body.Close() }()

		responseBody, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
		}
	}

	return nil
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
	//var GoogleVideoModel string
	//for _, model := range models {
	//	modelName := strings.TrimPrefix(Name, "models/")
	//	// Проверяем модели с поддержкой мультимодальности
	//	if strings.Contains(modelName, "gemini-2") || strings.Contains(modelName, "gemini-1.5-pro") {
	//		GoogleVideoModel = modelName
	//		break
	//	}
	//}
	//
	//if GoogleVideoModel == "" {
	//	return nil, "", fmt.Errorf("не найдена модель с поддержкой генерации видео")
	//}

	//logger.Debug("Используется модель для генерации видео: %s", GoogleVideoModel)

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

	// URL для генерации
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", m.url, GoogleVideoModel, m.apiKey)

	responseBody, err := executeGoogleAPIRequest(m.ctx, url, payload)
	if err != nil {
		return nil, "", fmt.Errorf("ошибка при вызове API: %w", err)
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
			//logger.Debug("Видео успешно сгенерировано (inline_data), размер: %d bytes, mime: %s",
			//	len(videoData), part.InlineData.MimeType)
			return videoData, part.InlineData.MimeType, nil
		}

		// Проверяем file_data (URI)
		if part.FileData.FileURI != "" && strings.HasPrefix(part.FileData.MimeType, "video/") {
			videoData, err := m.DownloadVideoFromURI(part.FileData.FileURI)
			if err != nil {
				return nil, "", fmt.Errorf("ошибка скачивания видео: %v", err)
			}
			//logger.Debug("Видео успешно сгенерировано (file_uri), размер: %d bytes, mime: %s",
			//	len(videoData), part.FileData.MimeType)
			return videoData, part.FileData.MimeType, nil
		}
	}

	// Если видео не найдено, возвращаем информативное сообщение
	//logger.Warn("Видео не найдено в ответе модели %s. Возможно модель не поддерживает генерацию видео или требуется другой промпт.", GoogleVideoModel)
	return nil, "", fmt.Errorf("модель %s не сгенерировала видео. Попробуйте более подробное описание или используйте другую модель", GoogleVideoModel)
}

// DownloadVideoFromURI скачивает видео по URI из Google File API
func (m *GoogleAgentClient) DownloadVideoFromURI(fileURI string) ([]byte, error) {
	if fileURI == "" {
		return nil, fmt.Errorf("пустой URI файла")
	}

	// Добавляем API ключ к запросу
	downloadURL := fmt.Sprintf("%s?key=%s", fileURI, m.apiKey)

	videoData, err := executeGoogleAPIGetRequest(m.ctx, downloadURL)
	if err != nil {
		return nil, fmt.Errorf("ошибка скачивания видео: %w", err)
	}

	//logger.Debug("Видео успешно скачано с URI, размер: %d bytes", len(videoData))

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

// parseAudioTranscriptionResponse парсит ответ транскрибации и возвращает текст
func parseAudioTranscriptionResponse(responseBody []byte) (string, error) {
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

	return transcription, nil
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

	// Отправляем запрос
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", m.url, GoogleAudioModel, m.apiKey)

	responseBody, err := executeGoogleAPIRequest(m.ctx, url, payload)
	if err != nil {
		return "", fmt.Errorf("ошибка при вызове API: %w", err)
	}

	// Парсим ответ
	transcription, err := parseAudioTranscriptionResponse(responseBody)
	if err != nil {
		return "", err
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

	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", m.url, audioModel, m.apiKey)

	responseBody, err := executeGoogleAPIRequest(m.ctx, url, payload)
	if err != nil {
		return "", fmt.Errorf("ошибка при вызове API: %w", err)
	}

	transcription, err := parseAudioTranscriptionResponse(responseBody)
	if err != nil {
		return "", err
	}

	//logger.Debug("Успешная транскрибация файла, длина текста: %d символов", len(transcription))

	return transcription, nil
}

// DeleteAudioFile удаляет загруженный аудио файл из Google File API
func (m *GoogleAgentClient) DeleteAudioFile(fileName string) error {
	if fileName == "" {
		return fmt.Errorf("пустое имя файла")
	}

	deleteURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/%s?key=%s", fileName, m.apiKey)

	if err := executeGoogleAPIDeleteRequest(m.ctx, deleteURL); err != nil {
		return fmt.Errorf("ошибка при вызове API: %w", err)
	}

	//logger.Debug("Аудио файл %s успешно удалён", fileName)

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
	if text == "" {
		return nil, fmt.Errorf("текст не может быть пустым")
	}

	// Используем правильную модель gemini-embedding-001 для генерации эмбеддингов
	// Документация: https://ai.google.dev/gemini-api/docs/embeddings
	embedURL := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-embedding-001:embedContent?key=%s", apiKey)

	payload := map[string]interface{}{
		"content": map[string]interface{}{
			"parts": []map[string]interface{}{
				{"text": text},
			},
		},
	}

	responseBody, err := executeGoogleAPIRequest(ctx, embedURL, payload)
	if err != nil {
		return nil, fmt.Errorf("ошибка при вызове API: %w", err)
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

	// Случай 1: Флаг VSearch отключён (VSearch: true → false)
	// Действие: Удалить ВСЕ эмбеддинги этой модели из БД
	if !updated.Search && existing.Search {
		if err := m.db.DeleteAllModelEmbeddings(modelId); err != nil {
			//logger.Warn("Не удалось удалить эмбеддинги для modelId=%d: %v", modelId, err)
		}

		// Очищаем VectorIds (они всегда пустые для Google)
		updated.VecIds.VectorId = []string{}
		updated.VecIds.FileIds = []Ids{}
	} else if updated.Search {
		// Случай 2: VSearch включён - управляем эмбеддингами

		// Проверяем, изменились ли файлы
		filesChanged := !slices.EqualFunc(existing.FileIds, updated.FileIds, func(a, b Ids) bool {
			return a.ID == b.ID && a.Name == b.Name
		})

		if filesChanged {
			//logger.Debug("Файлы изменились для modelId=%d, обновляем векторное хранилище в БД", modelId)

			// 2.1. Удаляем все старые эмбеддинги модели
			if len(existing.FileIds) > 0 {
				if err := m.db.DeleteAllModelEmbeddings(modelId); err != nil {
					//logger.Warn("Не удалось удалить эмбеддинги для modelId=%d: %v", modelId, err)
				}
			}

			// 2.2. Добавляем новые файлы как эмбеддинги в БД
			if len(updated.FileIds) > 0 {
				//logger.Debug("Добавляем %d новых файлов как эмбеддинги в БД для modelId=%d", len(updated.FileIds), modelId)

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
						//logger.Warn("Не удалось создать запрос для файла %s: %v", fileURI, err)
						continue
					}

					fileResp, err := http.DefaultClient.Do(fileReq)
					if err != nil {
						//logger.Warn("Не удалось скачать файл %s: %v", fileURI, err)
						continue
					}

					if fileResp.StatusCode != http.StatusOK {
						fileErr := fileResp.Body.Close()
						if fileErr != nil {
							return fileErr
						}
						//logger.Warn("Ошибка скачивания файла %s: статус %d", fileURI, fileResp.StatusCode)
						continue
					}

					fileContent, err := io.ReadAll(fileResp.Body)
					fileErr := fileResp.Body.Close()
					if fileErr != nil {
						return fileErr
					}

					if err != nil {
						//logger.Warn("Не удалось прочитать содержимое файла %s: %v", fileURI, err)
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
						//logger.Warn("Не удалось сгенерировать эмбеддинг для файла %s: %v", docName, err)
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

					if err := m.db.SaveEmbedding(userId, modelId, ProviderGoogle, docID, docName, content, embedding, metadata); err != nil {
						//	logger.Warn("Не удалось сохранить эмбеддинг для файла %s: %v", docName, err)
						//} else {
						//	logger.Debug("Документ '%s' успешно добавлен в векторное хранилище БД для modelId=%d", docName, modelId)
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
		// Случай 3: VSearch не был включён и не включается сейчас
		// Сохраняем существующие FileIds если не изменились
		if slices.EqualFunc(existing.FileIds, updated.FileIds, func(a, b Ids) bool {
			return a.ID == b.ID && a.Name == b.Name
		}) {
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

	return nil
}

// deleteGoogleModel удаляет модель google
func (m *UniversalModel) deleteGoogleModel(_ uint32, modelData *UserModelRecord, _ bool, progressCallback func(string)) error {
	if m.googleClient == nil {
		return fmt.Errorf("google client not initialized")
	}

	if progressCallback != nil {
		progressCallback(fmt.Sprintf("Google agent %s 'deleted' from API", modelData.AssistId)) // actually not deleted
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

	//logger.Debug("Создание Google модели: name=%s (fileIDs игнорируются)", modelData.Name, userId)

	// Делегируем создание клиенту
	umcr, err := m.googleClient.createGoogleAgent(modelData, userId, fileIDs)
	if err != nil {
		return UMCR{}, err
	}

	return umcr, nil
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

	responseBody, err := executeGoogleAPIRequest(m.ctx, imageURL, payload)
	if err != nil {
		return nil, "", fmt.Errorf("ошибка при вызове API: %w", err)
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

			//logger.Debug("GenerateImage: успешно сгенерировано изображение (%d байт, %s)", len(imageData), part.InlineData.MimeType)
			return imageData, part.InlineData.MimeType, nil
		}
	}

	// Если изображение не найдено, возвращаем ошибку
	return nil, "", fmt.Errorf("модель не сгенерировала изображение. Возможно, нужно использовать другой промпт или модель не поддерживает генерацию изображений")
}
