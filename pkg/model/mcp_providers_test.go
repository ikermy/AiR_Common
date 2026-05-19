package model_test

// mcp_providers_test.go — тесты инструментов MCP для всех провайдеров, uid=23.
// Запуск: go test ./pkg/model/... -v -run TestMCP_Providers -count=1
// Требует: MCP-сервер на https://localhost:8081/mcp (self-signed TLS).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// providerCase описывает один провайдер для параметризованных тестов.
type providerCase struct {
	name     string // человекочитаемое имя
	typeID   uint8  // числовой тип провайдера (1=OpenAI, 2=Mistral, 3=Google)
	session  string // X-Session-ID header — "UserID:providerType"
	wantTime bool   // get_current_time обязателен для всех
	wantS3   bool   // ожидаем get_s3_files / create_file (uid=23 имеет S3)
}

var allProviders = []providerCase{
	{
		name:     "OpenAI",
		typeID:   1,
		session:  fmt.Sprintf("%d:%d", testUID, 1),
		wantTime: true,
		wantS3:   true, // uid=23 имеет S3 для OpenAI
	},
	{
		name:     "Mistral",
		typeID:   2,
		session:  fmt.Sprintf("%d:%d", testUID, 2),
		wantTime: true,
		wantS3:   false, // uid=23 не имеет S3 для Mistral
	},
	{
		name:     "Google",
		typeID:   3,
		session:  fmt.Sprintf("%d:%d", testUID, 3),
		wantTime: true,
		wantS3:   false, // uid=23 не имеет S3 для Google
	},
}

// mcpRequestWithSession — как mcpRequest, но принимает произвольный sessionID.
func mcpRequestWithSession(t *testing.T, method string, params interface{}, sid string) map[string]interface{} {
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
	req.Header.Set("X-Session-ID", sid)

	resp, err := mcpClient().Do(req)
	if err != nil {
		t.Skipf("MCP сервер недоступен на %s: %v", mcpURL, err)
		return nil
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	t.Logf("[%s] → %s  ← HTTP %d  body: %s", sid, method, resp.StatusCode, string(data))

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &result), "ответ не является валидным JSON")
	return result
}

// ============================================================================
// TestMCP_Providers_ToolsList — tools/list для каждого провайдера
// ============================================================================

func TestMCP_Providers_ToolsList(t *testing.T) {
	for _, prov := range allProviders {
		prov := prov // capture
		t.Run(prov.name, func(t *testing.T) {
			resp := mcpRequestWithSession(t, "tools/list", map[string]interface{}{}, prov.session)

			require.Nil(t, resp["error"],
				"[%s] tools/list не должен возвращать ошибку", prov.name)

			result, ok := resp["result"].(map[string]interface{})
			require.True(t, ok, "[%s] result должен присутствовать", prov.name)

			tools, ok := result["tools"].([]interface{})
			require.True(t, ok, "[%s] result.tools должен быть массивом", prov.name)
			require.NotEmpty(t, tools,
				"[%s] список инструментов не должен быть пустым для uid=%d", prov.name, testUID)

			t.Logf("[%s] Инструментов: %d", prov.name, len(tools))

			toolNames := make(map[string]bool, len(tools))
			for _, rawTool := range tools {
				tool, ok := rawTool.(map[string]interface{})
				require.True(t, ok, "[%s] каждый элемент tools должен быть объектом", prov.name)

				name, _ := tool["name"].(string)
				toolNames[name] = true
				t.Logf("  • %s", name)

				// Базовые поля
				assert.NotEmpty(t, name,
					"[%s] имя инструмента не должно быть пустым", prov.name)
				assert.NotEmpty(t, tool["description"],
					"[%s] description инструмента %q не должно быть пустым", prov.name, name)
				assert.NotNil(t, tool["inputSchema"],
					"[%s] inputSchema инструмента %q не должна быть nil", prov.name, name)

				// user_id НЕ должен быть в inputSchema — MCP извлекает из X-Session-ID
				if schema, ok := tool["inputSchema"].(map[string]interface{}); ok {
					if props, ok := schema["properties"].(map[string]interface{}); ok {
						_, hasUID := props["user_id"]
						assert.False(t, hasUID,
							"[%s] инструмент %q не должен содержать user_id в inputSchema", prov.name, name)
					}
				}
			}

			// get_current_time — обязателен для всех провайдеров
			if prov.wantTime {
				assert.True(t, toolNames["get_current_time"],
					"[%s] get_current_time должен присутствовать для uid=%d", prov.name, testUID)
			}

			// get_s3_files / create_file — uid=23 имеет S3
			if prov.wantS3 {
				assert.True(t, toolNames["get_s3_files"],
					"[%s] get_s3_files должен присутствовать: uid=%d имеет S3", prov.name, testUID)
				assert.True(t, toolNames["create_file"],
					"[%s] create_file должен присутствовать вместе с get_s3_files", prov.name)
			}
		})
	}
}

