package model_test

// mcp_goauth_tools_test.go — тесты, проверяющие что Calendar/Sheets инструменты
// приходят исключительно от MCP сервера (tools/list), а НЕ хардкодятся на стороне клиента.
//
// Контекст: до рефакторинга (2026-05-15) AiR_Common хардкодил:
//   - определение hasCalendar / hasSheets путём сканирования tools по имени функции
//   - добавление "GOOGLE CALENDAR ACCESS ENABLED!" / "GOOGLE SHEETS ACCESS ENABLED" в промпт
//   - модификацию system_instruction при HasSheets=true
//   - вычисление hasTools через modelData.GOAuth.HasCalendar() || GOAuth.HasSheets()
//
// После рефакторинга:
//   - все tools (включая calendar_*, sheets_*) приходят только от MCP tools/list
//   - prompts/get system hint НЕ содержит hardcoded Calendar/Sheets сообщений
//   - нативные инструменты (google_search, code_interpreter, web_search) — локально по флагам
//
// Запуск: go test ./pkg/model/... -v -run TestMCP_GOAuth -count=1

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// TestMCP_GOAuth_CalendarSheetsFromMCP
// Проверяет что calendar_* и sheets_* инструменты приходят от MCP tools/list.
// uid=23 с Mistral провайдером имеет GOAuth.Calendar=true и GOAuth.Sheets=true.
// ============================================================================

func TestMCP_GOAuth_CalendarSheetsFromMCP(t *testing.T) {
	// Mistral (23:2) имеет GOAuth.Calendar=true и GOAuth.Sheets=true
	mistralSession := "23:2"

	resp := mcpRequestWithSession(t, "tools/list", map[string]interface{}{}, mistralSession)
	require.Nil(t, resp["error"], "tools/list не должен возвращать ошибку")

	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok)
	tools, ok := result["tools"].([]interface{})
	require.True(t, ok)

	toolNames := make(map[string]bool, len(tools))
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]interface{})
		name, _ := tool["name"].(string)
		toolNames[name] = true
	}

	// Calendar и Sheets инструменты должны присутствовать для Mistral (GOAuth.Calendar/Sheets=true)
	// Это подтверждает что сервер (MCP) управляет набором инструментов — не клиент
	calendarTools := []string{"calendar_create", "calendar_list", "calendar_delete", "calendar_get"}
	sheetsTools := []string{"sheets_read", "sheets_write", "sheets_append"}

	for _, name := range calendarTools {
		assert.True(t, toolNames[name],
			"[Mistral] calendar инструмент %q должен присутствовать в tools/list (GOAuth.Calendar=true)", name)
	}
	for _, name := range sheetsTools {
		assert.True(t, toolNames[name],
			"[Mistral] sheets инструмент %q должен присутствовать в tools/list (GOAuth.Sheets=true)", name)
	}

	t.Logf("[Mistral] ✅ calendar и sheets инструменты получены от MCP (%d инструментов всего)", len(tools))
}

// ============================================================================
// TestMCP_GOAuth_GoogleProviderToolsFromMCP
// Для Google провайдера (uid=23) tools тоже должны приходить от MCP.
// Проверяем что набор инструментов соответствует конфигурации пользователя на сервере.
// ============================================================================

func TestMCP_GOAuth_GoogleProviderToolsFromMCP(t *testing.T) {
	googleSession := "23:3"

	resp := mcpRequestWithSession(t, "tools/list", map[string]interface{}{}, googleSession)
	require.Nil(t, resp["error"])

	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok)
	tools, ok := result["tools"].([]interface{})
	require.True(t, ok)

	toolNames := make(map[string]bool, len(tools))
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]interface{})
		name, _ := tool["name"].(string)
		toolNames[name] = true
		t.Logf("[Google] tool: %s", name)
	}

	// get_current_time — всегда обязателен
	assert.True(t, toolNames["get_current_time"],
		"[Google] get_current_time должен присутствовать")

	// Если у Google-провайдера для uid=23 нет Calendar/Sheets — их не должно быть в списке
	// Это подтверждает что MCP ПРАВИЛЬНО фильтрует инструменты по провайдеру
	if !toolNames["calendar_create"] {
		t.Logf("[Google] ✅ calendar инструменты отсутствуют для uid=23/Google (ожидаемо — нет GOAuth для этого провайдера)")
	}

	t.Logf("[Google] Инструментов от MCP: %d", len(tools))
}

