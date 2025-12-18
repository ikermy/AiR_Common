package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/sashabaranov/go-openai"
)

const ModelShemaJSON = `{
        "type": "object",
        "properties": {
            "message": {
                "type": "string"
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
                                    "enum": ["photo", "video", "audio", "doc"]
                                },
                                "url": {
                                    "type": "string"
                                },
                                "file_name": {
                                    "type": "string"
                                },
                                "caption": {
                                    "type": "string"
                                }
                            },
                            "required": ["type", "url", "file_name", "caption"],
                            "additionalProperties": false
                        }
                    }
                },
                "required": ["send_files"],
                "additionalProperties": false
            },
            "target": { "type": "boolean" },
			"operator": { "type": "boolean" }
        },
        "required": ["message", "action", "target", "operator"],
        "additionalProperties": false
    }`

// вызывается во внешнем приложении при добавлении файла пользователем
// UploadFileToOpenAI загружает файл в OpenAI и возвращает его ID
func (m *UniversalModel) UploadFileToOpenAI(fileName string, fileData []byte) (string, error) {
	// Создаем запрос на загрузку файла из байтов
	fileRequest := openai.FileBytesRequest{
		Name:    fileName,
		Bytes:   fileData,
		Purpose: openai.PurposeAssistants,
	}

	// Загружаем файл через API OpenAI
	fileResponse, err := m.openaiClient.CreateFileBytes(m.ctx, fileRequest)
	if err != nil {
		return "", fmt.Errorf("ошибка загрузки файла через API OpenAI: %w", err)
	}

	return fileResponse.ID, nil
}

// вызывается во внешнем приложении при добавлении файла пользователем
// AddFileFromOpenAI добавляет новый файл в существующее векторное хранилище пользователя
func (m *UniversalModel) AddFileFromOpenAI(userId uint32, fileID, fileName string) error {
	// Проверка наличия OpenAI клиента
	if m.openaiClient == nil {
		return fmt.Errorf("OpenAI клиент не инициализирован")
	}

	// Получаем данные пользовательского Vector Store
	vectorStoreID, err := m.db.GetUserVectorStorage(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения векторного хранилища: %w", err)
	}

	// Добавляем файл в существующий Vector Store
	_, err = m.openaiClient.CreateVectorStoreFile(m.ctx, vectorStoreID, openai.VectorStoreFileRequest{
		FileID: fileID,
	})
	if err != nil {
		return fmt.Errorf("ошибка добавления файла в Vector Store: %w", err)
	}

	logger.Debug("Файл %s успешно добавлен в Vector Store", fileName, userId)
	return nil
}

// deleteFileFromOpenAI удаляет файл из OpenAI и связанного с ним Vector Store
func (m *UniversalModel) deleteFileFromOpenAI(fileID string) error {
	// 1. Удаляем файл по его ID
	if err := m.openaiClient.DeleteFile(m.ctx, fileID); err != nil {
		// Если файл уже удален (not found), это не является критической ошибкой
		if !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("ошибка удаления файла из OpenAI: %w", err)
		}
		logger.Error("Файл %s уже был удален или не найден в OpenAI: %v", fileID, err)
	}

	// 2. Ищем и удаляем связанный Vector Store
	// Получаем список всех векторных хранилищ
	vsList, err := m.openaiClient.ListVectorStores(m.ctx, openai.Pagination{})
	if err != nil {
		return fmt.Errorf("ошибка получения списка Vector Stores: %w", err)
	}

	// Ищем Vector Store, который содержит наш файл
	for _, vs := range vsList.VectorStores {
		// Получаем список файлов для каждого Vector Store
		files, err := m.openaiClient.ListVectorStoreFiles(m.ctx, vs.ID, openai.Pagination{})
		if err != nil {
			logger.Error("Предупреждение: не удалось получить файлы для Vector Store %s: %v", vs.ID, err)
			continue
		}

		// Если в хранилище только один файл и его ID совпадает с нашим, удаляем хранилище
		if len(files.VectorStoreFiles) == 1 && files.VectorStoreFiles[0].ID == fileID {
			_, err := m.openaiClient.DeleteVectorStore(m.ctx, vs.ID)
			if err != nil {
				// Логируем ошибку, но не прерываем процесс, так как основной файл уже мог быть удален
				logger.Error("Предупреждение: не удалось удалить Vector Store %s: %v", vs.ID, err)
			} else {
				logger.Debug("Vector Store %s, связанный с файлом %s, успешно удален: %v", vs.ID, fileID, err)
			}
			// Прерываем цикл, так как нашли и обработали нужное хранилище
			break
		}
	}

	return nil
}