// ============================================================================
// TestMCP_Providers_CallGetCurrentTime — вызов get_current_time для каждого провайдера
// ============================================================================

func TestMCP_Providers_CallGetCurrentTime(t *testing.T) {
	for _, prov := range allProviders {
		prov := prov
		t.Run(prov.name, func(t *testing.T) {
			resp := mcpRequestWithSession(t, "tools/call", map[string]interface{}{
				"name":      "get_current_time",
				"arguments": map[string]interface{}{},
			}, prov.session)

			require.Nil(t, resp["error"],
				"[%s] get_current_time не должен возвращать ошибку протокола", prov.name)

			result, ok := resp["result"].(map[string]interface{})
			require.True(t, ok, "[%s] result должен присутствовать", prov.name)

			content, ok := result["content"].([]interface{})
			require.True(t, ok, "[%s] result.content должен быть массивом", prov.name)
			require.NotEmpty(t, content, "[%s] result.content не должен быть пустым", prov.name)

			first, ok := content[0].(map[string]interface{})
			require.True(t, ok)

			assert.Equal(t, "text", first["type"],
				"[%s] тип контента должен быть text", prov.name)

			text, _ := first["text"].(string)
			assert.NotEmpty(t, text,
				"[%s] результат get_current_time не должен быть пустым", prov.name)

			t.Logf("[%s] Текущее время: %s", prov.name, text)
		})
	}
}

// ============================================================================
// TestMCP_Providers_CallGetS3Files — вызов get_s3_files для каждого провайдера
// ============================================================================

func TestMCP_Providers_CallGetS3Files(t *testing.T) {
	for _, prov := range allProviders {
		prov := prov
		t.Run(prov.name, func(t *testing.T) {
			resp := mcpRequestWithSession(t, "tools/call", map[string]interface{}{
				"name":      "get_s3_files",
				"arguments": map[string]interface{}{},
			}, prov.session)

			require.Nil(t, resp["error"],
				"[%s] get_s3_files не должен возвращать ошибку протокола", prov.name)

			result, ok := resp["result"].(map[string]interface{})
			require.True(t, ok, "[%s] result должен присутствовать", prov.name)

			content, ok := result["content"].([]interface{})
			require.True(t, ok, "[%s] result.content должен быть массивом", prov.name)
			require.NotEmpty(t, content)

			first, ok := content[0].(map[string]interface{})
			require.True(t, ok)

			text, _ := first["text"].(string)
			assert.NotEmpty(t, text,
				"[%s] get_s3_files должен вернуть непустой результат для uid=%d", prov.name, testUID)

			t.Logf("[%s] S3 файлы: %s", prov.name, text)
		})
	}
}

// ============================================================================
// TestMCP_Providers_NouserIDInSchema — проверка отсутствия user_id у всех инструментов
// всех провайдеров (агрегированный отчёт)
// ============================================================================

