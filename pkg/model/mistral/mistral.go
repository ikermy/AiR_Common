package mistral

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/ikermy/AiR_Common/pkg/mode"
)

// MistralAgentClient - обертка для работы с агентами и обычными моделями
type MistralAgentClient struct {
	ctx         context.Context
	cancel      context.CancelFunc
	apiKey      string
	url         string
	keyResolver func(userID uint32) string // Резолвер персональных ключей; nil → глобальный apiKey
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

// HasAPIKey возвращает true если для пользователя есть действующий API-ключ.
// Используется для ранней проверки перед выполнением запросов.
func (m *MistralAgentClient) HasAPIKey(userID uint32) bool {
	return m.resolveKey(userID) != ""
}

// NewMistralAgentClient создает новый клиент с поддержкой агентов
func NewMistralAgentClient(parent context.Context) *MistralAgentClient {
	ctx, cancel := context.WithCancel(parent)

	return &MistralAgentClient{
		ctx:    ctx,
		cancel: cancel,
		apiKey: "",
		url:    mode.MistralAgentsURL,
	}
}

// TokenUsage представляет информацию о расходе токенов
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type Response struct {
	Message         string
	FuncName        string
	FuncArgs        string
	ToolCallID      string // ID вызова функции для отправки результата
	HasFunc         bool
	GeneratedImages []GeneratedImage // Сгенерированные изображения
	Usage           *TokenUsage      // Информация о расходе токенов
}

// GeneratedImage представляет сгенерированное изображение
type GeneratedImage struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	FileType string `json:"file_type"` // png, jpg, etc.
}

// Shutdown корректно завершает работу клиента
func (m *MistralAgentClient) Shutdown() {
	if m.cancel != nil {
		m.cancel()
	}
}

// ============================================================================
// LIBRARY MANAGEMENT - Методы для работы с библиотеками документов
// Документация: https://docs.mistral.ai/agents/tools/built-in/document_library
// ============================================================================

// MistralLibrary представляет библиотеку документов
type MistralLibrary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"` // API возвращает строку, а не int64
}

// MistralDocument представляет документ в библиотеке
type MistralDocument struct {
	ID        string `json:"id"`
	FileName  string `json:"file_name"`
	Status    string `json:"status,omitempty"`     // processing, processed, failed
	CreatedAt string `json:"created_at,omitempty"` // API возвращает строку, а не int64
}

