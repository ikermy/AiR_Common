package mistral

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
)

// MistralMessage представляет сообщение для Mistral API
type MistralMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// MistralAgentClient - обертка для работы с агентами и обычными моделями
type MistralAgentClient struct {
	ctx    context.Context
	cancel context.CancelFunc
	apiKey string
	url    string
}

// NewMistralAgentClient создает новый клиент с поддержкой агентов
func NewMistralAgentClient(parent context.Context, conf *conf.Conf) *MistralAgentClient {
	ctx, cancel := context.WithCancel(parent)

	return &MistralAgentClient{
		ctx:    ctx,
		cancel: cancel,
		apiKey: conf.GPT.MistralKey,
		url:    mode.MistralAgentsURL,
	}
}

type Response struct {
	Message         string
	FuncName        string
	FuncArgs        string
	ToolCallID      string // ID вызова функции для отправки результата
	HasFunc         bool
	GeneratedImages []GeneratedImage // Сгенерированные изображения
}

// GeneratedImage представляет сгенерированное изображение
type GeneratedImage struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	FileType string `json:"file_type"` // png, jpg, etc.
}

// ChatWithModel отправляет массив сообщений обычной модели через HTTP API
func (m *MistralAgentClient) ChatWithModel(model string, messages []MistralMessage) (string, error) {
	payload := map[string]interface{}{
		"model":    model,
		"messages": messages,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, m.url, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
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

	var response map[string]interface{}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return "", fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				if content, ok := message["content"].(string); ok {
					return content, nil
				}
			}
		}
	}

	return "", fmt.Errorf("пустой ответ от модели")
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
	defer resp.Body.Close()

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
	boundary := "----MistralBoundary"

	// Записываем файл
	fmt.Fprintf(body, "--%s\r\n", boundary)
	fmt.Fprintf(body, "Content-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\n", fileName)
	fmt.Fprintf(body, "Content-Type: application/octet-stream\r\n\r\n")
	body.Write(fileData)
	fmt.Fprintf(body, "\r\n--%s--\r\n", boundary)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, url, body)
	if err != nil {
		return "", fmt.Errorf("ошибка создания POST запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", fmt.Sprintf("multipart/form-data; boundary=%s", boundary))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

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
	defer resp.Body.Close()

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
	defer resp.Body.Close()

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
	defer resp.Body.Close()

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
func (m *MistralAgentClient) StartConversation(agentID string, inputs interface{}) (ConversationResponse, error) {
	conversationsURL := "https://api.mistral.ai/v1/conversations"

	// Формат payload согласно документации:
	// inputs может быть строкой или массивом объектов с полями role, content, object, type
	payload := map[string]interface{}{
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

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody := new(bytes.Buffer)
	responseBody.ReadFrom(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return ConversationResponse{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, responseBody.String())
	}

	// RAW ответ для отладки
	//logger.Debug("StartConversation: сырой ответ от API: %s", responseBody.String())

	var result ConversationResponse
	if err := json.Unmarshal(responseBody.Bytes(), &result); err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return result, nil
}

// ContinueConversation продолжает существующий диалог через Conversations API
func (m *MistralAgentClient) ContinueConversation(conversationID string, inputs interface{}) (ConversationResponse, error) {
	conversationsURL := fmt.Sprintf("https://api.mistral.ai/v1/conversations/%s", conversationID)

	payload := map[string]interface{}{
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

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody := new(bytes.Buffer)
	responseBody.ReadFrom(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return ConversationResponse{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, responseBody.String())
	}

	// RAW ответ для отладки
	//logger.Debug("ContinueConversation: сырой ответ от API: %s", responseBody.String())

	var result ConversationResponse
	if err := json.Unmarshal(responseBody.Bytes(), &result); err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	return result, nil
}

// SendFunctionResult отправляет результат функции в conversation
// Согласно документации Mistral Conversations API
func (m *MistralAgentClient) SendFunctionResult(conversationID string, toolCallID string, functionResult string) (ConversationResponse, error) {
	conversationsURL := fmt.Sprintf("https://api.mistral.ai/v1/conversations/%s", conversationID)

	inputs := []map[string]interface{}{
		{
			"tool_call_id": toolCallID,
			"result":       functionResult,
			"object":       "entry",
			"type":         "function.result",
		},
	}

	payload := map[string]interface{}{
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

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody := new(bytes.Buffer)
	responseBody.ReadFrom(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return ConversationResponse{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, responseBody.String())
	}

	//logger.Debug("SendFunctionResult: сырой ответ от API: %s", responseBody.String())

	var result ConversationResponse
	if err := json.Unmarshal(responseBody.Bytes(), &result); err != nil {
		return ConversationResponse{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	//logger.Debug("SendFunctionResult: результат функции для tool_call_id=%s отправлен, conversation_id=%s", toolCallID, result.ConversationID)
	return result, nil
}

// FileUploadResponse представляет ответ от Files API при загрузке файла
type FileUploadResponse struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Purpose  string `json:"purpose"`
	Filename string `json:"filename"`
	Bytes    int    `json:"bytes"`
}

// UploadFile загружает файл в Mistral Files API для временного использования
// Документация: https://docs.mistral.ai/api/#tag/files/operation/files_api_routes_upload_file
func (m *MistralAgentClient) UploadFile(fileName string, fileData []byte) (*FileUploadResponse, error) {
	// Создаём multipart request
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)

	// Определяем purpose на основе расширения файла
	// audio - для аудио файлов (mp3, ogg, wav, m4a, etc)
	// chat - для документов и изображений по умолчанию
	purpose := "chat"
	lowerFileName := strings.ToLower(fileName)
	if strings.HasSuffix(lowerFileName, ".mp3") ||
		strings.HasSuffix(lowerFileName, ".ogg") ||
		strings.HasSuffix(lowerFileName, ".wav") ||
		strings.HasSuffix(lowerFileName, ".m4a") ||
		strings.HasSuffix(lowerFileName, ".flac") ||
		strings.HasSuffix(lowerFileName, ".opus") {
		purpose = "audio"
	}

	// Добавляем purpose
	if err := writer.WriteField("purpose", purpose); err != nil {
		return nil, fmt.Errorf("ошибка добавления поля purpose: %w", err)
	}

	// Добавляем файл
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания form file: %w", err)
	}

	if _, err := part.Write(fileData); err != nil {
		return nil, fmt.Errorf("ошибка записи данных файла: %w", err)
	}

	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("ошибка закрытия writer: %w", err)
	}

	// Отправляем запрос
	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, "https://api.mistral.ai/v1/files", &requestBody)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания HTTP запроса: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка HTTP запроса: %w", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения ответа: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	var result FileUploadResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %w", err)
	}

	return &result, nil
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
			logger.Error("DeleteFile: ошибка закрытия response body: %v", err)
		}
	}()

	// Проверяем статус ответа
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != 204 {
		responseBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return nil
}

// ParseConversationResponse преобразует ConversationResponse в Response для совместимости
func ParseConversationResponse(convResp ConversationResponse) Response {
	if len(convResp.Outputs) == 0 {
		return Response{}
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
			}
		}

		logger.Warn("ParseConversationResponse: type=function.call, но поле name пустое")
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
				logger.Warn("ParseConversationResponse: не удалось распарсить content ни как строку, ни как массив: %v", err)
			}
		}
	}

	message := strings.Join(textParts, "\n")

	return Response{
		Message:         message,
		HasFunc:         false,
		GeneratedImages: generatedImages,
	}
}
