package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// UniversalActionHandler универсальный обработчик функций для всех провайдеров
type UniversalActionHandler struct {
	port string // Порт для внутренних HTTP запросов (MCP сервер)
	db   comdb.Exterior
	ctx  context.Context
}

// NewUniversalActionHandler создаёт новый action handler с доступом к БД
func NewUniversalActionHandler(ctx context.Context, db comdb.Exterior, cfg *conf.Conf) *UniversalActionHandler {
	return &UniversalActionHandler{
		db:   db,
		ctx:  ctx,
		port: cfg.WEB.Land,
	}
}

// callMCP отправляет единый JSON-RPC запрос к MCP серверу (POST /mcp).
// userId и provider передаются через заголовок X-Session-ID — инструменты не получают user_id в аргументах.
func (h *UniversalActionHandler) callMCP(ctx context.Context, toolName, arguments string, provider create.ProviderType, userId uint32) string {
	// Парсим строку аргументов в map
	var args map[string]interface{}
	if arguments != "" && arguments != "{}" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			result, _ := json.Marshal(map[string]string{"error": "invalid arguments: " + err.Error()})
			return string(result)
		}
	}
	if args == nil {
		args = map[string]interface{}{}
	}

	// Строим JSON-RPC запрос
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      toolName,
			"arguments": args,
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": "failed to marshal MCP request"})
		return string(result)
	}

	var url string
	if mode.ProductionMode {
		url = fmt.Sprintf("http://localhost:%s/mcp", h.port)
	} else {
		url = fmt.Sprintf("https://localhost:%s/mcp", h.port)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(bodyBytes))
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": "failed to create MCP request"})
		return string(result)
	}
	req.Header.Set("Content-Type", "application/json")
	// Идентификация пользователя и провайдера — реальный userId без кодирования
	req.Header.Set("X-Session-ID", fmt.Sprintf("%d:%d", userId, provider))

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			result, _ := json.Marshal(map[string]string{"error": "запрос отменён по таймауту"})
			return string(result)
		}
		result, _ := json.Marshal(map[string]string{"error": "MCP request failed: " + err.Error()})
		return string(result)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": "failed to read MCP response"})
		return string(result)
	}

	// Парсим JSON-RPC ответ
	var rpcResp struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &rpcResp); err != nil {
		result, _ := json.Marshal(map[string]string{"error": "failed to parse MCP response"})
		return string(result)
	}

	if rpcResp.Error != nil {
		result, _ := json.Marshal(map[string]string{
			"error": fmt.Sprintf("MCP error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message),
		})
		return string(result)
	}

	if rpcResp.Result == nil || len(rpcResp.Result.Content) == 0 {
		return "{}"
	}

	return rpcResp.Result.Content[0].Text
}

// callMCPMethod отправляет произвольный JSON-RPC запрос к MCP серверу.
// Используется как внутренний транспорт для FetchToolsList и FetchSystemPrompt.
func (h *UniversalActionHandler) callMCPMethod(ctx context.Context, method string, params map[string]interface{}, provider create.ProviderType, userId uint32) ([]byte, error) {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  method,
		"params":  params,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal MCP request: %w", err)
	}

	var url string
	if mode.ProductionMode {
		url = fmt.Sprintf("http://localhost:%s/mcp", h.port)
	} else {
		url = fmt.Sprintf("https://localhost:%s/mcp", h.port)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create MCP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", fmt.Sprintf("%d:%d", userId, provider))

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("MCP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read MCP response: %w", err)
	}
	return body, nil
}

