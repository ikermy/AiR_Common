package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/sashabaranov/go-openai"
)

// JSONSchemaDefinition представляет JSON схему для ответа ассистента
type JSONSchemaDefinition struct {
	Type       string                        `json:"type"`
	Properties map[string]JSONSchemaProperty `json:"properties"`
	Required   []string                      `json:"required,omitempty"`
	Additional *bool                         `json:"additionalProperties,omitempty"`
}

type JSONSchemaProperty struct {
	Type       string                        `json:"type,omitempty"`
	Properties map[string]JSONSchemaProperty `json:"properties,omitempty"`
	Items      *JSONSchemaProperty           `json:"items,omitempty"`
	Enum       []string                      `json:"enum,omitempty"`
	Required   []string                      `json:"required,omitempty"`
	Additional *bool                         `json:"additionalProperties,omitempty"`
}

// MarshalJSON реализует интерфейс json.Marshaler
func (j JSONSchemaDefinition) MarshalJSON() ([]byte, error) {
	type Alias JSONSchemaDefinition
	return json.Marshal((Alias)(j))
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
			result := m.actionHandler.RunAction(ctx, toolCall.Function.Name, toolCall.Function.Arguments)
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

	if len(validResponses) == 0 {
		return emptyResponse, fmt.Errorf("не найдено валидных ответов от ассистента")
	}

	finalResponse := m.mergeResponses(validResponses)

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
	var (
		fileIDs     []string
		messageReq  openai.MessageRequest
		vectorStore *openai.VectorStore
	)
	// Гарантируем, что в createMsg/WithFiles не попадет nil
	t := text
	if t == nil {
		empty := ""
		t = &empty
	}
	if len(files) > 0 {
		vectorStore, err = m.getAssistantVectorStore(respModel.Assist.AssistId)
		if err != nil {
			logger.Error("Не удалось получить векторное хранилище: %w", err)
		}
	}

	// Создаем сообщение с файлами или без них
	if vectorStore != nil {
		var fileNames []string
		fileIDs, fileNames, err = m.uploadFilesForAssistant(files, vectorStore)
		if err != nil {
			logger.Error("Не удалось загрузить файлы: %w", err)
			messageReq = createMsg(t)
		} else {
			messageReq = createMsgWithFiles(t, fileNames)
		}
	} else {
		messageReq = createMsg(t)
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
			"operator": {
				Type: "boolean",
			},
		},
		Required:   []string{"message", "action", "target", "operator"}, // все поля обязательны иначе ошибка!
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
	defer func() {
		if len(fileIDs) > 0 {
			go m.cleanupFiles(fileIDs, vectorStore.ID)
		}
	}()

	return response, err
}
