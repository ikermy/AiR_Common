package create

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/ikermy/AiR_Common/pkg/mode"
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
	promptFetcher  GooglePromptHintFetcher
	toolsFetcher   GoogleFunctionDeclarationsFetcher
	keyResolver    func(userID uint32) string // Резолвер персональных ключей; nil → глобальный apiKey
}

// SetKeyResolver устанавливает функцию-резолвер персонального API-ключа пользователя.
func (m *MistralAgentClient) SetKeyResolver(fn func(userID uint32) string) {
	m.keyResolver = fn
}

// resolveKey возвращает API-ключ: персональный для userID (если задан) или глобальный.
func (m *MistralAgentClient) resolveKey(userID uint32) string {
	if m.keyResolver != nil && userID != 0 {
		if key := m.keyResolver(userID); key != "" {
			return key
		}
	}
	return m.apiKey
}

// SetMCPConfigFetchers устанавливает внешние fetchers для prompt hint и function declarations.
// Аналогично GoogleAgentClient.SetMCPConfigFetchers — используется при создании агента.
func (m *MistralAgentClient) SetMCPConfigFetchers(promptFetcher GooglePromptHintFetcher, toolsFetcher GoogleFunctionDeclarationsFetcher) {
	m.promptFetcher = promptFetcher
	m.toolsFetcher = toolsFetcher
}

// HasAPIKey возвращает true если для пользователя есть действующий API-ключ.
// Используется для ранней проверки перед выполнением запросов.
func (m *MistralAgentClient) HasAPIKey(userID uint32) bool {
	return m.resolveKey(userID) != ""
}

