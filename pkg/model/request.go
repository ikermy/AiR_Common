package model

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/sashabaranov/go-openai"
	"strings"
	"time"
)

// JSONSchemaDefinition представляет JSON схему для ответа ассистента
type JSONSchemaDefinition struct {
	Type       string                        `json:"type"`
	Properties map[string]JSONSchemaProperty `json:"properties"`
	Required   []string                      `json:"required,omitempty"`
	Additional *bool                         `json:"additionalProperties,omitempty"` // Изменено на указатель
}

type JSONSchemaProperty struct {
	Type       string                        `json:"type,omitempty"`
	Properties map[string]JSONSchemaProperty `json:"properties,omitempty"`
	Items      *JSONSchemaProperty           `json:"items,omitempty"`
	Enum       []string                      `json:"enum,omitempty"`
	Required   []string                      `json:"required,omitempty"`
	Additional *bool                         `json:"additionalProperties,omitempty"` // Изменено на указатель
}

// MarshalJSON реализует интерфейс json.Marshaler
func (j JSONSchemaDefinition) MarshalJSON() ([]byte, error) {
	type Alias JSONSchemaDefinition
	return json.Marshal((Alias)(j))
}

func createMsg(text *string) openai.MessageRequest {
	lastMessage := openai.MessageRequest{
		Role:    "user",
		Content: *text,
	}
	return lastMessage
}

// handleRequiredAction обрабатывает статус RunStatusRequiresAction
func (m *Models) handleRequiredAction(ctx context.Context, run *openai.Run) (*openai.Run, error) {
	if run.RequiredAction == nil || run.RequiredAction.SubmitToolOutputs == nil {
		return run, nil
	}

	toolOutputs := make([]openai.ToolOutput, 0)

	for _, toolCall := range run.RequiredAction.SubmitToolOutputs.ToolCalls {
		var output openai.ToolOutput

		if m.actionHandler != nil {
			result := m.actionHandler.RunAction(toolCall.Function.Name, toolCall.Function.Arguments)
			output = openai.ToolOutput{
				ToolCallID: toolCall.ID,
				Output:     result,
			}
		} else {
			output = openai.ToolOutput{
				ToolCallID: toolCall.ID,
				Output:     fmt.Sprintf("Функция %s не поддерживается", toolCall.Function.Name),
			}
		}

		toolOutputs = append(toolOutputs, output)
	}

	// Отправляем результаты выполнения функций
	updatedRun, err := m.client.SubmitToolOutputs(ctx, run.ThreadID, run.ID, openai.SubmitToolOutputsRequest{
		ToolOutputs: toolOutputs,
	})
	if err != nil {
		return nil, fmt.Errorf("не удалось отправить результаты функций: %w", err)
	}

	return &updatedRun, nil
}

// waitForRunCompletion ожидает завершения выполнения run
func (m *Models) waitForRunCompletion(ctx context.Context, run *openai.Run) (*openai.Run, error) {
	currentRun := run

	for currentRun.Status == openai.RunStatusQueued ||
		currentRun.Status == openai.RunStatusInProgress ||
		currentRun.Status == openai.RunStatusRequiresAction {

		// Обработка RunStatusRequiresAction
		if currentRun.Status == openai.RunStatusRequiresAction {
			updatedRun, err := m.handleRequiredAction(ctx, currentRun)
			if err != nil {
				return nil, err
			}
			currentRun = updatedRun
		}

		time.Sleep(50 * time.Millisecond)

		retrievedRun, err := m.client.RetrieveRun(ctx, currentRun.ThreadID, currentRun.ID)
		if err != nil {
			return nil, fmt.Errorf("не удалось получить статус запуска: %w", err)
		}
		currentRun = &retrievedRun
	}

	if currentRun.Status != openai.RunStatusCompleted {
		return nil, fmt.Errorf("запуск завершился неудачно со статусом %s", currentRun.Status)
	}

	return currentRun, nil
}

