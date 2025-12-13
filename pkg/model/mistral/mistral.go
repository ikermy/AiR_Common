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
func (m *MistralAgentClient) ChatWithAgent(agentID string, messages []MistralMessage, tools interface{}) (Response, error) {
	payload := map[string]interface{}{
		"agent_id": agentID,
		"messages": messages,
	}

	// Добавляем инструменты если они переданы
	if tools != nil {
		payload["tools"] = tools
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

// ChatWithAgentFiles отправляет массив сообщений агенту с поддержкой файлов
func (m *MistralAgentClient) ChatWithAgentFiles(agentID string, messages []MistralMessage, files []io.Reader, fileNames []string, tools interface{}) (Response, error) {
	// Пока Mistral API не поддерживает напрямую файлы в запросах к агентам
	// Можно реализовать загрузку файлов отдельно или передавать их содержимое в сообщениях
	// Для простоты используем базовый ChatWithAgent
	// В будущем можно расширить функциональность
	return m.ChatWithAgent(agentID, messages, tools)
}

// Shutdown корректно завершает работу клиента
func (m *MistralAgentClient) Shutdown() {
	if m.cancel != nil {
		m.cancel()
	}
}
