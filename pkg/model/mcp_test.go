package model_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	mcpURL    = "https://localhost:8081/mcp"
	testUID   = 23
	testProv  = 1 // ProviderOpenAI
	sessionID = "23:1"
)

// mcpClient — тестовый HTTP клиент с отключённой проверкой TLS (self-signed cert).
func mcpClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}
}

// mcpRequest отправляет JSON-RPC запрос к MCP серверу и возвращает тело ответа.
func mcpRequest(t *testing.T, method string, params interface{}, withSession bool) map[string]interface{} {
	t.Helper()

	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  method,
		"params":  params,
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, mcpURL, bytes.NewBuffer(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if withSession {
		req.Header.Set("X-Session-ID", sessionID)
	}

	resp, err := mcpClient().Do(req)
	require.NoError(t, err, "MCP сервер недоступен на %s", mcpURL)
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	t.Logf("→ %s  ← HTTP %d  body: %s", method, resp.StatusCode, string(data))

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result), "ответ не является валидным JSON")
	return result
}

// mcpNotification отправляет JSON-RPC уведомление (без id) и проверяет HTTP 202.
func mcpNotification(t *testing.T, method string) {
	t.Helper()

	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  method,
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, mcpURL, bytes.NewBuffer(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := mcpClient().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	t.Logf("→ %s  ← HTTP %d", method, resp.StatusCode)
	assert.Equal(t, http.StatusAccepted, resp.StatusCode,
		"уведомление должно вернуть 202 Accepted")
}

// ============================================================================
// Тесты хендшейка
// ============================================================================

func TestMCP_Initialize(t *testing.T) {
	resp := mcpRequest(t, "initialize", map[string]interface{}{
		"protocolVersion": "2025-03-26",
		"clientInfo": map[string]interface{}{
			"name":    "AiR-Common-Test",
			"version": "1.0.0",
		},
		"capabilities": map[string]interface{}{},
	}, false)

	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok, "result должен присутствовать в ответе initialize")

	assert.NotEmpty(t, result["serverInfo"], "serverInfo должен быть непустым")
	assert.NotEmpty(t, result["capabilities"], "capabilities должен быть непустым")
	t.Logf("serverInfo: %v", result["serverInfo"])
}

func TestMCP_NotificationsInitialized(t *testing.T) {
	mcpNotification(t, "notifications/initialized")
}

// ============================================================================
// Тесты tools/list
// ============================================================================

func TestMCP_ToolsList(t *testing.T) {
	resp := mcpRequest(t, "tools/list", map[string]interface{}{}, true)

	require.Nil(t, resp["error"], "tools/list не должен возвращать ошибку")

	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok, "result должен присутствовать")

	tools, ok := result["tools"].([]interface{})
	require.True(t, ok, "result.tools должен быть массивом")
	require.NotEmpty(t, tools, "список инструментов не должен быть пустым для uid=%d", testUID)

	t.Logf("Инструментов: %d", len(tools))
	toolNames := make(map[string]bool, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]interface{})
		require.True(t, ok)
		name, _ := tool["name"].(string)
		toolNames[name] = true
		t.Logf("  • %s", name)

		// Каждый инструмент должен иметь name, description, inputSchema
		assert.NotEmpty(t, name, "имя инструмента не должно быть пустым")
		assert.NotEmpty(t, tool["description"], "описание инструмента %q не должно быть пустым", name)
		assert.NotNil(t, tool["inputSchema"], "inputSchema инструмента %q не должна быть nil", name)

		// user_id НЕ должен быть в параметрах — MCP берёт его из X-Session-ID
		if schema, ok := tool["inputSchema"].(map[string]interface{}); ok {
			if props, ok := schema["properties"].(map[string]interface{}); ok {
				_, hasUID := props["user_id"]
				assert.False(t, hasUID,
					"инструмент %q не должен содержать user_id в inputSchema", name)
			}
		}
	}

	// get_current_time — всегда обязателен
	assert.True(t, toolNames["get_current_time"],
		"get_current_time должен присутствовать в tools/list для любого пользователя")

	// get_s3_files — пользователь 23 имеет S3 (подтверждено TestMCP_Call_GetS3Files)
	assert.True(t, toolNames["get_s3_files"],
		"get_s3_files должен присутствовать: у uid=%d есть S3-файлы, но сервер не включает инструмент в tools/list", testUID)
	assert.True(t, toolNames["create_file"],
		"create_file должен присутствовать вместе с get_s3_files (S3=true)")
}

func TestMCP_ToolsList_NoSession(t *testing.T) {
	resp := mcpRequest(t, "tools/list", map[string]interface{}{}, false)
	// Без X-Session-ID должна быть ошибка
	assert.NotNil(t, resp["error"],
		"tools/list без X-Session-ID должен вернуть ошибку")
	if rpcErr, ok := resp["error"].(map[string]interface{}); ok {
		t.Logf("Код ошибки: %v, Сообщение: %v", rpcErr["code"], rpcErr["message"])
	}
}