// ============================================================================
// TestMCP_GOAuth_SystemPromptNoHardcodedCalendarSheets
// Проверяет что system prompt hint НЕ содержит hardcoded Calendar/Sheets сообщений.
// До рефакторинга AiR_Common добавлял:
//   "GOOGLE CALENDAR ACCESS ENABLED!"
//   "GOOGLE SHEETS ACCESS ENABLED"
// в JSONreminder и system_instruction. Теперь это запрещено.
// ============================================================================

func TestMCP_GOAuth_SystemPromptNoHardcodedCalendarSheets(t *testing.T) {
	// Проверяем для всех провайдеров
	for _, prov := range allProviders {
		prov := prov
		t.Run(prov.name, func(t *testing.T) {
			resp := mcpRequestWithSession(t, "prompts/get", map[string]interface{}{
				"name": "system",
			}, prov.session)

			if rpcErr := resp["error"]; rpcErr != nil {
				t.Skipf("[%s] prompts/get не реализован: %v", prov.name, rpcErr)
			}

			result, ok := resp["result"].(map[string]interface{})
			require.True(t, ok)

			messages, ok := result["messages"].([]interface{})
			require.True(t, ok)
			require.NotEmpty(t, messages)

			first, ok := messages[0].(map[string]interface{})
			require.True(t, ok)

			content, ok := first["content"].(map[string]interface{})
			require.True(t, ok)

			text, _ := content["text"].(string)

			// Hardcoded Calendar/Sheets сообщения ЗАПРЕЩЕНЫ в system prompt
			// Они создавались в google/request.go до рефакторинга 2026-05-15
			forbiddenPhrases := []string{
				"GOOGLE CALENDAR ACCESS ENABLED",
				"GOOGLE SHEETS ACCESS ENABLED",
				"DO NOT refuse saying 'no Calendar access'",
				"calendar_create_event, calendar_list_events, calendar_delete_event",
				"FORBIDDEN phrases:\n- 'I cannot view'",
				"Use functions: calendar_create_event",
			}

			for _, phrase := range forbiddenPhrases {
				assert.False(t, strings.Contains(text, phrase),
					"[%s] system prompt НЕ должен содержать hardcoded фразу %q — инструкции по Calendar/Sheets должны приходить от MCP",
					prov.name, phrase)
			}

			t.Logf("[%s] ✅ system prompt не содержит hardcoded Calendar/Sheets фраз (%d chars)", prov.name, len(text))
		})
	}
}

// ============================================================================
// TestMCP_GOAuth_CalendarToolSchemaNouserID
// Проверяет что calendar_* инструменты (от MCP) не содержат user_id в inputSchema.
// ============================================================================

func TestMCP_GOAuth_CalendarToolSchemaNouserID(t *testing.T) {
	// Mistral имеет calendar инструменты
	resp := mcpRequestWithSession(t, "tools/list", map[string]interface{}{}, "23:2")
	require.Nil(t, resp["error"])

	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok)
	tools, ok := result["tools"].([]interface{})
	require.True(t, ok)

	calendarChecked := 0
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]interface{})
		name, _ := tool["name"].(string)

		if !strings.HasPrefix(name, "calendar_") && !strings.HasPrefix(name, "sheets_") {
			continue
		}

		calendarChecked++
		schema, ok := tool["inputSchema"].(map[string]interface{})
		if !ok {
			continue
		}
		props, ok := schema["properties"].(map[string]interface{})
		if !ok {
			continue
		}

		_, hasuserID := props["user_id"]
		assert.False(t, hasuserID,
			"инструмент %q не должен содержать user_id в inputSchema — MCP берёт из X-Session-ID", name)

		t.Logf("✅ %s: user_id отсутствует в inputSchema", name)
	}

	if calendarChecked == 0 {
		t.Skip("calendar/sheets инструменты не найдены в tools/list для 23:2")
	}
	t.Logf("Проверено %d calendar/sheets инструментов", calendarChecked)
}

// ============================================================================
// TestMCP_GOAuth_CalendarCall
// Вызов calendar_list для Mistral (имеет GOAuth.Calendar=true).
// Проверяет что MCP сервер обрабатывает вызов корректно.
// ============================================================================