// CreateLibrary создаёт новую библиотеку документов
// POST /v1/libraries
func (m *MistralAgentClient) CreateLibrary(name, description string) (*MistralLibrary, error) {
	const librariesURL = "https://api.mistral.ai/v1/libraries"

	payload := map[string]any{
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
	defer func() { _ = resp.Body.Close() }()

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

// DeleteLibrary удаляет библиотеку
// DELETE /v1/libraries/{library_id}
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
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return nil
}

// UploadDocumentToLibrary загружает документ в библиотеку
// POST /v1/libraries/{library_id}/documents
func (m *MistralAgentClient) UploadDocumentToLibrary(libraryID, fileName string, fileData []byte) (string, error) {
	url := fmt.Sprintf("https://api.mistral.ai/v1/libraries/%s/documents", libraryID)

	// Создаём multipart форму
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)

	part, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		return "", fmt.Errorf("ошибка создания multipart поля: %v", err)
	}
	if _, err = part.Write(fileData); err != nil {
		return "", fmt.Errorf("ошибка записи файла в multipart: %v", err)
	}
	if err = mw.Close(); err != nil {
		return "", fmt.Errorf("ошибка закрытия multipart writer: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, url, body)
	if err != nil {
		return "", fmt.Errorf("ошибка создания POST запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	var document MistralDocument
	if err := json.Unmarshal(responseBody, &document); err != nil {
		return "", fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return document.ID, nil
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
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return nil
}

// GetDocumentStatus получает статус документа
// GET /v1/libraries/{library_id}/documents/{document_id}
func (m *MistralAgentClient) GetDocumentStatus(libraryID, documentID string) (string, error) {
	url := fmt.Sprintf("https://api.mistral.ai/v1/libraries/%s/documents/%s", libraryID, documentID)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("ошибка создания GET запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	var document MistralDocument
	if err := json.Unmarshal(responseBody, &document); err != nil {
		return "", fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return document.Status, nil
}

// DownloadFile скачивает файл (изображение) по file_id через Mistral Files API
// Документация: https://docs.mistral.ai/api/#tag/files/operation/files_api_routes_download_file
func (m *MistralAgentClient) DownloadFile(fileID string) ([]byte, error) {
	url := fmt.Sprintf("https://api.mistral.ai/v1/files/%s/content", fileID)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания GET запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyText, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(bodyText))
	}

	fileBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения файла: %v", err)
	}

	return fileBytes, nil
}

// ConversationResponse представляет ответ от Conversations API
type ConversationResponse struct {
	ConversationID string               `json:"conversation_id"`
	Outputs        []ConversationOutput `json:"outputs"`
	Usage          *TokenUsage          `json:"usage,omitempty"` // Информация о токенах
}

// ConversationOutput представляет один output в ответе
type ConversationOutput struct {
	Type      string          `json:"type"`       // "message.output" или "function.call"
	Content   json.RawMessage `json:"content"`    // Может быть строкой или массивом объектов
	CreatedAt string          `json:"created_at"` // API возвращает строку, а не int64
	// Поля для function.call
	ToolCallID string `json:"tool_call_id,omitempty"` // ID вызова функции (для связи с результатом)
	Name       string `json:"name,omitempty"`         // Имя функции (только для type="function.call")
	Arguments  string `json:"arguments,omitempty"`    // Аргументы функции как JSON строка (только для type="function.call")
}

// ConversationContent представляет контент в output (text или tool_file)
type ConversationContent struct {
	Type     string `json:"type"`      // "text" или "tool_file"
	Text     string `json:"text"`      // для type="text"
	FileID   string `json:"file_id"`   // для type="tool_file"
	FileName string `json:"file_name"` // для type="tool_file"
	FileType string `json:"file_type"` // для type="tool_file"
	Tool     string `json:"tool"`      // для type="tool_file" (например "image_generation")
}

// StartConversation начинает новый диалог с агентом через Conversations API
// Документация: https://docs.mistral.ai/api/#tag/conversations
func (m *MistralAgentClient) StartConversation(agentID string, inputs any, userID uint32) (ConversationResponse, error) {
	conversationsURL := mode.MistralConversationsURL

	// Формат payload согласно документации:
	// inputs может быть строкой или массивом объектов с полями role, content, object, type
	payload := map[string]any{
		"agent_id": agentID,
		"inputs":   inputs,
		"stream":   false,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, conversationsURL, bytes.NewBuffer(body))
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.resolveKey(userID))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка чтения тела ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return ConversationResponse{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	// RAW ответ для отладки
	//logger.Debug("StartConversation: сырой ответ от API: %s", string(responseBody))

	var result ConversationResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return result, nil
}

// ContinueConversation продолжает существующий диалог через Conversations API
func (m *MistralAgentClient) ContinueConversation(conversationID string, inputs any, userID uint32) (ConversationResponse, error) {
	conversationsURL := fmt.Sprintf("%s/%s", mode.MistralConversationsURL, conversationID)

	payload := map[string]any{
		"inputs": inputs,
		"stream": false,
		"store":  true,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, conversationsURL, bytes.NewBuffer(body))
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.resolveKey(userID))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка чтения тела ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return ConversationResponse{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	// RAW ответ для отладки
	//logger.Debug("ContinueConversation: сырой ответ от API: %s", string(responseBody))

	var result ConversationResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return result, nil
}

// SendFunctionResult отправляет результат функции в conversation
// Согласно документации Mistral Conversations API
func (m *MistralAgentClient) SendFunctionResult(conversationID string, toolCallID string, functionResult string, userID uint32) (ConversationResponse, error) {
	conversationsURL := fmt.Sprintf("%s/%s", mode.MistralConversationsURL, conversationID)

	inputs := []map[string]any{
		{
			"tool_call_id": toolCallID,
			"result":       functionResult,
			"object":       "entry",
			"type":         "function.result",
		},
	}

	payload := map[string]any{
		"inputs":            inputs,
		"stream":            false,
		"store":             true,
		"handoff_execution": "server",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, conversationsURL, bytes.NewBuffer(body))
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.resolveKey(userID))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка чтения тела ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return ConversationResponse{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	//logger.Debug("SendFunctionResult: сырой ответ от API: %s", string(responseBody))

	var result ConversationResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	//logger.Debug("SendFunctionResult: результат функции для tool_call_id=%s отправлен, conversation_id=%s", toolCallID, result.ConversationID)
	return result, nil
}

// ============================================================================
// STREAMING METHODS - Методы для работы в режиме Server-Sent Events (SSE)
// ============================================================================

// PatchAgent обновляет конфигурацию Mistral Agent через PATCH /v1/agents/{agent_id}.
// Используется для синхронизации инструментов (tools) с текущим набором MCP-функций.
func (m *MistralAgentClient) PatchAgent(agentID string, tools []map[string]any) error {
	patchURL := fmt.Sprintf("%s/%s", mode.MistralAgentsBaseURL, agentID)

	payload := map[string]any{
		"tools": tools,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ошибка сериализации PATCH запроса: %w", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPatch, patchURL, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("ошибка создания PATCH запроса: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка HTTP запроса PATCH agent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyText, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PATCH agent вернул статус %d: %s", resp.StatusCode, string(bodyText))
	}

	return nil
}

// StartConversationStreaming начинает новый диалог с агентом в streaming режиме
// onDelta вызывается для каждого delta события с текстом или JSON событиями function calls
// Возвращает ConversationResponse с накопленными данными и usage токенов
func (m *MistralAgentClient) StartConversationStreaming(agentID string, inputs any, onDelta func(string) error, userID uint32) (ConversationResponse, error) {
	conversationsURL := mode.MistralConversationsURL

	payload := map[string]any{
		"agent_id":          agentID,
		"inputs":            inputs,
		"stream":            true,
		"handoff_execution": "client", // Клиент выполняет function tools — Mistral возвращает function.call события
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, conversationsURL, bytes.NewBuffer(body))
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.resolveKey(userID))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream") // SSE

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyText, _ := io.ReadAll(resp.Body)
		return ConversationResponse{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(bodyText))
	}

	// Читаем SSE поток
	return m.readStreamingResponse(resp.Body, onDelta)
}

// ContinueConversationStreaming продолжает диалог в streaming режиме
func (m *MistralAgentClient) ContinueConversationStreaming(conversationID string, inputs any, onDelta func(string) error, userID uint32) (ConversationResponse, error) {
	conversationsURL := fmt.Sprintf("%s/%s", mode.MistralConversationsURL, conversationID)

	payload := map[string]any{
		"inputs":            inputs,
		"stream":            true,
		"store":             true,
		"handoff_execution": "client", // Клиент выполняет function tools — Mistral возвращает function.call события
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, conversationsURL, bytes.NewBuffer(body))
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.resolveKey(userID))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyText, _ := io.ReadAll(resp.Body)
		return ConversationResponse{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(bodyText))
	}

	return m.readStreamingResponse(resp.Body, onDelta)
}

