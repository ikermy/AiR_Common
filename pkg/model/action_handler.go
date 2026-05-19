package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// UniversalActionHandler универсальный обработчик функций для всех провайдеров
type UniversalActionHandler struct {
	port       string // Порт для внутренних HTTP запросов (MCP сервер)
	ctx        context.Context
	httpClient *http.Client // shared client с таймаутом
}

// NewUniversalActionHandler создаёт новый action handler с доступом к БД
func NewUniversalActionHandler(ctx context.Context, cfg *conf.Conf) *UniversalActionHandler {
	return &UniversalActionHandler{
		ctx:  ctx,
		port: cfg.WEB.Land,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// mcpURL возвращает URL MCP сервера в зависимости от режима (Production/Dev)
func (h *UniversalActionHandler) mcpURL() string {
	//if mode.ProductionMode {
	//	return fmt.Sprintf("http://localhost:%s/mcp", h.port)
	//}
	//return fmt.Sprintf("https://localhost:%s/mcp", h.port)
	return mode.MCPserver
}

// callMCP отправляет единый JSON-RPC запрос к MCP серверу (POST /mcp).
// UserID и provider передаются через заголовок X-Session-ID — инструменты не получают user_id в аргументах.
func (h *UniversalActionHandler) callMCP(ctx context.Context, toolName, arguments string, provider create.ProviderType, userID uint32) string {
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.mcpURL(), bytes.NewBuffer(bodyBytes))
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": "failed to create MCP request"})
		return string(result)
	}
	req.Header.Set("Content-Type", "application/json")
	// Идентификация пользователя и провайдера — реальный UserID без кодирования
	req.Header.Set("X-Session-ID", fmt.Sprintf("%d:%d", userID, provider))

	resp, err := h.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			result, _ := json.Marshal(map[string]string{"error": "запрос отменён по таймауту"})
			return string(result)
		}
		result, _ := json.Marshal(map[string]string{"error": "MCP request failed: " + err.Error()})
		return string(result)
	}
	defer func() { _ = resp.Body.Close() }()

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
func (h *UniversalActionHandler) callMCPMethod(ctx context.Context, method string, params map[string]interface{}, provider create.ProviderType, userID uint32) ([]byte, error) {
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.mcpURL(), bytes.NewBuffer(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create MCP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-ID", fmt.Sprintf("%d:%d", userID, provider))

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("MCP request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read MCP response: %w", err)
	}
	return body, nil
}

// FetchToolsList реализует MCPConfigProvider: вызывает MCP tools/list и возвращает
// function-инструменты для данного пользователя (без user_id в inputSchema).
// Нативные OpenAI инструменты (code_interpreter, web_search) не включаются.
func (h *UniversalActionHandler) FetchToolsList(ctx context.Context, userID uint32, provider create.ProviderType) ([]MCPToolDefinition, error) {
	body, err := h.callMCPMethod(ctx, "tools/list", map[string]interface{}{}, provider, userID)
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
func (h *UniversalActionHandler) FetchSystemPrompt(ctx context.Context, userID uint32, provider create.ProviderType) (string, error) {
	body, err := h.callMCPMethod(ctx, "prompts/get", map[string]interface{}{"name": "system"}, provider, userID)
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

func (h *UniversalActionHandler) RunAction(ctx context.Context, functionName, arguments string, provider create.ProviderType, userID uint32) string {
	// Все инструменты — через MCP сервер (включая lead_target).
	// MCP сервер сам решает какие инструменты доступны пользователю и выполняет их.
	return h.callMCP(ctx, functionName, arguments, provider, userID)
}