// ============================================================================
// Тесты tools/call
// ============================================================================

func TestMCP_Call_GetCurrentTime(t *testing.T) {
	resp := mcpRequest(t, "tools/call", map[string]interface{}{
		"name":      "get_current_time",
		"arguments": map[string]interface{}{},
	}, true)

	require.Nil(t, resp["error"], "get_current_time не должен возвращать ошибку протокола")

	result := requireToolResult(t, resp)
	t.Logf("Текущее время: %s", result)

	assert.NotEmpty(t, result, "результат get_current_time не должен быть пустым")
}

func TestMCP_Call_GetS3Files(t *testing.T) {
	resp := mcpRequest(t, "tools/call", map[string]interface{}{
		"name":      "get_s3_files",
		"arguments": map[string]interface{}{},
	}, true)

	require.Nil(t, resp["error"], "get_s3_files не должен возвращать ошибку протокола")

	result := requireToolResult(t, resp)
	t.Logf("S3 файлы: %s", result)

	// Результат может быть пустым массивом [] или массивом URL
	assert.NotEmpty(t, result, "результат get_s3_files не должен быть пустой строкой")
}

func TestMCP_Call_UnknownTool(t *testing.T) {
	resp := mcpRequest(t, "tools/call", map[string]interface{}{
		"name":      "non_existent_tool_xyz",
		"arguments": map[string]interface{}{},
	}, true)

	// Должна быть ошибка: инструмент не существует
	rpcErr, hasErr := resp["error"]
	if hasErr {
		t.Logf("Ожидаемая ошибка протокола: %v", rpcErr)
	} else {
		// Или isError=true в result
		if result, ok := resp["result"].(map[string]interface{}); ok {
			isError, _ := result["isError"].(bool)
			assert.True(t, isError, "неизвестный инструмент должен вернуть isError=true")
		}
	}
}

// ============================================================================
// Тесты prompts/get
// ============================================================================

func TestMCP_PromptsList(t *testing.T) {
	resp := mcpRequest(t, "prompts/list", map[string]interface{}{}, true)

	if rpcErr := resp["error"]; rpcErr != nil {
		t.Skipf("prompts/list не реализован (ожидается): %v", rpcErr)
	}

	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok)

	prompts, ok := result["prompts"].([]interface{})
	require.True(t, ok, "result.prompts должен быть массивом")

	t.Logf("Промптов: %d", len(prompts))
	for _, p := range prompts {
		if pm, ok := p.(map[string]interface{}); ok {
			t.Logf("  • %s: %s", pm["name"], pm["description"])
		}
	}
}

func TestMCP_PromptsGet_System(t *testing.T) {
	resp := mcpRequest(t, "prompts/get", map[string]interface{}{
		"name": "system",
	}, true)

	if rpcErr := resp["error"]; rpcErr != nil {
		t.Skipf("prompts/get не реализован (ожидается): %v", rpcErr)
	}

	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok)

	messages, ok := result["messages"].([]interface{})
	require.True(t, ok, "result.messages должен быть массивом")
	require.NotEmpty(t, messages, "messages не должен быть пустым")

	first, ok := messages[0].(map[string]interface{})
	require.True(t, ok)

	content, ok := first["content"].(map[string]interface{})
	require.True(t, ok, "content должен присутствовать в первом сообщении")

	text, _ := content["text"].(string)
	require.NotEmpty(t, text, "text в промпте не должен быть пустым")

	t.Logf("System prompt hint (%d chars):\n%s", len(text), text)

	// Проверяем что промпт НЕ содержит text-mode артефакты
	assert.NotContains(t, text, "JSON: target=",
		"system prompt не должен содержать JSON: target= артефакт")
	assert.NotContains(t, text, "send_files=[]",
		"system prompt не должен содержать send_files=[] артефакт")
	assert.NotContains(t, text, "Return: valid JSON",
		"system prompt не должен содержать Return: valid JSON артефакт")

	// Пользователь 23 имеет S3 — hint должен содержать инструкции по файлам
	assert.Contains(t, text, "get_s3_files",
		"hint должен упоминать get_s3_files: у uid=%d есть S3 файлы", testUID)
}

func TestMCP_PromptsGet_UnknownName(t *testing.T) {
	resp := mcpRequest(t, "prompts/get", map[string]interface{}{
		"name": "non_existent_prompt",
	}, true)

	if resp["error"] != nil {
		t.Logf("Ошибка для неизвестного промпта: %v", resp["error"])
		return
	}
	// Или пустой result — тоже допустимо
	t.Logf("Ответ для неизвестного промпта: %v", resp["result"])
}