// createModel Создаю новую модель OpenAI Assistant
func (m *UniversalModel) createModel(
	userId uint32, gptName string, modelName string, model []byte, fileIDs []Ids) (UMCR, error) {
	// Извлекаем текстовые инструкции из JSON
	var modelData map[string]interface{}
	if err := json.Unmarshal(model, &modelData); err != nil {
		return UMCR{}, fmt.Errorf("ошибка при разборе JSON модели: %w", err)
	}

	// Создаем текст для системных инструкций
	systemInstructions := modelData["prompt"].(string)

	// Извлекаю id[]string из fileIDs
	var ids []string
	for _, fileID := range fileIDs {
		if fileID.ID != "" {
			ids = append(ids, fileID.ID)
		}
	}

	var vectorStoreIDs []string
	// Если есть файлы, создаем для них Vector Store
	if len(ids) > 0 {
		vsName := fmt.Sprintf("vs_user_%d_%d", userId, time.Now().Unix())
		vsRequest := openai.VectorStoreRequest{
			Name:    vsName,
			FileIDs: ids,
		}
		vectorStore, err := m.openaiClient.CreateVectorStore(m.ctx, vsRequest)
		if err != nil {
			return UMCR{}, fmt.Errorf("ошибка создания Vector Store: %w", err)
		}
		vectorStoreIDs = append(vectorStoreIDs, vectorStore.ID)
	}

	description := fmt.Sprintf("Модель для пользователя %d", userId)

	// Создаем базовый AssistantRequest
	assistantRequest := openai.AssistantRequest{
		Name:         &modelName,
		Description:  &description,
		Instructions: &systemInstructions,
		Model:        gptName,
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   "response_with_action_files",
				Strict: true,
				Schema: json.RawMessage(ModelShemaJSON),
			},
		},
	}

	// Условно добавляем инструменты на основе флагов в modelData
	var tools []openai.AssistantTool

	// Принудительно добавляем file_search если есть файлы
	if len(vectorStoreIDs) > 0 {
		tools = append(tools, openai.AssistantTool{Type: "file_search"})
	} else if search, ok := modelData["search"].(bool); ok && search {
		tools = append(tools, openai.AssistantTool{Type: "file_search"})
	}

	if interpreter, ok := modelData["interpreter"].(bool); ok && interpreter {
		tools = append(tools, openai.AssistantTool{Type: "code_interpreter"})
	}

	// Добавляем функции get_s3_files и create_file
	tools = append(tools,
		openai.AssistantTool{
			Type: "function",
			Function: &openai.FunctionDefinition{
				Name:        "get_s3_files",
				Description: "Получает список доступных файлов для конкретного пользователя",
				Strict:      false,
				Parameters: map[string]interface{}{
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
		openai.AssistantTool{
			Type: "function",
			Function: &openai.FunctionDefinition{
				Name:        "create_file",
				Description: "Создает файл с указанным содержимым и сохраняет его на S3 для конкретного пользователя",
				Strict:      false,
				Parameters: map[string]interface{}{
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

	// Устанавливаем инструменты (теперь они всегда будут, так как добавили функции)
	assistantRequest.Tools = tools

	// Добавляем ToolResources только если есть векторы для file_search
	if len(vectorStoreIDs) > 0 {
		assistantRequest.ToolResources = &openai.AssistantToolResource{
			FileSearch: &openai.AssistantToolFileSearch{
				VectorStoreIDs: vectorStoreIDs,
			},
		}
	}

	assistant, err := m.openaiClient.CreateAssistant(m.ctx, assistantRequest)
	if err != nil {
		// Если были fileIDs, удаляю их из OpenAI
		for _, fileID := range ids {
			if errDel := m.deleteFileFromOpenAI(fileID); errDel != nil {
				logger.Error("ошибка удаления файла %s при ошибке создания ассистента: %v", fileID, errDel)
			}
		}

		return UMCR{}, fmt.Errorf("ошибка создания Assistant через OpenAI API: %w", err)
	}

	type VecIds struct {
		FileIds  []Ids
		VectorId []string
	}

	vecIds := VecIds{
		FileIds:  fileIDs,
		VectorId: vectorStoreIDs,
	}
	// Преобразую fileIDs в json.RawMessage
	allIds, err := json.Marshal(vecIds)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка при преобразовании fileIDs в JSON: %w", err)
	}

	return UMCR{
		AssistID: assistant.ID,
		AllIds:   allIds,
		Provider: ProviderOpenAI,
	}, nil
}

// deleteOpenAIModel удаляет OpenAI Assistant (с поддержкой WS сообщений)
func (m *UniversalModel) deleteOpenAIModel(userId uint32, modelData *UserModelRecord, deleteFiles bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Удаление ассистента из OpenAI...")
	}

	if m.openaiClient != nil {
		// Удаляем Assistant из OpenAI
		_, err := m.openaiClient.DeleteAssistant(m.ctx, modelData.AssistId)
		if err != nil {
			if !strings.Contains(err.Error(), "not found") {
				return fmt.Errorf("ошибка удаления ассистента: %w", err)
			}
			logger.Error("Ассистент %s не найден в OpenAI", modelData.AssistId, userId)
		}

		// Удаляем файлы только если deleteFiles = true
		if deleteFiles && len(modelData.FileIds) > 0 {
			if progressCallback != nil {
				progressCallback(fmt.Sprintf("🔄 Удаление файлов из OpenAI (%d файлов)...", len(modelData.FileIds)))
			}

			// Удаляем все файлы
			for i, file := range modelData.FileIds {
				if err := m.deleteFileFromOpenAI(file.ID); err != nil {
					logger.Error("Ошибка удаления файла %s: %v", file.ID, err, userId)
				}

				// Отправляем прогресс каждые 5 файлов
				if progressCallback != nil && (i+1)%5 == 0 {
					progressCallback(fmt.Sprintf("🔄 Удалено %d из %d файлов...", i+1, len(modelData.FileIds)))
				}
			}
		}
	} else {
		logger.Warn("OpenAI клиент не инициализирован, пропускаем удаление из API")
		if progressCallback != nil {
			progressCallback("⚠️ OpenAI клиент не инициализирован, удаляем только из БД")
		}
	}

	// Удаляем векторные хранилища
	//if len(modelData.VectorIDs) > 0 {
	//	if progressCallback != nil {
	//		progressCallback("🔄 Удаление векторных хранилищ...")
	//	}
	//
	//	for _, vectorId := range modelData.VectorIDs {
	//		if _, err := m.openaiClient.DeleteVectorStore(m.ctx, vectorId); err != nil {
	//			logger.Error("Ошибка удаления Vector Store %s: %v", vectorId, err, userId)
	//		}
	//	}
	//}

	if progressCallback != nil {
		progressCallback("✅ OpenAI Assistant и файлы удалены из API")
	}

	err := m.db.RemoveModelFromUser(userId, modelData.ModelId)
	if err != nil {
		return fmt.Errorf("ошибка удаления связи из user_models: %w", err)
	}

	// Если удалённая модель была активной - переключаем на оставшуюся
	if modelData.IsActive {
		remainingModels, err := m.db.GetAllUserModels(userId)
		if err != nil {
			logger.Warn("Ошибка получения оставшихся моделей: %v", err, userId)
		} else if len(remainingModels) > 0 {
			// Переключаем на первую оставшуюся модель по провайдеру
			newActiveProvider := remainingModels[0].Provider
			err = m.db.SetActiveModelByProvider(userId, newActiveProvider)
			if err != nil {
				logger.Error("Ошибка автоматического переключения активной модели: %v", err, userId)
			} else {
				logger.Info("Активная модель автоматически переключена на провайдер %s после удаления",
					newActiveProvider.String(), userId)
				if progressCallback != nil {
					progressCallback(fmt.Sprintf("✅ Активная модель переключена на %s", newActiveProvider.String()))
				}
			}
		}
	}

	if progressCallback != nil {
		progressCallback("✅ Модель пользователя успешно удалена")
	}

	logger.Info("OpenAI модель успешно удалена из API и БД", userId)
	return nil
}

// createOpenAIModel создаёт OpenAI Assistant (внутренний метод)
func (m *UniversalModel) createOpenAIModel(userId uint32, gptName string, modelName string, modelJSON []byte, fileIDs []Ids) (UMCR, error) {
	if m.openaiClient == nil {
		return UMCR{}, fmt.Errorf("OpenAI клиент не инициализирован")
	}
	// Используем существующий метод createModel
	umcr, err := m.createModel(userId, gptName, modelName, modelJSON, fileIDs)
	if err != nil {
		return UMCR{}, err
	}

	return umcr, nil
}

// updateOpenAIModelInPlace обновляет OpenAI Assistant
func (m *UniversalModel) updateOpenAIModelInPlace(userId uint32, existing, updated *UniversalModelData, modelJSON []byte) error {
	// Парсим JSON для извлечения дополнительных настроек
	var modelData map[string]interface{}
	if err := json.Unmarshal(modelJSON, &modelData); err != nil {
		return fmt.Errorf("ошибка разбора JSON модели: %w", err)
	}

	description := fmt.Sprintf("Модель для пользователя %d", userId)

	// Определяем инструменты
	var tools []openai.AssistantTool
	var vectorStoreIDs []string

	// Проверяем, нужен ли file_search
	searchEnabled, _ := modelData["search"].(bool)
	needsFileSearch := searchEnabled && len(updated.FileIds) > 0

	existingModelData, err := m.db.GetModelByProvider(userId, existing.Provider)
	if err != nil || existingModelData == nil {
		return fmt.Errorf("ошибка получения записи модели: %w", err)
	}

	if needsFileSearch {
		// Проверяем, изменились ли файлы
		if !filesEqual(existing.FileIds, updated.FileIds) {
			// Создаем новое векторное хранилище
			var ids []string
			for _, fileID := range updated.FileIds {
				if fileID.ID != "" {
					ids = append(ids, fileID.ID)
				}
			}

			vsName := fmt.Sprintf("vs_user_%d_%d", userId, time.Now().Unix())
			vsRequest := openai.VectorStoreRequest{
				Name:    vsName,
				FileIDs: ids,
			}
			vectorStore, err := m.openaiClient.CreateVectorStore(m.ctx, vsRequest)
			if err != nil {
				return fmt.Errorf("ошибка создания нового Vector Store: %w", err)
			}
			vectorStoreIDs = append(vectorStoreIDs, vectorStore.ID)

			// Удаляем старые файлы и векторные хранилища
			for _, file := range existing.FileIds {
				if err := m.deleteFileFromOpenAI(file.ID); err != nil {
					logger.Error("Ошибка удаления файла %s: %v", file.ID, err, userId)
				}
			}

			for _, oldVectorId := range existing.VecIds.VectorId {
				if _, err := m.openaiClient.DeleteVectorStore(m.ctx, oldVectorId); err != nil {
					logger.Error("Ошибка удаления старого Vector Store %s: %v", oldVectorId, err, userId)
				}
			}
		} else {
			// Файлы не изменились
			vectorStoreIDs = existing.VecIds.VectorId
		}

		tools = append(tools, openai.AssistantTool{Type: "file_search"})
	} else {
		// File search не нужен - удаляем все файлы и векторные хранилища
		for _, file := range existing.FileIds {
			if err := m.deleteFileFromOpenAI(file.ID); err != nil {
				logger.Error("Ошибка удаления файла %s: %v", file.ID, err, userId)
			}
		}

		for _, vectorId := range existing.VecIds.VectorId {
			if _, err := m.openaiClient.DeleteVectorStore(m.ctx, vectorId); err != nil {
				logger.Error("Ошибка удаления Vector Store %s: %v", vectorId, err, userId)
			}
		}

		vectorStoreIDs = []string{}
		logger.Debug("Векторные хранилища и файлы удалены, так как search=false или нет файлов", userId)
	}

	// Code interpreter
	if interpreter, ok := modelData["interpreter"].(bool); ok && interpreter {
		tools = append(tools, openai.AssistantTool{Type: "code_interpreter"})
	}

	// Добавляем стандартные функции (из action_handler.go)
	tools = append(tools,
		openai.AssistantTool{
			Type: "function",
			Function: &openai.FunctionDefinition{
				Name:        "lead_target",
				Description: "Отмечает достижение целевого действия в диалоге",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"target": map[string]interface{}{
							"type":        "boolean",
							"description": "true если цель достигнута",
						},
					},
					"required": []string{"target"},
				},
			},
		},
		openai.AssistantTool{
			Type: "function",
			Function: &openai.FunctionDefinition{
				Name:        "get_s3_files",
				Description: "Получает список доступных файлов для конкретного пользователя",
				Parameters: map[string]interface{}{
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
	)

	// Создаем запрос на обновление
	updateRequest := openai.AssistantRequest{
		Name:         &updated.Name,
		Description:  &description,
		Instructions: &updated.Prompt,
		Model:        updated.GptType.Name,
		Tools:        tools,
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   "response_with_action_files",
				Strict: true,
				Schema: json.RawMessage(ModelShemaJSON),
			},
		},
	}

	// Добавляем ToolResources только если есть векторы
	if len(vectorStoreIDs) > 0 {
		updateRequest.ToolResources = &openai.AssistantToolResource{
			FileSearch: &openai.AssistantToolFileSearch{
				VectorStoreIDs: vectorStoreIDs,
			},
		}
	}

	// Обновляем ассистента через OpenAI API
	_, err = m.openaiClient.ModifyAssistant(m.ctx, existingModelData.AssistId, updateRequest)
	if err != nil {
		return fmt.Errorf("ошибка обновления Assistant: %w", err)
	}

	// Обновляем информацию о файлах и векторах
	type VecIds struct {
		FileIds  []Ids
		VectorId []string
	}

	vecIds := VecIds{
		FileIds:  updated.FileIds,
		VectorId: vectorStoreIDs,
	}

	// Сериализуем vecIds в JSON
	vecIdsJSON, err := json.Marshal(vecIds)
	if err != nil {
		return fmt.Errorf("failed to marshal vector IDs: %w", err)
	}

	umcr := UMCR{
		AssistID: existingModelData.AssistId,
		AllIds:   vecIdsJSON,
		Provider: ProviderOpenAI,
	}

	// Сохраняем в БД
	if err := m.SaveModel(userId, umcr, updated); err != nil {
		return fmt.Errorf("ошибка сохранения обновленной модели в БД: %w", err)
	}

	logger.Info("OpenAI Assistant успешно обновлен для пользователя %d", userId, userId)
	return nil
}

// filesEqual сравнивает два слайса файлов
// Используется для проверки изменились ли файлы при обновлении модели
func filesEqual(a, b []Ids) bool {
	if len(a) != len(b) {
		return false
	}

	aMap := make(map[string]string)
	for _, file := range a {
		aMap[file.ID] = file.Name
	}

	for _, file := range b {
		if name, exists := aMap[file.ID]; !exists || name != file.Name {
			return false
		}
	}

	return true
}