func TestMCP_Providers_NouserIDInSchema(t *testing.T) {
	for _, prov := range allProviders {
		prov := prov
		t.Run(prov.name, func(t *testing.T) {
			resp := mcpRequestWithSession(t, "tools/list", map[string]interface{}{}, prov.session)
			require.Nil(t, resp["error"])

			result, ok := resp["result"].(map[string]interface{})
			require.True(t, ok)

			tools, ok := result["tools"].([]interface{})
			require.True(t, ok)

			violations := 0
			for _, rawTool := range tools {
				tool, _ := rawTool.(map[string]interface{})
				name, _ := tool["name"].(string)
				if schema, ok := tool["inputSchema"].(map[string]interface{}); ok {
					if props, ok := schema["properties"].(map[string]interface{}); ok {
						if _, hasUID := props["user_id"]; hasUID {
							t.Errorf("[%s] инструмент %q содержит запрещённый параметр user_id в inputSchema", prov.name, name)
							violations++
						}
					}
				}
			}

			if violations == 0 {
				t.Logf("[%s] ✅ Ни один из %d инструментов не содержит user_id в inputSchema", prov.name, len(tools))
			}
		})
	}
}

// ============================================================================
// TestMCP_Providers_ToolsCount — сравнение количества инструментов по провайдерам
// ============================================================================

func TestMCP_Providers_ToolsCount(t *testing.T) {
	type countResult struct {
		provider string
		count    int
		names    []string
	}

	results := make([]countResult, 0, len(allProviders))

	for _, prov := range allProviders {
		resp := mcpRequestWithSession(t, "tools/list", map[string]interface{}{}, prov.session)
		if resp["error"] != nil {
			t.Logf("[%s] ОШИБКА: %v", prov.name, resp["error"])
			continue
		}

		result, ok := resp["result"].(map[string]interface{})
		if !ok {
			continue
		}
		tools, ok := result["tools"].([]interface{})
		if !ok {
			continue
		}

		names := make([]string, 0, len(tools))
		for _, rawTool := range tools {
			if tool, ok := rawTool.(map[string]interface{}); ok {
				if name, ok := tool["name"].(string); ok {
					names = append(names, name)
				}
			}
		}

		results = append(results, countResult{
			provider: prov.name,
			count:    len(tools),
			names:    names,
		})
	}

	t.Log("=== Сводка инструментов по провайдерам (uid=23) ===")
	for _, r := range results {
		t.Logf("%-10s : %d инструментов → %v", r.provider, r.count, r.names)
	}

	// Каждый провайдер может иметь свой уникальный набор инструментов.
	// Минимальное требование: у каждого провайдера хотя бы один инструмент (get_current_time).
	for _, r := range results {
		assert.Greater(t, r.count, 0,
			"провайдер %s должен возвращать хотя бы один инструмент", r.provider)
	}
}

// ============================================================================
// TestMCP_LeadTarget_Schema — проверка схемы lead_target у всех провайдеров
// ============================================================================
// lead_target появляется только для пользователей с включённым MetaAction.
// Если uid=23 не имеет MetaAction — тест логирует отсутствие и пропускается.
// После реализации в AiR_Landing инструмент должен появиться в tools/list.