// extractAssistantResponse извлекает ответ ассистента из сообщений треда
func (m *Models) extractAssistantResponse(ctx context.Context, run *openai.Run) (AssistResponse, error) {
	var emptyResponse AssistResponse

	order := "desc"
	messagesList, err := m.client.ListMessage(ctx, run.ThreadID, nil, &order, nil, nil, nil)
	if err != nil {
		return emptyResponse, fmt.Errorf("не удалось получить список сообщений: %w", err)
	}

	if len(messagesList.Messages) == 0 {
		return emptyResponse, fmt.Errorf("получен пустой список сообщений")
	}

	var validResponses []AssistResponse

	// Собираем все валидные ответы от ассистента после запуска
	for _, message := range messagesList.Messages {
		if message.Role == "assistant" && int64(message.CreatedAt) >= run.CreatedAt {
			for _, content := range message.Content {
				if content.Text != nil {
					response := content.Text.Value
					if response == "" {
						continue
					}

					logger.Debug("Получен JSON ответ от ассистента: %s", response)

					var assistResp AssistResponse
					if err := json.Unmarshal([]byte(response), &assistResp); err != nil {
						logger.Error("Ошибка парсинга JSON: %v. Ответ: %s", err, response)
						continue
					}

					validResponses = append(validResponses, assistResp)
				}
			}
		}
	}

	// Извлекаем созданные файлы
	generatedFiles, err := m.extractGeneratedFiles(ctx, run)
	if err != nil {
		logger.Warn("Не удалось извлечь созданные файлы: %v", err)
	} else {
		logger.Debug("Найдено %d созданных файлов Code Interpreter: %v", len(generatedFiles), generatedFiles)
	}

	// ИСПРАВЛЕННАЯ ЛОГИКА ЗАМЕНЫ URL
	if len(generatedFiles) > 0 && len(validResponses) > 0 {
		logger.Debug("Начинаем замену URL в %d ответах", len(validResponses))

		for i := range validResponses {
			for j := range validResponses[i].Action.SendFiles {
				originalURL := validResponses[i].Action.SendFiles[j].URL

				// Проверяем точное соответствие placeholder
				if originalURL == "file_placeholder_will_be_replaced_by_system" && j < len(generatedFiles) {
					newURL := fmt.Sprintf("openai_file:%s", generatedFiles[j])
					validResponses[i].Action.SendFiles[j].URL = newURL

					logger.Debug("Заменен placeholder URL на: %s для файла: %s",
						newURL, validResponses[i].Action.SendFiles[j].FileName)
				} else {
					logger.Debug("URL не заменен: '%s' (placeholder: %v, файлов: %d)",
						originalURL, originalURL == "file_placeholder_will_be_replaced_by_system", len(generatedFiles))
				}
			}
		}
	} else {
		logger.Debug("Замена URL пропущена: файлов=%d, ответов=%d", len(generatedFiles), len(validResponses))
	}

	if len(validResponses) == 0 {
		return emptyResponse, fmt.Errorf("не найдено валидных ответов от ассистента")
	}

	finalResponse := m.mergeResponses(validResponses)

	// Финальное логирование для проверки
	logger.Debug("Финальный ответ - Message: '%s', Files count: %d",
		finalResponse.Message, len(finalResponse.Action.SendFiles))

	for i, file := range finalResponse.Action.SendFiles {
		logger.Debug("Файл %d: URL='%s', Name='%s'", i, file.URL, file.FileName)
	}

	return finalResponse, nil
}

// mergeResponses объединяет несколько ответов в один
func (m *Models) mergeResponses(responses []AssistResponse) AssistResponse {
	if len(responses) == 1 {
		return responses[0]
	}

	var merged AssistResponse
	var messages []string
	var allFiles []File

	for _, resp := range responses {
		if resp.Message != "" {
			messages = append(messages, resp.Message)
		}
		if len(resp.Action.SendFiles) > 0 {
			allFiles = append(allFiles, resp.Action.SendFiles...)
		}
	}

	// Объединяем сообщения
	if len(messages) > 0 {
		merged.Message = strings.Join(messages, "\n\n")
	}

	// Объединяем файлы (удаляем дубликаты по URL)
	if len(allFiles) > 0 {
		uniqueFiles := make(map[string]File)
		for _, file := range allFiles {
			uniqueFiles[file.URL] = file
		}

		for _, file := range uniqueFiles {
			merged.Action.SendFiles = append(merged.Action.SendFiles, file)
		}
	}

	return merged
}

