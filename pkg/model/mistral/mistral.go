package mistral

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ikermy/AiR_Common/pkg/conf"
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
	Message  string
	FuncName string
	FuncArgs string
	HasFunc  bool
}

// ChatWithAgent отправляет массив сообщений конкретному агенту с указанными инструментами
// libraryIds - опциональный список ID библиотек для document_library tool
func (m *MistralAgentClient) ChatWithAgent(agentID string, messages []MistralMessage, tools interface{}, libraryIds ...string) (Response, error) {
	payload := map[string]interface{}{
		"agent_id": agentID,
		"messages": messages,
	}

	// Добавляем инструменты если они переданы
	if tools != nil {
		payload["tools"] = tools
	}

	// Добавляем library_ids если есть (для document_library tool)
	if len(libraryIds) > 0 {
		payload["library_ids"] = libraryIds
	}

	// Добавляем response_format для структурированных ответов
	// Примечание: формат может быть установлен при создании агента,
	// но также можно передавать в каждом запросе для гибкости
	payload["response_format"] = map[string]interface{}{
		"type": "json_object",
	}

	body, _ := json.Marshal(payload)

	req, _ := http.NewRequestWithContext(m.ctx, http.MethodPost, m.url, bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody := new(bytes.Buffer)
	responseBody.ReadFrom(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, responseBody.String())
	}

	var response map[string]interface{}
	if err := json.Unmarshal(responseBody.Bytes(), &response); err != nil {
		return Response{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := choice["message"].(map[string]interface{}); ok {
				// Проверяем tool_calls (структурированный вызов функции)
				if toolCalls, ok := msg["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
					if toolCall, ok := toolCalls[0].(map[string]interface{}); ok {
						if function, ok := toolCall["function"].(map[string]interface{}); ok {
							name, _ := function["name"].(string)
							args, _ := function["arguments"].(string)

							return Response{
								Message:  "", // Для вызовов функций сообщение пустое
								FuncName: name,
								FuncArgs: args,
								HasFunc:  true,
							}, nil
						}
					}
				}

				// Обычный текстовый ответ - content как массив объектов
				if contentArray, ok := msg["content"].([]interface{}); ok {
					var fullText strings.Builder
					for _, item := range contentArray {
						if contentObj, ok := item.(map[string]interface{}); ok {
							if text, exists := contentObj["text"].(string); exists && text != "" {
								fullText.WriteString(text)
							}
						}
					}

					// В методе ChatWithAgent в mistral.go, в блоке обработки текста:
					fullMessage := fullText.String()
					if fullMessage != "" {
						// Проверяем, содержит ли текст JSON вызов функции lead_target
						if strings.Contains(fullMessage, `"target"`) && strings.Contains(fullMessage, "true") {
							// Очищаем сообщение от JSON части
							lines := strings.Split(fullMessage, "\n")
							var cleanLines []string
							for _, line := range lines {
								trimmed := strings.TrimSpace(line)
								// Пропускаем строки с JSON
								if !strings.Contains(trimmed, `"target"`) && !strings.HasPrefix(trimmed, "{") && !strings.HasSuffix(trimmed, "}") {
									cleanLines = append(cleanLines, line)
								}
							}
							cleanMessage := strings.TrimSpace(strings.Join(cleanLines, "\n"))

							return Response{
								Message:  cleanMessage,
								FuncName: "lead_target",
								FuncArgs: `{"target": true}`,
								HasFunc:  true,
							}, nil
						}
						return Response{Message: fullMessage, HasFunc: false}, nil
					}
				}

				// Обычный текстовый ответ - content как строка (fallback)
				if content, ok := msg["content"].(string); ok && content != "" {
					// Проверяем наличие XML тегов функции
					if strings.Contains(content, "<function>") && strings.Contains(content, "</function>") {
						// Извлекаем содержимое между тегами
						start := strings.Index(content, "<function>") + len("<function>")
						end := strings.Index(content, "</function>")

						if start > len("<function>") && end > start {
							// Очищаем сообщение от XML тегов функции
							cleanMessage := strings.ReplaceAll(content, content[strings.Index(content, "<function>"):end+len("</function>")], "")
							cleanMessage = strings.TrimSpace(cleanMessage)

							return Response{
								Message:  cleanMessage,
								FuncName: "lead_target",
								FuncArgs: `{"target": true}`,
								HasFunc:  true,
							}, nil
						}
					}

					return Response{Message: content, HasFunc: false}, nil
				}
			}
		}
	}

	return Response{}, fmt.Errorf("не удалось извлечь ответ от агента")
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

// ChatWithAgentFiles отправляет массив сообщений агенту с временной загрузкой файлов
// Файлы передаются в теле запроса через multipart/form-data
// libraryIds - опциональный список ID библиотек для document_library tool
// Документация: https://docs.mistral.ai/agents/tools/built-in/document_library
func (m *MistralAgentClient) ChatWithAgentFiles(agentID string, messages []MistralMessage, files []io.Reader, fileNames []string, tools interface{}, libraryIds ...string) (Response, error) {
	if len(files) == 0 || len(files) != len(fileNames) {
		// Если файлов нет или массивы не совпадают, используем обычный запрос
		return m.ChatWithAgent(agentID, messages, tools, libraryIds...)
	}

	// Создаём multipart форму
	body := &bytes.Buffer{}
	boundary := "----MistralFormBoundary"

	// Добавляем agent_id
	fmt.Fprintf(body, "--%s\r\n", boundary)
	fmt.Fprintf(body, "Content-Disposition: form-data; name=\"agent_id\"\r\n\r\n")
	fmt.Fprintf(body, "%s\r\n", agentID)

	// Добавляем messages как JSON
	messagesJSON, err := json.Marshal(messages)
	if err != nil {
		return Response{}, fmt.Errorf("ошибка сериализации сообщений: %v", err)
	}
	fmt.Fprintf(body, "--%s\r\n", boundary)
	fmt.Fprintf(body, "Content-Disposition: form-data; name=\"messages\"\r\n")
	fmt.Fprintf(body, "Content-Type: application/json\r\n\r\n")
	body.Write(messagesJSON)
	fmt.Fprintf(body, "\r\n")

	// Добавляем tools если есть
	if tools != nil {
		toolsJSON, err := json.Marshal(tools)
		if err == nil {
			fmt.Fprintf(body, "--%s\r\n", boundary)
			fmt.Fprintf(body, "Content-Disposition: form-data; name=\"tools\"\r\n")
			fmt.Fprintf(body, "Content-Type: application/json\r\n\r\n")
			body.Write(toolsJSON)
			fmt.Fprintf(body, "\r\n")
		}
	}

	// Добавляем library_ids если есть
	if len(libraryIds) > 0 {
		libraryIdsJSON, err := json.Marshal(libraryIds)
		if err == nil {
			fmt.Fprintf(body, "--%s\r\n", boundary)
			fmt.Fprintf(body, "Content-Disposition: form-data; name=\"library_ids\"\r\n")
			fmt.Fprintf(body, "Content-Type: application/json\r\n\r\n")
			body.Write(libraryIdsJSON)
			fmt.Fprintf(body, "\r\n")
		}
	}

	// Добавляем файлы
	for i, file := range files {
		fmt.Fprintf(body, "--%s\r\n", boundary)
		fmt.Fprintf(body, "Content-Disposition: form-data; name=\"files\"; filename=\"%s\"\r\n", fileNames[i])
		fmt.Fprintf(body, "Content-Type: application/octet-stream\r\n\r\n")

		// Копируем содержимое файла
		if _, err := io.Copy(body, file); err != nil {
			return Response{}, fmt.Errorf("ошибка копирования файла %s: %v", fileNames[i], err)
		}
		fmt.Fprintf(body, "\r\n")
	}

	// Закрываем multipart
	fmt.Fprintf(body, "--%s--\r\n", boundary)

	// Создаём запрос
	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, m.url, body)
	if err != nil {
		return Response{}, fmt.Errorf("ошибка создания запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", fmt.Sprintf("multipart/form-data; boundary=%s", boundary))

	// Выполняем запрос
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody := new(bytes.Buffer)
	responseBody.ReadFrom(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, responseBody.String())
	}

	var response map[string]interface{}
	if err := json.Unmarshal(responseBody.Bytes(), &response); err != nil {
		return Response{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	// Парсим ответ (используем ту же логику что и в ChatWithAgent)
	if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := choice["message"].(map[string]interface{}); ok {
				// Проверяем tool_calls
				if toolCalls, ok := msg["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
					if toolCall, ok := toolCalls[0].(map[string]interface{}); ok {
						if function, ok := toolCall["function"].(map[string]interface{}); ok {
							name, _ := function["name"].(string)
							args, _ := function["arguments"].(string)

							return Response{
								Message:  "",
								FuncName: name,
								FuncArgs: args,
								HasFunc:  true,
							}, nil
						}
					}
				}

				// Обычный текстовый ответ
				if contentArray, ok := msg["content"].([]interface{}); ok {
					var fullText strings.Builder
					for _, item := range contentArray {
						if contentObj, ok := item.(map[string]interface{}); ok {
							if text, exists := contentObj["text"].(string); exists && text != "" {
								fullText.WriteString(text)
							}
						}
					}

					fullMessage := fullText.String()
					if fullMessage != "" {
						return Response{Message: fullMessage, HasFunc: false}, nil
					}
				}

				// Fallback - content как строка
				if content, ok := msg["content"].(string); ok && content != "" {
					return Response{Message: content, HasFunc: false}, nil
				}
			}
		}
	}

	return Response{}, fmt.Errorf("не удалось извлечь ответ от агента")
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
