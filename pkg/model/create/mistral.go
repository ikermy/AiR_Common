package models

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ikermy/AiR_Common/pkg/logger"
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
								"description": "Подпись к файлу"
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
	"required": ["message", "action", "target", "operator"]
}`

// MistralLibrary представляет библиотеку документов Mistral
type MistralLibrary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// MistralDocument представляет документ в библиотеке Mistral
type MistralDocument struct {
	ID        string `json:"id"`
	LibraryID string `json:"library_id,omitempty"`
	FileName  string `json:"file_name"`
	Status    string `json:"status,omitempty"` // processing, processed, failed
	CreatedAt string `json:"created_at,omitempty"`
}

// MistralAgentClient клиент для работы с Mistral Agents API
type MistralAgentClient struct {
	apiKey string
	url    string
	ctx    context.Context
}

// deleteMistralModel удаляет Mistral Agent (с поддержкой WS сообщений)
func (m *UniversalModel) deleteMistralModel(userId uint32, modelData *UserModelRecord, deleteFiles bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Удаление Mistral агента...")
	}

	// Удаляем агента через API
	if m.mistralClient != nil {
		if err := m.mistralClient.deleteAgent(modelData.AssistId); err != nil {
			logger.Error("ошибка удаления Mistral агента %s: %v", modelData.AssistId, err, userId)
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
				logger.Error("Ошибка получения данных модели для удаления файлов: %v", err, userId)
			} else if modelJSON != nil && len(modelJSON.VecIds.VectorId) > 0 {
				libraryID := modelJSON.VecIds.VectorId[0]

				// Удаляем каждый документ из библиотеки
				for i, file := range modelData.FileIds {
					if err := m.mistralClient.DeleteDocumentFromLibrary(libraryID, file.ID); err != nil {
						logger.Error("Ошибка удаления документа %s из библиотеки: %v", file.ID, err, userId)
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
					logger.Error("Ошибка удаления библиотеки %s: %v", libraryID, err, userId)
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
		logger.Warn("Mistral клиент не инициализирован, пропускаем удаление из API", userId)
		if progressCallback != nil {
			progressCallback("⚠️ Mistral клиент не инициализирован, удаляем только из БД")
		}
	}

	if progressCallback != nil {
		progressCallback("✅ Mistral агент и файлы удалены из API")
	}

	logger.Info("Mistral модель успешно удалена из API", userId)
	return nil
}

// deleteAgent удаляет Mistral Agent по ID
func (m *MistralAgentClient) deleteAgent(agentID string) error {
	// Убираем /completions из URL
	baseURL := strings.Replace(m.url, "/completions", "", 1)
	deleteURL := fmt.Sprintf("%s/%s", baseURL, agentID)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodDelete, deleteURL, nil)
	if err != nil {
		return fmt.Errorf("ошибка создания DELETE запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
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

	return nil
}

// updateMistralModelInPlace обновляет Mistral Agent
func (m *UniversalModel) updateMistralModelInPlace(userId uint32, existing, updated *UniversalModelData, modelJSON []byte) error {
	if m.mistralClient == nil {
		return fmt.Errorf("Mistral клиент не инициализирован")
	}

	// Для Mistral нужно удалить старого агента и создать нового
	// (Mistral API может не поддерживать PATCH/UPDATE агентов)

	existingModelData, err := m.db.GetModelByProvider(userId, existing.Provider)
	if err != nil || existingModelData == nil {
		return fmt.Errorf("ошибка получения записи модели: %w", err)
	}

	// Проверяем, изменились ли файлы (аналогично OpenAI)
	// Если файлы не изменились - используем существующие VectorId (library_ids)
	if !filesEqual(existing.FileIds, updated.FileIds) {
		// Файлы изменились - библиотека уже обновлена, используем новые данные
		logger.Debug("Файлы изменились, используем обновленные данные библиотеки", userId)
	} else {
		// Файлы не изменились - используем существующие VectorId и FileIds
		updated.VecIds.VectorId = existing.VecIds.VectorId
		updated.FileIds = existing.FileIds
	}

	// Удаляем старого агента
	if err := m.mistralClient.deleteAgent(existingModelData.AssistId); err != nil {
		logger.Warn("Не удалось удалить старого Mistral агента %s: %v", existingModelData.AssistId, err, userId)
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

	logger.Info("Mistral Agent успешно обновлен для пользователя %d (новый ID: %s)", userId, umcr.AssistID, userId)
	return nil
}

// createMistralModel создаёт Mistral Agent (внутренний метод)
func (m *UniversalModel) createMistralModel(userId uint32, gptName string, modelName string, modelJSON []byte, fileIDs []Ids) (UMCR, error) {
	if m.mistralClient == nil {
		return UMCR{}, fmt.Errorf("Mistral клиент не инициализирован")
	}

	// Парсим JSON для извлечения данных модели
	var modelData UniversalModelData
	if err := json.Unmarshal(modelJSON, &modelData); err != nil {
		return UMCR{}, fmt.Errorf("ошибка при разборе JSON модели: %w", err)
	}

	if modelData.Prompt == "" {
		return UMCR{}, fmt.Errorf("поле 'prompt' отсутствует или пустое")
	}

	// Создаём агента через Mistral API с поддержкой всех возможностей
	umcr, err := m.mistralClient.createMistralAgent(&modelData, userId, fileIDs)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка создания Mistral агента: %w", err)
	}

	return umcr, nil
}

// createMistralAgent создает нового агента с указанными параметрами
// createMistralAgent создаёт Mistral Agent (внутренний метод)
func (m *MistralAgentClient) createMistralAgent(modelData *UniversalModelData, userId uint32, fileIDs []Ids) (UMCR, error) {
	if modelData == nil {
		return UMCR{}, fmt.Errorf("modelData не может быть nil")
	}

	// Убираем /completions из URL для endpoint создания агента
	baseURL := strings.Replace(m.url, "/completions", "", 1)

	description := fmt.Sprintf("Agent для пользователя %d", userId)

	// Добавляем JSON Schema в промпт для структурированного вывода
	// Mistral API не поддерживает response_format, поэтому добавляем схему в instructions
	enhancedPrompt := modelData.Prompt + "\n\n" +
		fmt.Sprintf("СИСТЕМНАЯ ИНФОРМАЦИЯ:\n"+
			"- Твой user_id: %d\n"+
			"- При вызове функций get_s3_files и create_file ВСЕГДА используй этот user_id\n"+
			"- НЕ спрашивай user_id у пользователя\n\n", userId) +
		"ВАЖНО: Твой ответ ДОЛЖЕН быть валидным JSON в следующем формате:\n" +
		MistralSchemaJSON + "\n\n" +
		"Всегда возвращай ответ строго в этом JSON формате."

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

	// Добавляем функции get_s3_files и create_file ВСЕГДА (как в OpenAI)
	tools = append(tools,
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "get_s3_files",
				"description": "Получает список доступных файлов для конкретного пользователя",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя для получения его файлов",
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
				"description": "Создает файл с указанным содержимым и сохраняет его на S3 для конкретного пользователя",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя для сохранения файла",
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
	)

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
	defer resp.Body.Close()

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

// CreateLibrary создаёт новую библиотеку документов (аналог VectorStore в OpenAI)
func (m *MistralAgentClient) CreateLibrary(name, description string) (*MistralLibrary, error) {
	const librariesURL = "https://api.mistral.ai/v1/libraries"

	payload := map[string]interface{}{
		"name": name,
	}
	if description != "" {
		payload["description"] = description
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, librariesURL, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("ошибка создания POST запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
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

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	var library MistralLibrary
	if err := json.Unmarshal(responseBody, &library); err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return &library, nil
}

// GetLibrary получает информацию о библиотеке
func (m *MistralAgentClient) GetLibrary(libraryID string) (*MistralLibrary, error) {
	url := fmt.Sprintf("https://api.mistral.ai/v1/libraries/%s", libraryID)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания GET запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)

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

	var library MistralLibrary
	if err := json.Unmarshal(responseBody, &library); err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return &library, nil
}

// ListLibraries получает список всех библиотек
func (m *MistralAgentClient) ListLibraries() ([]MistralLibrary, error) {
	const librariesURL = "https://api.mistral.ai/v1/libraries"

	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, librariesURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания GET запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)

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

	req, err := http.NewRequestWithContext(m.ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("ошибка создания DELETE запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return nil
}

// DeleteDocumentFromLibrary удаляет документ из библиотеки
// DELETE /v1/libraries/{library_id}/documents/{document_id}
func (m *MistralAgentClient) DeleteDocumentFromLibrary(libraryID, documentID string) error {
	url := fmt.Sprintf("https://api.mistral.ai/v1/libraries/%s/documents/%s", libraryID, documentID)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("ошибка создания DELETE запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return nil
}

// UploadDocumentToLibrary загружает документ в библиотеку (multipart/form-data)
func (m *MistralAgentClient) UploadDocumentToLibrary(libraryID, fileName string, fileData []byte) (*MistralDocument, error) {
	url := fmt.Sprintf("https://api.mistral.ai/v1/libraries/%s/documents", libraryID)

	// Создаём multipart форму
	body := &bytes.Buffer{}

	// Простая реализация multipart - для продакшена используйте mime/multipart
	boundary := "----MistralBoundary"

	// Записываем файл
	fmt.Fprintf(body, "--%s\r\n", boundary)
	fmt.Fprintf(body, "Content-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n", fileName)
	fmt.Fprintf(body, "Content-Type: application/octet-stream\r\n\r\n")
	body.Write(fileData)
	fmt.Fprintf(body, "\r\n--%s--\r\n", boundary)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания POST запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", fmt.Sprintf("multipart/form-data; boundary=%s", boundary))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	var document MistralDocument
	if err := json.Unmarshal(responseBody, &document); err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return &document, nil
}

// GetDocument получает информацию о документе
func (m *MistralAgentClient) GetDocument(libraryID, documentID string) (*MistralDocument, error) {
	url := fmt.Sprintf("https://api.mistral.ai/v1/libraries/%s/documents/%s", libraryID, documentID)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания GET запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)

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

	var document MistralDocument
	if err := json.Unmarshal(responseBody, &document); err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return &document, nil
}

// ListDocuments получает список документов в библиотеке
func (m *MistralAgentClient) ListDocuments(libraryID string) ([]MistralDocument, error) {
	url := fmt.Sprintf("https://api.mistral.ai/v1/libraries/%s/documents", libraryID)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания GET запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)

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

	var response struct {
		Data []MistralDocument `json:"data"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return response.Data, nil
}