func (m *Models) Request(modelId string, dialogId uint64, text *string, files ...FileUpload) (AssistResponse, error) {
	var emptyResponse AssistResponse

	if (text == nil || *text == "") && len(files) == 0 {
		return emptyResponse, fmt.Errorf("пустое сообщение и нет файлов")
	}

	err := m.CreateThead(dialogId)
	if err != nil {
		logger.Warn("не удалось создать тред: %v", err)
	}

	val, ok := m.responders.Load(dialogId)
	if !ok {
		return emptyResponse, fmt.Errorf("RespModel не найден для dialogId %d", dialogId)
	}

	respModel := val.(*RespModel)

	respModel.mu.RLock()
	thead, ok := respModel.TreadsGPT[dialogId]
	respModel.mu.RUnlock()

	if !ok || thead == nil {
		return emptyResponse, fmt.Errorf("тред не найден для dialogId %d после попытки создания", dialogId)
	}

	// Загружаем файлы, если они есть
	var fileIDs []string
	if len(files) > 0 {
		fileIDs, err = m.uploadFiles(files)
		if err != nil {
			return emptyResponse, fmt.Errorf("не удалось загрузить файлы: %w", err)
		}
	}

	// Создаем сообщение с файлами или без них
	var messageReq openai.MessageRequest
	if len(fileIDs) > 0 {
		messageReq = createMsgWithFiles(text, fileIDs)
	} else {
		messageReq = createMsg(text)
	}

	_, err = m.client.CreateMessage(m.ctx, thead.ID, messageReq)
	if err != nil {
		return emptyResponse, fmt.Errorf("не удалось создать сообщение: %w", err)
	}

	// Создаем схему ответа
	additionalFalse := false
	schema := JSONSchemaDefinition{
		Type: "object",
		Properties: map[string]JSONSchemaProperty{
			"message": {
				Type: "string",
			},
			"action": {
				Type: "object",
				Properties: map[string]JSONSchemaProperty{
					"send_files": {
						Type: "array",
						Items: &JSONSchemaProperty{
							Type: "object",
							Properties: map[string]JSONSchemaProperty{
								"type":      {Type: "string", Enum: []string{"photo", "video", "audio", "doc"}},
								"url":       {Type: "string"},
								"file_name": {Type: "string"},
								"caption":   {Type: "string"},
							},
							Required:   []string{"type", "url", "file_name", "caption"},
							Additional: &additionalFalse,
						},
					},
				},
				Required:   []string{"send_files"},
				Additional: &additionalFalse,
			},
			"target": {
				Type: "boolean",
			},
		},
		Required:   []string{"message", "action", "target"},
		Additional: &additionalFalse,
	}

	responseFormat := &openai.ChatCompletionResponseFormat{
		Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
		JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
			Name:   "assist_response",
			Strict: true,
			Schema: schema,
		},
	}

	runRequest := openai.RunRequest{
		AssistantID:    modelId,
		ResponseFormat: responseFormat,
	}

	run, err := m.client.CreateRun(m.ctx, thead.ID, runRequest)
	if err != nil {
		return emptyResponse, fmt.Errorf("не удалось создать запуск: %w", err)
	}

	completedRun, err := m.waitForRunCompletion(m.ctx, &run)
	if err != nil {
		return emptyResponse, err
	}

	response, err := m.extractAssistantResponse(m.ctx, completedRun)

	// Очищаем загруженные файлы после обработки
	if len(fileIDs) > 0 {
		go m.cleanupFiles(fileIDs)
	}

	return response, err
}