// FetchToolsList реализует MCPConfigProvider: вызывает MCP tools/list и возвращает
// function-инструменты для данного пользователя (без user_id в inputSchema).
// Нативные OpenAI инструменты (code_interpreter, web_search) не включаются.
func (h *UniversalActionHandler) FetchToolsList(ctx context.Context, userId uint32, provider create.ProviderType) ([]MCPToolDefinition, error) {
	body, err := h.callMCPMethod(ctx, "tools/list", map[string]interface{}{}, provider, userId)
	if err != nil {
		return nil, err
	}

	var rpcResp struct {
		Result *struct {
			Tools []struct {
				Name        string      `json:"name"`
				Description string      `json:"description"`
				InputSchema interface{} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("failed to parse tools/list response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if rpcResp.Result == nil {
		return nil, fmt.Errorf("empty tools/list result")
	}

	tools := make([]MCPToolDefinition, 0, len(rpcResp.Result.Tools))
	for _, t := range rpcResp.Result.Tools {
		tools = append(tools, MCPToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return tools, nil
}

// FetchSystemPrompt реализует MCPConfigProvider: вызывает MCP prompts/get с name=system
// и возвращает prompt hint для данного пользователя.
// Вызывающий код сам добавляет modelData.Prompt перед ним.
func (h *UniversalActionHandler) FetchSystemPrompt(ctx context.Context, userId uint32, provider create.ProviderType) (string, error) {
	body, err := h.callMCPMethod(ctx, "prompts/get", map[string]interface{}{"name": "system"}, provider, userId)
	if err != nil {
		return "", err
	}

	var rpcResp struct {
		Result *struct {
			Messages []struct {
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return "", fmt.Errorf("failed to parse prompts/get response: %w", err)
	}
	if rpcResp.Error != nil {
		return "", fmt.Errorf("MCP error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if rpcResp.Result == nil || len(rpcResp.Result.Messages) == 0 {
		return "", fmt.Errorf("empty prompts/get result")
	}
	return rpcResp.Result.Messages[0].Content.Text, nil
}

// MCPToolDefinition — тип определён в model_router.go того же пакета.

func (h *UniversalActionHandler) RunAction(ctx context.Context, functionName, arguments string, provider create.ProviderType, userId uint32) string {
	switch functionName {

	case "lead_target":
		// lead_target — вызов внешнего Meta-сервиса, НЕ через MCP
		var params struct {
			RespId int64 `json:"resp_id"`
		}
		if err := json.Unmarshal([]byte(arguments), &params); err != nil {
			return `{"error": "неверные параметры для lead_target"}`
		}

		url := fmt.Sprintf("http://localhost:8091/service/lead/target?rid=%d", params.RespId)
		req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
		if err != nil {
			result, _ := json.Marshal(map[string]interface{}{"target": true, "error": "failed to create request"})
			return string(result)
		}

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			result, _ := json.Marshal(map[string]interface{}{"target": true, "error": "failed to execute request"})
			return string(result)
		}
		defer resp.Body.Close()

		result, _ := json.Marshal(map[string]bool{"target": true})
		return string(result)

	default:
		// Все остальные инструменты — через MCP сервер
		return h.callMCP(ctx, functionName, arguments, provider, userId)
	}
}

// GetTools возвращает инструменты в формате нужного провайдера.
// Список инструментов статический; user_id не включается — MCP берёт его из X-Session-ID.
func (h *UniversalActionHandler) GetTools(provider create.ProviderType) interface{} {
	functions := []map[string]interface{}{
		{
			"name":        "lead_target",
			"description": "Выполняется, когда цель диалога достигнута",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"target": map[string]interface{}{
						"type":        "boolean",
						"description": "true - цель достигнута",
					},
				},
				"required": []string{"target"},
			},
		},
		{
			"name":        "get_s3_files",
			"description": "Получает список файлов пользователя из S3 хранилища",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
				"required":   []string{},
			},
		},
		{
			"name":        "create_file",
			"description": "Создает и сохраняет файл в S3 хранилище пользователя",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"content":   map[string]interface{}{"type": "string", "description": "Содержимое файла"},
					"file_name": map[string]interface{}{"type": "string", "description": "Имя файла для сохранения"},
				},
				"required": []string{"content", "file_name"},
			},
		},
		{
			"name":        "get_current_time",
			"description": "Возвращает текущую дату и время в часовом поясе пользователя",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
				"required":   []string{},
			},
		},
	}

	calendarFunctions := []map[string]interface{}{
		{
			"name":        "calendar_create_event",
			"description": "Создает событие в Google Calendar пользователя",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"title":       map[string]interface{}{"type": "string", "description": "Название события"},
					"description": map[string]interface{}{"type": "string", "description": "Описание события"},
					"start_time":  map[string]interface{}{"type": "string", "description": "Время начала RFC3339 (например: 2026-02-05T15:00:00+03:00)"},
					"end_time":    map[string]interface{}{"type": "string", "description": "Время окончания RFC3339"},
					"location":    map[string]interface{}{"type": "string", "description": "Место проведения"},
					"attendees":   map[string]interface{}{"type": "array", "description": "Email адреса участников", "items": map[string]interface{}{"type": "string"}},
				},
				"required": []string{"title", "start_time", "end_time"},
			},
		},
		{
			"name":        "calendar_list_events",
			"description": "Получает список событий из Google Calendar",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"time_min":    map[string]interface{}{"type": "string", "description": "Начало периода RFC3339 (опционально)"},
					"time_max":    map[string]interface{}{"type": "string", "description": "Конец периода RFC3339 (опционально)"},
					"max_results": map[string]interface{}{"type": "integer", "description": "Максимальное количество событий (по умолчанию 10)"},
				},
				"required": []string{},
			},
		},
		{
			"name":        "calendar_delete_event",
			"description": "Удаляет событие из Google Calendar",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"event_id": map[string]interface{}{"type": "string", "description": "ID события для удаления"}},
				"required":   []string{"event_id"},
			},
		},
		{
			"name":        "calendar_get_event",
			"description": "Получает детали события из Google Calendar",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"event_id": map[string]interface{}{"type": "string", "description": "ID события"}},
				"required":   []string{"event_id"},
			},
		},
	}
	functions = append(functions, calendarFunctions...)

	sheetsFunctions := []map[string]interface{}{
		{
			"name":        "sheets_read_range",
			"description": "Читает данные из Google Sheets таблицы",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы из URL (между /d/ и /edit)"},
					"range":          map[string]interface{}{"type": "string", "description": "Диапазон ячеек (например: Лист1!A1:D10)"},
				},
				"required": []string{"spreadsheet_id", "range"},
			},
		},
		{
			"name":        "sheets_write_range",
			"description": "Записывает данные в Google Sheets таблицу",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
					"range":          map[string]interface{}{"type": "string", "description": "Диапазон для записи (например: Лист1!A1)"},
					"values":         map[string]interface{}{"type": "array", "description": "Двумерный массив данных [[row1], [row2]]", "items": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}},
				},
				"required": []string{"spreadsheet_id", "range", "values"},
			},
		},
		{
			"name":        "sheets_append_range",
			"description": "Добавляет данные в конец Google Sheets таблицы",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
					"range":          map[string]interface{}{"type": "string", "description": "Диапазон для добавления (например: Лист1!A:D)"},
					"values":         map[string]interface{}{"type": "array", "description": "Двумерный массив данных для добавления", "items": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}},
				},
				"required": []string{"spreadsheet_id", "range", "values"},
			},
		},
		{
			"name":        "sheets_create_spreadsheet",
			"description": "Создает новую Google Sheets таблицу",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"title":       map[string]interface{}{"type": "string", "description": "Название новой таблицы"},
					"sheet_names": map[string]interface{}{"type": "array", "description": "Названия листов (опционально)", "items": map[string]interface{}{"type": "string"}},
				},
				"required": []string{"title"},
			},
		},
		{
			"name":        "sheets_get_info",
			"description": "Получает информацию о Google Sheets таблице (листы, размеры)",
			"parameters": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"}},
				"required":   []string{"spreadsheet_id"},
			},
		},
	}
	functions = append(functions, sheetsFunctions...)

	if provider == create.ProviderOpenAI {
		tools := make([]create.ProviderTool, len(functions))
		for i, fn := range functions {
			tools[i] = create.ProviderTool{
				Type: create.ToolTypeFunction,
				Function: &create.ToolFunctionDefinition{
					Name:        fn["name"].(string),
					Description: fn["description"].(string),
					Parameters:  fn["parameters"],
				},
			}
		}
		return tools
	}

	if provider == create.ProviderMistral {
		tools := make([]map[string]interface{}, len(functions))
		for i, fn := range functions {
			tools[i] = map[string]interface{}{"type": "function", "function": fn}
		}
		return tools
	}

	return functions
}