// SendMultipleFunctionResultsStreaming отправляет результаты НЕСКОЛЬКИХ функций в streaming режиме
// functionResults - массив объектов с полями: tool_call_id, result, object, type
func (m *MistralAgentClient) SendMultipleFunctionResultsStreaming(conversationID string, functionResults []map[string]any, onDelta func(string) error, userID uint32) (ConversationResponse, error) {
	conversationsURL := fmt.Sprintf("%s/%s", mode.MistralConversationsURL, conversationID)

	payload := map[string]any{
		"inputs": functionResults,
		"stream": true,
		"store":  true,
		// handoff_execution не указываем: мы уже выполнили функции на клиенте и возвращаем результаты
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	//logger.Debug("SendMultipleFunctionResultsStreaming: отправка %d результатов функций для conversation=%s",
	//	len(functionResults), conversationID)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, conversationsURL, bytes.NewBuffer(body))
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.resolveKey(userID))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyText, _ := io.ReadAll(resp.Body)
		return ConversationResponse{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(bodyText))
	}

	return m.readStreamingResponse(resp.Body, onDelta)
}

// readStreamingResponse читает SSE поток и обрабатывает события
// Mistral API формат отличается от OpenAI - события приходят как JSON объекты без явного type
func (m *MistralAgentClient) readStreamingResponse(body io.Reader, onDelta func(string) error) (ConversationResponse, error) {
	scanner := bufio.NewScanner(body)
	var result ConversationResponse
	var outputs []ConversationOutput
	var textParts []string
	var usageData *TokenUsage
	var conversationID string

	// Для накопления function call arguments (по tool_call_id)
	functionCallsMap := make(map[string]*ConversationOutput)

	for scanner.Scan() {
		line := scanner.Text()

		// SSE формат: "data: {...json...}"
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		// Пропускаем "[DONE]" маркер
		if data == "[DONE]" {
			break
		}

		// Парсим JSON событие
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			//logger.Warn("readStreamingResponse: ошибка парсинга SSE события: %v, data: %s", err, data)
			continue
		}

		// Извлекаем тип события
		eventType, _ := event["type"].(string)

		// Обрабатываем различные типы событий Mistral API
		switch eventType {
		case "message.output.delta":
			// Текстовая delta - извлекаем из поля "content"
			if content, ok := event["content"].(string); ok && content != "" {
				textParts = append(textParts, content)
				if onDelta != nil {
					if err := onDelta(content); err != nil {
						//logger.Warn("readStreamingResponse: ошибка в onDelta callback: %v", err)
					}
				}
			}

		case "conversation.response.started":
			// Начало ответа - извлекаем conversation_id
			if convID, ok := event["conversation_id"].(string); ok {
				conversationID = convID
			}

		case "conversation.response.done":
			// Завершение ответа - извлекаем usage
			if usage, ok := event["usage"].(map[string]any); ok {
				usageData = &TokenUsage{}
				if pt, ok := usage["prompt_tokens"].(float64); ok {
					usageData.PromptTokens = int(pt)
				}
				if ct, ok := usage["completion_tokens"].(float64); ok {
					usageData.CompletionTokens = int(ct)
				}
				if tt, ok := usage["total_tokens"].(float64); ok {
					usageData.TotalTokens = int(tt)
				}
			}

		case "conversation.response.error":
			// Ошибка во время обработки ответа
			errorMsg := "unknown error"
			if msg, ok := event["message"].(string); ok {
				errorMsg = msg
			}
			if code, ok := event["code"].(float64); ok {
				errorMsg = fmt.Sprintf("%s (code: %.0f)", errorMsg, code)
			}
			//logger.Warn("readStreamingResponse: conversation.response.error - %s", errorMsg)
			return ConversationResponse{}, fmt.Errorf("conversation error: %s", errorMsg)

		case "function.call.delta":
			// Накопление аргументов вызова функции (приходят по частям)
			toolCallID, _ := event["tool_call_id"].(string)
			if toolCallID == "" {
				continue
			}

			// Получаем или создаем функцию в map
			funcCall, exists := functionCallsMap[toolCallID]
			if !exists {
				funcCall = &ConversationOutput{
					Type:       "function.call",
					ToolCallID: toolCallID,
					Arguments:  "",
				}
				// Извлекаем name если есть
				if name, ok := event["name"].(string); ok {
					funcCall.Name = name
				}
				functionCallsMap[toolCallID] = funcCall
			}

			// Накапливаем аргументы
			if argsDelta, ok := event["arguments"].(string); ok && argsDelta != "" {
				funcCall.Arguments += argsDelta
			}

		case "function.call":
			// Вызов функции (если поддерживается в streaming)
			var output ConversationOutput
			output.Type = "function.call"

			if name, ok := event["name"].(string); ok {
				output.Name = name
			}
			if toolCallID, ok := event["tool_call_id"].(string); ok {
				output.ToolCallID = toolCallID
			}
			if args, ok := event["arguments"].(string); ok {
				output.Arguments = args
			}

			outputs = append(outputs, output)

			// Отправляем событие вызова функции через callback
			if onDelta != nil {
				eventJSON, _ := json.Marshal(map[string]any{
					"type":         "function_call",
					"name":         output.Name,
					"tool_call_id": output.ToolCallID,
					"arguments":    output.Arguments,
				})
				if err := onDelta(string(eventJSON)); err != nil {
					//logger.Warn("readStreamingResponse: ошибка отправки function_call события: %v", err)
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка чтения SSE потока: %w", err)
	}

	// Добавляем накопленные function calls в outputs
	for _, funcCall := range functionCallsMap {
		outputs = append(outputs, *funcCall)

		// Отправляем событие вызова функции через callback
		if onDelta != nil {
			eventJSON, _ := json.Marshal(map[string]any{
				"type":         "function_call",
				"name":         funcCall.Name,
				"tool_call_id": funcCall.ToolCallID,
				"arguments":    funcCall.Arguments,
			})
			if err := onDelta(string(eventJSON)); err != nil {
				//logger.Warn("readStreamingResponse: ошибка отправки function_call события: %v", err)
			}
		}
	}

	// Если outputs пустой, но есть текст - формируем output из накопленного текста
	if len(outputs) == 0 && len(textParts) > 0 {
		textContent := strings.Join(textParts, "")
		contentJSON, _ := json.Marshal(textContent)
		outputs = append(outputs, ConversationOutput{
			Type:    "message.output",
			Content: contentJSON,
		})
	}

	// Формируем финальный результат
	result.Outputs = outputs
	result.Usage = usageData // Сохраняем информацию о токенах (отправку делает RequestStreaming один раз)
	if conversationID != "" {
		result.ConversationID = conversationID
	}

	return result, nil
}

// DeleteFile удаляет файл из Mistral Files API по его ID
// Документация: https://docs.mistral.ai/api/#tag/files/operation/files_api_routes_delete_file
func (m *MistralAgentClient) DeleteFile(fileID string) error {
	if fileID == "" {
		return fmt.Errorf("fileID не может быть пустым")
	}

	// Формируем DELETE запрос
	req, err := http.NewRequestWithContext(m.ctx, http.MethodDelete, fmt.Sprintf("https://api.mistral.ai/v1/files/%s", fileID), nil)
	if err != nil {
		return fmt.Errorf("ошибка создания HTTP запроса: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка HTTP запроса: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			//logger.Error("DeleteFile: ошибка закрытия response body: %v", err)
		}
	}()

	// Проверяем статус ответа
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return nil
}

// ParseConversationResponse преобразует ConversationResponse в Response для совместимости
func ParseConversationResponse(convResp ConversationResponse) Response {
	if len(convResp.Outputs) == 0 {
		return Response{Usage: convResp.Usage} // Хотя бы Usage передаем
	}

	// Берём последний output (самый свежий ответ)
	lastOutput := convResp.Outputs[len(convResp.Outputs)-1]

	// Проверяем тип output
	if lastOutput.Type == "function.call" {
		// Это вызов функции - name и arguments уже распарсены в структуре
		if lastOutput.Name != "" {
			//logger.Debug("ParseConversationResponse: обнаружен вызов функции %s с аргументами: %s, tool_call_id: %s", lastOutput.Name, lastOutput.Arguments, lastOutput.ToolCallID)
			return Response{
				Message:    "",
				FuncName:   lastOutput.Name,
				FuncArgs:   lastOutput.Arguments,
				ToolCallID: lastOutput.ToolCallID, // Сохраняем для отправки результата
				HasFunc:    true,
				Usage:      convResp.Usage, // Передаем информацию о токенах
			}
		}

		//logger.Warn("ParseConversationResponse: type=function.call, но поле name пустое")
		return Response{}
	}

	var textParts []string
	var generatedImages []GeneratedImage

	// Content может быть строкой или массивом - пробуем оба варианта
	// Сначала пробуем распарсить как строку
	var contentStr string
	if err := json.Unmarshal(lastOutput.Content, &contentStr); err == nil {
		// Это строка
		textParts = append(textParts, contentStr)
	} else {
		// Пробуем распарсить как массив объектов ConversationContent
		var contentArray []ConversationContent
		if err := json.Unmarshal(lastOutput.Content, &contentArray); err == nil {
			// Это массив
			for _, content := range contentArray {
				switch content.Type {
				case "text":
					if content.Text != "" {
						textParts = append(textParts, content.Text)
					}
				case "tool_file":
					// Это сгенерированное изображение
					if content.FileID != "" {
						generatedImages = append(generatedImages, GeneratedImage{
							FileID:   content.FileID,
							FileName: content.FileName,
							FileType: content.FileType,
						})
						//logger.Debug("ParseConversationResponse: обнаружено изображение file_id=%s, tool=%s", content.FileID, content.Tool)
					}
				}
			}
		} else {
			// Content может быть пустым для function.call
			if lastOutput.Type != "function.call" {
				//logger.Warn("ParseConversationResponse: не удалось распарсить content ни как строку, ни как массив: %v", err)
			}
		}
	}

	message := strings.Join(textParts, "\n")

	return Response{
		Message:         message,
		HasFunc:         false,
		GeneratedImages: generatedImages,
		Usage:           convResp.Usage, // Передаем информацию о токенах
	}
}