// DeleteDocument удаляет документ из библиотеки
func (m *MistralAgentClient) DeleteDocument(libraryID, documentID string) error {
	url := fmt.Sprintf("https://api.mistral.ai/v1/libraries/%s/documents/%s", libraryID, documentID)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("ошибка создания DELETE запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return nil
}

// GetDocumentContent получает текстовое содержимое документа
func (m *MistralAgentClient) GetDocumentContent(libraryID, documentID string) (string, error) {
	url := fmt.Sprintf("https://api.mistral.ai/v1/libraries/%s/documents/%s/content", libraryID, documentID)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("ошибка создания GET запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)

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

	return string(responseBody), nil
}

// ============================================================================
// HIGH-LEVEL METHODS - Высокоуровневые методы для работы с документами
// ============================================================================

// CreateMistralLibraryWithFiles создаёт библиотеку и загружает в неё файлы
// Аналог создания VectorStore в OpenAI
func (m *UniversalModel) CreateMistralLibraryWithFiles(userId uint32, fileIDs []Ids) (string, error) {
	if m.mistralClient == nil {
		return "", fmt.Errorf("Mistral клиент не инициализирован")
	}

	// Создаём библиотеку
	libraryName := fmt.Sprintf("Library для пользователя %d", userId)
	library, err := m.mistralClient.CreateLibrary(libraryName, "")
	if err != nil {
		return "", fmt.Errorf("ошибка создания библиотеки: %w", err)
	}

	logger.Info("Создана библиотека Mistral: %s для пользователя %d", library.ID, userId)

	// Загружаем файлы в библиотеку (нужно получить данные файлов из хранилища)
	// TODO: реализовать загрузку файлов из вашего хранилища
	// for _, fileID := range fileIDs {
	//     fileData := getFileData(fileID.ID) // получить данные файла
	//     m.mistralClient.UploadDocumentToLibrary(library.ID, fileID.Name, fileData)
	// }

	return library.ID, nil
}

// AddFileToMistralLibrary добавляет файл в существующую библиотеку
func (m *UniversalModel) AddFileToMistralLibrary(userId uint32, libraryID, fileName string, fileData []byte) (*MistralDocument, error) {
	if m.mistralClient == nil {
		return nil, fmt.Errorf("Mistral клиент не инициализирован")
	}

	document, err := m.mistralClient.UploadDocumentToLibrary(libraryID, fileName, fileData)
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки документа: %w", err)
	}

	logger.Info("Файл %s успешно добавлен в библиотеку %s для пользователя %d", fileName, libraryID, userId)
	return document, nil
}

// DeleteMistralLibrary удаляет библиотеку со всеми документами
func (m *UniversalModel) DeleteMistralLibrary(userId uint32, libraryID string) error {
	if m.mistralClient == nil {
		return fmt.Errorf("Mistral клиент не инициализирован")
	}

	err := m.mistralClient.DeleteLibrary(libraryID)
	if err != nil {
		return fmt.Errorf("ошибка удаления библиотеки: %w", err)
	}

	logger.Info("Библиотека %s удалена для пользователя %d", libraryID, userId)
	return nil
}

// GetMistralLibraryDocuments получает список документов в библиотеке
func (m *UniversalModel) GetMistralLibraryDocuments(userId uint32, libraryID string) ([]MistralDocument, error) {
	if m.mistralClient == nil {
		return nil, fmt.Errorf("Mistral клиент не инициализирован")
	}

	documents, err := m.mistralClient.ListDocuments(libraryID)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения списка документов: %w", err)
	}

	return documents, nil
}