// deleteMistralModel удаляет Mistral Agent (с поддержкой WS сообщений)
func (m *UniversalModel) deleteMistralModel(userID uint32, modelData *UserModelRecord, deleteFiles bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Удаление Mistral агента...")
	}

	// Удаляем агента через API
	if m.mistralClient != nil {
		if err := m.mistralClient.deleteAgent(modelData.AssistId); err != nil {
			//logger.Error("ошибка удаления Mistral агента %s: %v", modelData.AssistId, err, userID)
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
			modelJSON, err := m.ReadModel(userID, &provider)
			if err != nil {
				//logger.Error("Ошибка получения данных модели для удаления файлов: %v", err, userID)
			} else if modelJSON != nil && len(modelJSON.VecIds.VectorId) > 0 {
				libraryID := modelJSON.VecIds.VectorId[0]

				// Удаляем каждый документ из библиотеки
				for i, file := range modelData.FileIds {
					if err := m.mistralClient.DeleteDocumentFromLibrary(libraryID, file.ID); err != nil {
						//logger.Error("Ошибка удаления документа %s из библиотеки: %v", file.ID, err, userID)
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
					//logger.Error("Ошибка удаления библиотеки %s: %v", libraryID, err, userID)
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
		//logger.Warn("Mistral клиент не инициализирован, пропускаем удаление из API", userID)
		if progressCallback != nil {
			progressCallback("⚠️ Mistral клиент не инициализирован, удаляем только из БД")
		}
	}

	if progressCallback != nil {
		progressCallback("✅ Mistral агент и файлы удалены из API")
	}

	//logger.Debug("Mistral модель успешно удалена из API", userID)
	return nil
}

// deleteAgent удаляет Mistral Agent по ID
func (m *MistralAgentClient) deleteAgent(agentID string) error {
	deleteURL := fmt.Sprintf("%s/%s", mode.MistralAgentsBaseURL, agentID)

	return m.executeMistralDeleteRequest(deleteURL)
}

// updateMistralModelInPlace обновляет Mistral Agent
func (m *UniversalModel) updateMistralModelInPlace(userID uint32, existing, updated *UniversalModelData) error {
	if m.mistralClient == nil {
		return fmt.Errorf("Mistral клиент не инициализирован")
	}

	// Для Mistral нужно удалить старого агента и создать нового
	// (Mistral API может не поддерживать PATCH/UPDATE агентов)

	// Получаем все модели пользователя и находим нужную
	allModels, err := m.db.GetAllUserModels(userID)
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
		//logger.Debug("Файлы изменились, используем обновленные данные библиотеки", userID)
	} else {
		// Файлы не изменились - используем существующие VectorId и FileIds
		updated.VecIds.VectorId = existing.VecIds.VectorId
		updated.FileIds = existing.FileIds
	}

	// Удаляем старого агента
	if err := m.mistralClient.deleteAgent(existingModelData.AssistId); err != nil {
		//logger.Warn("Не удалось удалить старого Mistral агента %s: %v", existingModelData.AssistId, err, userID)
	}

	// Создаем нового агента с обновленными данными
	umcr, err := m.mistralClient.createMistralAgent(updated, userID, updated.FileIds)
	if err != nil {
		return fmt.Errorf("ошибка создания нового Mistral агента: %w", err)
	}

	// Сохраняем в БД
	if err := m.SaveModel(userID, umcr, updated); err != nil {
		return fmt.Errorf("ошибка сохранения обновленной модели в БД: %w", err)
	}

	//logger.Debug("Mistral Agent успешно обновлен (новый ID: %s)", umcr.AssistID, userID)
	return nil
}

// createMistralModel создаёт Mistral Agent (внутренний метод)
func (m *UniversalModel) createMistralModel(userID uint32, modelData *UniversalModelData, fileIDs []Ids) (UMCR, error) {
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
	umcr, err := m.mistralClient.createMistralAgent(modelData, userID, fileIDs)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка создания Mistral агента: %w", err)
	}

	return umcr, nil
}

// createMistralAgent создает нового агента с указанными параметрами
func (m *MistralAgentClient) createMistralAgent(modelData *UniversalModelData, userID uint32, fileIDs []Ids) (UMCR, error) {
	if modelData == nil {
		return UMCR{}, fmt.Errorf("modelData не может быть nil")
	}

	baseURL := mode.MistralAgentsBaseURL

	description := fmt.Sprintf("Agent for user %d", userID)

	// ============================================================================
	// SYSTEM PROMPT — базовый prompt + hint от MCP.
	// Все инструкции по функциям приходят от MCP (FetchSystemPrompt).
	// Хардкодированные инструкции по S3/Calendar/Sheets/Image удалены.
	// ============================================================================
	enhancedPrompt := modelData.Prompt + "\n\n"

	if m.promptFetcher != nil {
		if hint, fetchErr := m.promptFetcher(m.ctx, userID, ProviderMistral); fetchErr == nil && hint != "" {
			enhancedPrompt += hint + "\n\n"
		}
	}

	// Напоминание про target/operator — системные поля ответа, не зависят от MCP.
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

	// target/operator rules
	if modelData.MetaAction != "" {
		enhancedPrompt += "**target** (boolean) - Is the dialog GOAL achieved:\n" +
			"  Check the goal condition from YOUR INSTRUCTIONS ABOVE\n" +
			"  If condition is EXACTLY met → target: true\n" +
			"  If condition is NOT met → target: false\n\n"
	} else {
		enhancedPrompt += "**target**: ALWAYS false (no goal)\n\n"
	}
	if modelData.Operator {
		enhancedPrompt += "**operator** (boolean) - Is operator required:\n" +
			"  Check the operator condition from YOUR INSTRUCTIONS ABOVE\n" +
			"  If user requests operator → operator: true\n" +
			"  In all other cases → operator: false\n\n"
	} else {
		enhancedPrompt += "**operator**: ALWAYS false (operator disabled)\n\n"
	}

	enhancedPrompt += "IMPORTANT: Your response MUST be valid JSON (you may wrap in ```json):\n" +
		MistralSchemaJSON + "\n\n" +
		"Always return response strictly in this JSON format. You may use markdown: ```json\\n{...}\\n```"

	payload := map[string]any{
		"name":         modelData.Name,
		"model":        modelData.GptType.Name,
		"description":  description,
		"instructions": enhancedPrompt,
	}

	// ============================================================================
	// FUNCTION TOOLS — только от MCP. Нет fallback-хардкода.
	// ============================================================================
	var tools []map[string]any

	if m.toolsFetcher != nil {
		if mcpFunctions, fetchErr := m.toolsFetcher(m.ctx, userID, ProviderMistral); fetchErr == nil {
			for _, f := range mcpFunctions {
				tools = append(tools, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        f.Name,
						"description": f.Description,
						"parameters":  f.Parameters,
					},
				})
			}
		}
	}

	// ============================================================================
	// BUILT-IN MISTRAL TOOLS — нативные возможности API, не MCP-функции.
	// ============================================================================
	if modelData.Interpreter {
		tools = append(tools, map[string]any{"type": "code_interpreter"})
	}
	if modelData.Image {
		tools = append(tools, map[string]any{"type": "image_generation"})
	}
	if modelData.WebSearch {
		tools = append(tools, map[string]any{"type": "web_search"})
	}

	// document_library — если включён поиск по документам
	if modelData.Search || len(fileIDs) > 0 || len(modelData.VecIds.VectorId) > 0 {
		documentLibraryTool := map[string]any{
			"type": "document_library",
		}
		if len(modelData.VecIds.VectorId) > 0 {
			documentLibraryTool["library_ids"] = modelData.VecIds.VectorId
		}
		tools = append(tools, documentLibraryTool)
	}

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

	req.Header.Set("Authorization", "Bearer "+m.resolveKey(userID))
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

	var response map[string]any
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return UMCR{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	agentID, ok := response["id"].(string)
	if !ok {
		return UMCR{}, fmt.Errorf("не удалось получить ID созданного агента")
	}

	var allIds []byte
	if len(fileIDs) > 0 || len(modelData.VecIds.VectorId) > 0 {
		type VecIds struct {
			FileIds  []Ids    `json:"FileIds"`
			VectorId []string `json:"VectorId"`
		}
		vecIds := VecIds{FileIds: fileIDs, VectorId: modelData.VecIds.VectorId}
		allIds, err = json.Marshal(vecIds)
		if err != nil {
			return UMCR{}, fmt.Errorf("ошибка при преобразовании vecIds в JSON: %w", err)
		}
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
// userID: ID пользователя для резолвинга персонального API-ключа (0 = глобальный ключ)
func (m *MistralAgentClient) executeMistralRequest(method, url string, body []byte, successStatuses []int, userID uint32) ([]byte, error) {
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

	req.Header.Set("Authorization", "Bearer "+m.resolveKey(userID))
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
		[]int{http.StatusOK, http.StatusNoContent, http.StatusNotFound}, 0)
	return err
}

// executeMistralGetRequest получает данные через общий API (GET)
func (m *MistralAgentClient) executeMistralGetRequest(url string) ([]byte, error) {
	return m.executeMistralRequest(http.MethodGet, url, nil, nil, 0)
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