func TestMCP_LeadTarget_Schema(t *testing.T) {
	for _, prov := range allProviders {
		prov := prov
		t.Run(prov.name, func(t *testing.T) {
			resp := mcpRequestWithSession(t, "tools/list", map[string]interface{}{}, prov.session)
			require.Nil(t, resp["error"])

			result, ok := resp["result"].(map[string]interface{})
			require.True(t, ok)
			tools, ok := result["tools"].([]interface{})
			require.True(t, ok)

			// Ищем lead_target в списке
			var leadTarget map[string]interface{}
			for _, rawTool := range tools {
				tool, _ := rawTool.(map[string]interface{})
				if tool["name"] == "lead_target" {
					leadTarget = tool
					break
				}
			}

			if leadTarget == nil {
				t.Logf("[%s] lead_target отсутствует в tools/list для uid=%d — "+
					"либо MetaAction не включён, либо не реализован в MCP (см. MCP_MIGRATION.md раздел 17)",
					prov.name, testUID)
				t.Skip("lead_target не доступен — пропускаем")
				return
			}

			t.Logf("[%s] ✅ lead_target найден в tools/list", prov.name)

			// Проверяем поля
			assert.NotEmpty(t, leadTarget["description"],
				"[%s] lead_target должен иметь description", prov.name)
			assert.NotNil(t, leadTarget["inputSchema"],
				"[%s] lead_target должен иметь inputSchema", prov.name)

			// Проверяем inputSchema: должен быть resp_id, НЕ должно быть user_id
			if schema, ok := leadTarget["inputSchema"].(map[string]interface{}); ok {
				if props, ok := schema["properties"].(map[string]interface{}); ok {
					_, hasRespId := props["resp_id"]
					assert.True(t, hasRespId,
						"[%s] lead_target.inputSchema должен содержать resp_id", prov.name)

					_, hasuserID := props["user_id"]
					assert.False(t, hasuserID,
						"[%s] lead_target.inputSchema не должен содержать user_id — MCP берёт его из X-Session-ID", prov.name)
				}
			}
		})
	}
}

// ============================================================================
// TestMCP_LeadTarget_Call — вызов lead_target через MCP
// ============================================================================

func TestMCP_LeadTarget_Call(t *testing.T) {
	const testRespId = 999 // тестовый resp_id; реальный сервис может вернуть ошибку — это ожидаемо

	for _, prov := range allProviders {
		prov := prov
		t.Run(prov.name, func(t *testing.T) {
			// Сначала проверяем что инструмент доступен
			listResp := mcpRequestWithSession(t, "tools/list", map[string]interface{}{}, prov.session)
			if listResp["error"] != nil {
				t.Skipf("[%s] tools/list вернул ошибку, пропускаем", prov.name)
				return
			}
			listResult, _ := listResp["result"].(map[string]interface{})
			tools, _ := listResult["tools"].([]interface{})
			hasLeadTarget := false
			for _, rawTool := range tools {
				if tool, _ := rawTool.(map[string]interface{}); tool["name"] == "lead_target" {
					hasLeadTarget = true
					break
				}
			}
			if !hasLeadTarget {
				t.Skipf("[%s] lead_target не в tools/list для uid=%d — пропускаем вызов", prov.name, testUID)
				return
			}

			// Вызываем lead_target
			resp := mcpRequestWithSession(t, "tools/call", map[string]interface{}{
				"name":      "lead_target",
				"arguments": map[string]interface{}{"resp_id": testRespId},
			}, prov.session)

			// Ошибка протокола (JSON-RPC error) недопустима
			require.Nil(t, resp["error"],
				"[%s] lead_target не должен возвращать JSON-RPC ошибку протокола", prov.name)

			result, ok := resp["result"].(map[string]interface{})
			require.True(t, ok, "[%s] result должен присутствовать", prov.name)

			content, ok := result["content"].([]interface{})
			require.True(t, ok, "[%s] result.content должен быть массивом", prov.name)
			require.NotEmpty(t, content)

			first, _ := content[0].(map[string]interface{})
			text, _ := first["text"].(string)

			t.Logf("[%s] lead_target(resp_id=%d) → %s", prov.name, testRespId, text)

			// Результат должен быть непустым JSON
			assert.NotEmpty(t, text, "[%s] lead_target должен вернуть непустой результат", prov.name)

			// Если вернул JSON с "target": true — это успех
			var resultMap map[string]interface{}
			if err := json.Unmarshal([]byte(text), &resultMap); err == nil {
				if target, ok := resultMap["target"].(bool); ok {
					t.Logf("[%s] lead_target.target = %v", prov.name, target)
				}
			}
		})
	}
}
