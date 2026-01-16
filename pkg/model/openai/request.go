package openai

import (
	"AiR_TG-lead-generator/internal/app/model"
	"AiR_TG-lead-generator/internal/app/model/create"
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

func (m *OpenAIModel) handleRequiredAction(ctx context.Context, run *openai.Run) (*openai.Run, error) {
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

	updatedRun, err := m.client.SubmitToolOutputs(ctx, run.ThreadID, run.ID, openai.SubmitToolOutputsRequest{
		ToolOutputs: toolOutputs,
	})
	if err != nil {
		return nil, fmt.Errorf("не удалось отправить результаты функций: %w", err)
	}

	return &updatedRun, nil
}

func (m *OpenAIModel) waitForRunCompletion(ctx context.Context, run *openai.Run) (*openai.Run, error) {
	currentRun := run

	for currentRun.Status == openai.RunStatusQueued ||
		currentRun.Status == openai.RunStatusInProgress ||
		currentRun.Status == openai.RunStatusRequiresAction {

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

func (m *OpenAIModel) extractAssistantResponse(ctx context.Context, run *openai.Run) (model.AssistResponse, error) {
	var emptyResponse model.AssistResponse

	order := "desc"
	messagesList, err := m.client.ListMessage(ctx, run.ThreadID, nil, &order, nil, nil, nil)
	if err != nil {
		return emptyResponse, fmt.Errorf("не удалось получить список сообщений: %w", err)
	}

	if len(messagesList.Messages) == 0 {
		return emptyResponse, fmt.Errorf("получен пустой список сообщений")
	}

	var validResponses []model.AssistResponse

	for _, message := range messagesList.Messages {
		if message.Role == "assistant" && int64(message.CreatedAt) >= run.CreatedAt {
			for _, content := range message.Content {
				if content.Text != nil {
					response := content.Text.Value
					if response == "" {
						continue
					}

					var assistResp model.AssistResponse
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

func (m *OpenAIModel) mergeResponses(responses []model.AssistResponse) model.AssistResponse {
	if len(responses) == 1 {
		return responses[0]
	}

	var merged model.AssistResponse
	var messages []string
	var allFiles []model.File

	for _, resp := range responses {
		if resp.Message != "" {
			messages = append(messages, resp.Message)
		}
		if len(resp.Action.SendFiles) > 0 {
			allFiles = append(allFiles, resp.Action.SendFiles...)
		}
	}

	if len(messages) > 0 {
		merged.Message = strings.Join(messages, "\n\n")
	}

	if len(allFiles) > 0 {
		uniqueFiles := make(map[string]model.File)
		for _, file := range allFiles {
			uniqueFiles[file.URL] = file
		}

		for _, file := range uniqueFiles {
			merged.Action.SendFiles = append(merged.Action.SendFiles, file)
		}
	}

	return merged
}

func (m *OpenAIModel) Request(userId uint32, modelId string, dialogId uint64, text string, files ...model.FileUpload) (model.AssistResponse, error) {
	var emptyResponse model.AssistResponse

	if text == "" && len(files) == 0 {
		return emptyResponse, fmt.Errorf("пустое сообщение и нет файлов")
	}

	err := m.CreateThead(dialogId)
	if err != nil {
		logger.Warn("не удалось создать тред: %v", err, userId)
	}

	// Ищем RespModel по dialogId в Chan
	var respModel *RespModel
	m.responders.Range(func(key, value interface{}) bool {
		rm := value.(*RespModel)

		if rm.Chan != nil && rm.Chan.DialogId == dialogId {
			respModel = rm
			return false
		}
		return true
	})

	if respModel == nil {
		return emptyResponse, fmt.Errorf("RespModel не найден для dialogId %d", dialogId)
	}

	thead := respModel.Thread
	if thead == nil {
		return emptyResponse, fmt.Errorf("тред не найден для dialogId %d после попытки создания", dialogId)
	}

	// Обновляем TTL при каждом запросе
	respModel.TTL = time.Now().Add(m.UserModelTTl)

	var (
		fileIDs     []string
		messageReq  openai.MessageRequest
		vectorStore *openai.VectorStore
	)

	if len(files) > 0 {
		vectorStore, err = m.getAssistantVectorStore(respModel.Assist.AssistId)
		if err != nil {
			logger.Error("Не удалось получить векторное хранилище: %w", err, userId)
		}
	}

	if vectorStore != nil {
		var fileNames []string
		fileIDs, fileNames, err = m.uploadFilesForAssistant(files, vectorStore)
		if err != nil {
			logger.Error("Не удалось загрузить файлы: %w", err, userId)
			messageReq = createMsg(text)
		} else {
			messageReq = createMsgWithFiles(text, fileNames)
		}
	} else {
		messageReq = createMsg(text)
	}

	_, err = m.client.CreateMessage(m.ctx, thead.ID, messageReq)
	if err != nil {
		return emptyResponse, fmt.Errorf("не удалось создать сообщение: %w", err)
	}

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
		Required:   []string{"message", "action", "target", "operator"},
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

	if m.actionHandler != nil {
		if tools := m.actionHandler.GetTools(create.ProviderOpenAI); tools != nil {
			if openaiTools, ok := tools.([]openai.Tool); ok {
				runRequest.Tools = openaiTools
			}
		}
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

	defer func() {
		if len(fileIDs) > 0 {
			go m.cleanupFiles(fileIDs, vectorStore.ID)
		}
	}()

	return response, err
}

// createMsg создает простое сообщение для OpenAI
func createMsg(text string) openai.MessageRequest {
	return openai.MessageRequest{
		Role:    "user",
		Content: text,
	}
}

// createMsgWithFiles создает сообщение с файлами для OpenAI
func createMsgWithFiles(text string, fileNames []string) openai.MessageRequest {
	msg := openai.MessageRequest{
		Role:    "user",
		Content: text,
	}
	if len(fileNames) == 1 {
		text += fmt.Sprintf("\n\nОБЯЗАТЕЛЬНО используй file_search для анализа содержимого этого файла: %s. И если потребуется code_interpreter. ИГНОРИРУЙ все остальные файлы в векторном хранилище - это важно!", fileNames[0])
	} else {
		text += fmt.Sprintf("\n\nОБЯЗАТЕЛЬНО используй file_search для анализа содержимого этих файлов: %s. И если потребуется code_interpreter. ИГНОРИРУЙ все остальные файлы в векторном хранилище - это важно!", strings.Join(fileNames, ", "))
	}
	msg.Content = text

	return msg
}