// ============================================================================
// Тест полного цикла: handshake → tools/list → tools/call
// ============================================================================

func TestMCP_FullCycle(t *testing.T) {
	t.Log("=== Шаг 1: initialize ===")
	initResp := mcpRequest(t, "initialize", map[string]interface{}{
		"protocolVersion": "2025-03-26",
		"clientInfo":      map[string]interface{}{"name": "test", "version": "0.1"},
		"capabilities":    map[string]interface{}{},
	}, false)
	require.Nil(t, initResp["error"])

	t.Log("=== Шаг 2: notifications/initialized ===")
	mcpNotification(t, "notifications/initialized")

	t.Log("=== Шаг 3: tools/list ===")
	listResp := mcpRequest(t, "tools/list", map[string]interface{}{}, true)
	require.Nil(t, listResp["error"])

	listResult := listResp["result"].(map[string]interface{})
	tools := listResult["tools"].([]interface{})
	require.NotEmpty(t, tools)

	// Собираем имена инструментов
	toolNames := make([]string, 0, len(tools))
	for _, raw := range tools {
		if tm, ok := raw.(map[string]interface{}); ok {
			if name, ok := tm["name"].(string); ok {
				toolNames = append(toolNames, name)
			}
		}
	}
	t.Logf("Доступные инструменты: %v", toolNames)

	t.Log("=== Шаг 4: tools/call get_current_time ===")
	timeResp := mcpRequest(t, "tools/call", map[string]interface{}{
		"name":      "get_current_time",
		"arguments": map[string]interface{}{},
	}, true)
	require.Nil(t, timeResp["error"])
	timeResult := requireToolResult(t, timeResp)
	t.Logf("Время: %s", timeResult)
	assert.NotEmpty(t, timeResult)

	t.Log("✅ Полный цикл MCP прошёл успешно")
}

// ============================================================================
// Вспомогательные функции
// ============================================================================

// requireToolResult извлекает text из result.content[0].text и проверяет структуру.
func requireToolResult(t *testing.T, resp map[string]interface{}) string {
	t.Helper()

	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok, "result должен присутствовать в ответе tools/call, got: %v", resp)

	content, ok := result["content"].([]interface{})
	require.True(t, ok, "result.content должен быть массивом")
	require.NotEmpty(t, content, "result.content не должен быть пустым")

	first, ok := content[0].(map[string]interface{})
	require.True(t, ok)

	assert.Equal(t, "text", first["type"], "тип контента должен быть text")

	text, _ := first["text"].(string)
	return text
}

// requireToolResult извлекает text из result.content[0].text и проверяет структуру.
func TestMCP_ErrorCodes(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantErrCode float64
		desc        string
		withSession bool
	}{
		{
			name:        "invalid json",
			body:        `{not valid json`,
			wantErrCode: -32700,
			desc:        "невалидный JSON → -32700 Parse error",
		},
		{
			name:        "missing method",
			body:        `{"jsonrpc":"2.0","id":"1"}`,
			wantErrCode: -32600,
			desc:        "нет method → -32600 Invalid Request",
		},
		{
			name:        "unknown method",
			body:        `{"jsonrpc":"2.0","id":"1","method":"unknown/method","params":{}}`,
			wantErrCode: -32601,
			desc:        "неизвестный метод → -32601 Method not found",
			withSession: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(
				context.Background(), http.MethodPost, mcpURL,
				bytes.NewBufferString(tc.body),
			)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			if tc.withSession {
				req.Header.Set("X-Session-ID", sessionID)
			}

			resp, err := mcpClient().Do(req)
			require.NoError(t, err, "сервер должен быть доступен")
			defer func() { _ = resp.Body.Close() }()

			data, _ := io.ReadAll(resp.Body)
			t.Logf("%s → HTTP %d: %s", tc.desc, resp.StatusCode, string(data))

			var result map[string]interface{}
			if err := json.Unmarshal(data, &result); err != nil {
				// Для -32700 сервер может вернуть не-JSON, это допустимо
				return
			}

			if rpcErr, ok := result["error"].(map[string]interface{}); ok {
				code, _ := rpcErr["code"].(float64)
				assert.Equal(t, tc.wantErrCode, code,
					"%s: ожидался код %v, получен %v", tc.desc, tc.wantErrCode, code)
			}
		})
	}
}

// BenchmarkMCP_GetCurrentTime измеряет латентность вызова get_current_time.
func BenchmarkMCP_GetCurrentTime(b *testing.B) {
	client := mcpClient()
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "1",
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "get_current_time",
			"arguments": map[string]interface{}{},
		},
	}
	raw, _ := json.Marshal(body)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, mcpURL, bytes.NewBuffer(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Session-ID", fmt.Sprintf("%d:%d", testUID, testProv))
		resp, err := client.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
}
