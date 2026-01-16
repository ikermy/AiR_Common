package openai

import (
	"AiR_TG-lead-generator/internal/app/model"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/sashabaranov/go-openai"
)

func (m *OpenAIModel) CreateThead(dialogId uint64) error {
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
		return fmt.Errorf("RespModel не найден для dialogId %d", dialogId)
	}

	// Если thread уже существует, ничего не делаем
	if respModel.Thread != nil {
		return nil
	}

	// Создаём новый thread
	th, err := m.client.CreateThread(m.ctx, openai.ThreadRequest{
		Messages: []openai.ThreadMessage{},
		Metadata: map[string]interface{}{
			"dialogId": fmt.Sprintf("%d", dialogId),
		},
	})
	if err != nil {
		return fmt.Errorf("не удалось создать тред: %w", err)
	}

	respModel.Thread = &th
	return nil
}

func (m *OpenAIModel) getAssistantVectorStore(assistantID string) (*openai.VectorStore, error) {
	assistant, err := m.client.RetrieveAssistant(m.ctx, assistantID)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить ассистента: %w", err)
	}

	if assistant.ToolResources != nil &&
		assistant.ToolResources.FileSearch != nil &&
		len(assistant.ToolResources.FileSearch.VectorStoreIDs) > 0 {

		vectorStoreID := assistant.ToolResources.FileSearch.VectorStoreIDs[0]
		vectorStore, err := m.client.RetrieveVectorStore(m.ctx, vectorStoreID)
		if err != nil {
			return nil, fmt.Errorf("не удалось получить векторное хранилище: %w", err)
		}

		return &vectorStore, nil
	}

	return nil, fmt.Errorf("у ассистента нет привязанного векторного хранилища")
}

func (m *OpenAIModel) addFilesToVectorStore(vectorStoreID string, fileIDs []string) error {
	for _, fileID := range fileIDs {
		_, err := m.client.CreateVectorStoreFile(m.ctx, vectorStoreID, openai.VectorStoreFileRequest{
			FileID: fileID,
		})
		if err != nil {
			logger.Error("Не удалось добавить файл %s в векторное хранилище: %v", fileID, err)
			continue
		}

		err = m.waitForFileProcessing(vectorStoreID, fileID)
		if err != nil {
			logger.Warn("Файл %s не был полностью обработан в векторном хранилище: %v", fileID, err)
		}
	}
	return nil
}

func (m *OpenAIModel) waitForFileProcessing(vectorStoreID, fileID string) error {
	maxRetries := 60
	retryDelay := 1 * time.Second

	for i := 0; i < maxRetries; i++ {
		select {
		case <-m.ctx.Done():
			return fmt.Errorf("отменено контекстом ожидание обработки файла %s", fileID)
		default:
		}

		vectorStoreFile, err := m.client.RetrieveVectorStoreFile(
			m.ctx,
			vectorStoreID,
			fileID,
		)
		if err != nil {
			logger.Warn("Ошибка получения статуса файла %s: %v", fileID, err)
			time.Sleep(retryDelay)
			continue
		}

		switch vectorStoreFile.Status {
		case "completed":
			return nil
		case "failed":
			return fmt.Errorf("обработка файла %s в векторном хранилище завершилась неудачей", fileID)
		case "cancelled":
			return fmt.Errorf("обработка файла %s в векторном хранилище была отменена", fileID)
		case "in_progress":
			time.Sleep(retryDelay)
			continue
		default:
			logger.Warn("Неизвестный статус файла %s: %s", fileID, vectorStoreFile.Status)
			time.Sleep(retryDelay)
			continue
		}
	}

	return fmt.Errorf("превышено время ожидания обработки файла %s", fileID)
}

func (m *OpenAIModel) uploadFilesForAssistant(files []model.FileUpload, vectorStore *openai.VectorStore) ([]string, []string, error) {
	if len(files) == 0 {
		return nil, nil, nil
	}

	select {
	case <-m.ctx.Done():
		return nil, nil, fmt.Errorf("отменено контекстом перед загрузкой файлов")
	default:
	}

	fileIDs, err := m.uploadFiles(files)
	if err != nil {
		return nil, nil, fmt.Errorf("не удалось загрузить файлы: %w", err)
	}

	select {
	case <-m.ctx.Done():
		return nil, nil, fmt.Errorf("отменено контекстом после загрузки файлов")
	default:
	}

	var fileNames []string
	for _, file := range files {
		fileNames = append(fileNames, file.Name)
	}

	if err = m.addFilesToVectorStore(vectorStore.ID, fileIDs); err != nil {
		return nil, nil, fmt.Errorf("не удалось добавить файлы в векторное хранилище: %v", err)
	}

	select {
	case <-m.ctx.Done():
		return nil, nil, fmt.Errorf("отменено контекстом после индексирования файлов")
	default:
	}

	return fileIDs, fileNames, nil
}

func (m *OpenAIModel) uploadFiles(files []model.FileUpload) ([]string, error) {
	var fileIDs []string

	for _, file := range files {
		data, err := io.ReadAll(file.Content)
		if err != nil {
			return nil, fmt.Errorf("не удалось прочитать содержимое файла %s: %w", file.Name, err)
		}

		uploadReq := openai.FileBytesRequest{
			Name:    file.Name,
			Bytes:   data,
			Purpose: openai.PurposeAssistants,
		}

		uploadedFile, err := m.client.CreateFileBytes(m.ctx, uploadReq)
		if err != nil {
			return nil, fmt.Errorf("не удалось загрузить файл %s: %w", file.Name, err)
		}

		fileIDs = append(fileIDs, uploadedFile.ID)
	}

	return fileIDs, nil
}

func (m *OpenAIModel) cleanupFiles(fileIDs []string, vectorStoreID ...string) {
	for _, fileID := range fileIDs {
		if len(vectorStoreID) > 0 && vectorStoreID[0] != "" {
			err := m.client.DeleteVectorStoreFile(m.ctx, vectorStoreID[0], fileID)
			if err != nil {
				if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "Not found") {
					// Файл уже удален
				} else {
					logger.Warn("не удалось удалить файл %s из векторного хранилища %s: %v", fileID, vectorStoreID[0], err)
				}
			}
		}

		err := m.client.DeleteFile(m.ctx, fileID)
		if err != nil {
			if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "Not found") {
				// Файл уже удален
			} else {
				logger.Warn("не удалось удалить файл %s из общего хранилища: %v", fileID, err)
			}
		}
	}
}

func (m *OpenAIModel) downloadFileFromOpenAI(fileID string) ([]byte, error) {
	rawResponse, err := m.client.GetFileContent(m.ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("не удалось скачать содержимое файла: %w", err)
	}
	defer func() { _ = rawResponse.Close() }()

	content, err := io.ReadAll(rawResponse)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать содержимое файла: %w", err)
	}

	return content, nil
}

func (m *OpenAIModel) extractGeneratedFiles(run *openai.Run) ([]string, error) {
	order := "desc"
	messagesList, err := m.client.ListMessage(m.ctx, run.ThreadID, nil, &order, nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить сообщения: %w", err)
	}

	var generatedFileIDs []string

	for _, message := range messagesList.Messages {
		if message.Role == "assistant" && int64(message.CreatedAt) >= run.CreatedAt {
			for _, content := range message.Content {
				if content.ImageFile != nil {
					generatedFileIDs = append(generatedFileIDs, content.ImageFile.FileID)
				}
			}

			for _, fileID := range message.FileIds {
				generatedFileIDs = append(generatedFileIDs, fileID)
			}
		}
	}

	return generatedFileIDs, nil
}