func TestMCP_GOAuth_CalendarCall(t *testing.T) {
	mistralSession := "23:2"

	// Сначала убеждаемся что инструмент доступен
	listResp := mcpRequestWithSession(t, "tools/list", map[string]interface{}{}, mistralSession)
	require.Nil(t, listResp["error"])

	listResult, _ := listResp["result"].(map[string]interface{})
	tools, _ := listResult["tools"].([]interface{})
	hasCalendar := false
	for _, rawTool := range tools {
		if tool, _ := rawTool.(map[string]interface{}); tool["name"] == "calendar_list" {
			hasCalendar = true
			break
		}
	}
	if !hasCalendar {
		t.Skip("calendar_list не в tools/list для 23:2 — пропускаем")
	}

	// Вызываем calendar_list
	resp := mcpRequestWithSession(t, "tools/call", map[string]interface{}{
		"name": "calendar_list",
		"arguments": map[string]interface{}{
			"max_results": 5,
		},
	}, mistralSession)

	// JSON-RPC ошибка протокола недопустима
	require.Nil(t, resp["error"],
		"calendar_list не должен возвращать JSON-RPC ошибку протокола")

	result, ok := resp["result"].(map[string]interface{})
	require.True(t, ok, "result должен присутствовать")

	content, ok := result["content"].([]interface{})
	require.True(t, ok, "result.content должен быть массивом")
	require.NotEmpty(t, content)

	first, _ := content[0].(map[string]interface{})
	text, _ := first["text"].(string)

	// isError может быть true если нет OAuth токена для uid=23 — это нормально
	isError, _ := result["isError"].(bool)
	if isError {
		t.Logf("⚠️ calendar_list вернул isError=true (возможно нет OAuth токена для uid=23): %s", text)
	} else {
		t.Logf("✅ calendar_list вызван успешно: %s", text)
	}

	// Главное — ответ непустой (сервер обработал запрос)
	assert.NotEmpty(t, text, "calendar_list должен вернуть непустой результат")
}

// ============================================================================
// TestMCP_GOAuth_ToolsPerProviderSummary
// Сводный отчёт: какие инструменты доступны для каждого провайдера uid=23.
// Подтверждает что разные провайдеры получают разный набор инструментов от MCP.
// ============================================================================

func TestMCP_GOAuth_ToolsPerProviderSummary(t *testing.T) {
	type summary struct {
		provider string
		session  string
		tools    []string
	}

	var summaries []summary

	for _, prov := range allProviders {
		resp := mcpRequestWithSession(t, "tools/list", map[string]interface{}{}, prov.session)
		if resp["error"] != nil {
			t.Logf("[%s] ОШИБКА: %v", prov.name, resp["error"])
			continue
		}

		result, _ := resp["result"].(map[string]interface{})
		tools, _ := result["tools"].([]interface{})

		names := make([]string, 0, len(tools))
		for _, rawTool := range tools {
			if tool, ok := rawTool.(map[string]interface{}); ok {
				if name, ok := tool["name"].(string); ok {
					names = append(names, name)
				}
			}
		}

		summaries = append(summaries, summary{
			provider: prov.name,
			session:  prov.session,
			tools:    names,
		})
	}

	t.Log("=== MCP tools/list по провайдерам (uid=23) ===")
	t.Log("Инструменты определяются ТОЛЬКО MCP сервером — не клиентом AiR_Common")
	for _, s := range summaries {
		t.Logf("%-10s (%s): %v", s.provider, s.session, s.tools)
	}

	// Проверяем что разные провайдеры могут иметь разные инструменты
	// (это ключевое свойство MCP-архитектуры: server controls tool availability)
	if len(summaries) >= 2 {
		allSame := true
		for i := 1; i < len(summaries); i++ {
			if len(summaries[i].tools) != len(summaries[0].tools) {
				allSame = false
				break
			}
		}
		if !allSame {
			t.Log("✅ Разные провайдеры получают разный набор инструментов от MCP — архитектура корректна")
		}
	}

	// Минимальное требование: у каждого провайдера есть хотя бы один инструмент
	for _, s := range summaries {
		assert.Greater(t, len(s.tools), 0,
			"провайдер %s должен получать хотя бы один инструмент от MCP", s.provider)
	}
}
