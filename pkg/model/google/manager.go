package google

import (
	"fmt"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// CreateModel создаёт новую модель Google
func (m *GoogleModel) CreateModel(userId uint32, provider create.ProviderType, modelData *create.UniversalModelData, fileIDs []create.Ids) (create.UMCR, error) {
	// Создаем экземпляр universalModel для делегирования
	modelsManager := &create.UniversalModel{}

	return modelsManager.CreateModel(userId, provider, modelData, fileIDs)
}

// DeleteModel удаляет модель Google
func (m *GoogleModel) DeleteModel(userId uint32) error {
	if m.client == nil {
		return fmt.Errorf("google клиент не инициализирован")
	}

	// Получаем все модели пользователя
	userModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	// Ищем активную модель Google
	var activeModel *create.UserModelRecord
	for i := range userModels {
		if userModels[i].IsActive && userModels[i].Provider == create.ProviderGoogle {
			activeModel = &userModels[i]
			break
		}
	}

	if activeModel == nil {
		return fmt.Errorf("активная Google модель не найдена")
	}

	// Удаляем агента через клиент (это просто логирование для Google)
	if err := m.client.DeleteGoogleAgent(activeModel.AssistId); err != nil {
		logger.Warn("Ошибка удаления Google агента %s: %v", activeModel.AssistId, err)
	}

	// Удаление из БД происходит через modelsManager
	logger.Info("Модель Google удалена", userId)

	return nil
}

// UpdateModel обновляет модель Google
func (m *GoogleModel) UpdateModel(userId uint32, modelJSON []byte) error {
	if m.client == nil {
		return fmt.Errorf("google клиент не инициализирован")
	}

	// TODO: Реализовать обновление модели
	return fmt.Errorf("UpdateModel не реализован")
}

// ============================================================================
// VECTOR EMBEDDING METHODS - Делегирование к методам из files.go
// ============================================================================

// UploadDocumentWithEmbedding загружает документ и сохраняет эмбеддинг в MariaDB
// Автоматически использует modelId активной Google модели пользователя
func (m *GoogleModel) UploadDocumentWithEmbedding(userId uint32, docName, content string, metadata create.DocumentMetadata) (string, error) {
	// Получаем modelId активной Google модели
	modelId, err := m.getActiveModelId(userId)
	if err != nil {
		return "", fmt.Errorf("ошибка получения modelId: %w", err)
	}

	// 1. Генерируем эмбеддинг через Google API
	embedding, err := m.GenerateEmbedding(content)
	if err != nil {
		return "", fmt.Errorf("ошибка генерации эмбеддинга: %w", err)
	}

	// 2. Создаём уникальный ID
	docID := fmt.Sprintf("doc_%d_%d", modelId, time.Now().UnixNano())

	// 3. Сохраняем в MariaDB с привязкой к modelId
	err = m.saveEmbedding(userId, modelId, docID, docName, content, embedding, metadata)
	if err != nil {
		return "", fmt.Errorf("ошибка сохранения в БД: %w", err)
	}

	logger.Info("UploadDocumentWithEmbedding: документ '%s' загружен для modelId=%d, эмбеддинг сохранён в MariaDB (dim=%d)",
		docName, modelId, len(embedding))
	return docID, nil
}

// SearchSimilarDocuments ищет похожие документы в БД по эмбеддингу
// SearchSimilarDocuments ищет похожие документы по запросу через векторный поиск
func (m *GoogleModel) SearchSimilarDocuments(userId uint32, query string, limit int) ([]create.VectorDocument, error) {
	// Получаем modelId активной Google модели
	modelId, err := m.getActiveModelId(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения modelId: %w", err)
	}

	// Проверяем наличие эмбеддингов перед генерацией запроса
	// Это позволяет избежать лишних вызовов Google API если база пустая
	count, err := m.db.CountModelEmbeddings(modelId)
	if err != nil {
		return nil, fmt.Errorf("ошибка проверки наличия эмбеддингов: %w", err)
	}

	if count == 0 {
		// Нет эмбеддингов для поиска - возвращаем пустой массив без вызова API
		logger.Debug("SearchSimilarDocuments: нет эмбеддингов для modelId=%d, пропуск поиска", modelId)
		return []create.VectorDocument{}, nil
	}

	logger.Debug("SearchSimilarDocuments: найдено %d эмбеддингов для modelId=%d, выполняем поиск", count, modelId)

	// Генерируем эмбеддинг для поискового запроса
	queryEmbedding, err := m.GenerateEmbedding(query)
	if err != nil {
		return nil, fmt.Errorf("ошибка генерации эмбеддинга запроса: %w", err)
	}

	// Ищем похожие документы в БД
	return m.searchSimilarEmbeddings(modelId, queryEmbedding, limit)
}

// DeleteDocument удаляет документ из БД по docID
func (m *GoogleModel) DeleteDocument(userId uint32, docID string) error {
	// Получаем modelId активной Google модели
	modelId, err := m.getActiveModelId(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения modelId: %w", err)
	}

	return m.deleteDocument(modelId, docID)
}

// ListUserDocuments возвращает список документов модели из БД
func (m *GoogleModel) ListUserDocuments(userId uint32) ([]create.VectorDocument, error) {
	// Получаем modelId активной Google модели
	modelId, err := m.getActiveModelId(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения modelId: %w", err)
	}

	return m.listModelDocuments(modelId)
}

// getActiveModelId получает modelId активной Google модели пользователя
func (m *GoogleModel) getActiveModelId(userId uint32) (uint64, error) {
	// Получаем все модели пользователя и находим Google модель
	allModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		logger.Error("getActiveModelId: ошибка GetAllUserModels для userId=%d: %v", userId, err)
		return 0, fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	// Находим Google модель
	var model *create.UserModelRecord
	for i := range allModels {
		if allModels[i].Provider == create.ProviderGoogle {
			model = &allModels[i]
			break
		}
	}

	if model == nil {
		logger.Error("getActiveModelId: Google модель не найдена", userId)
		logger.Debug("getActiveModelId: всего моделей", len(allModels))
		for i, m := range allModels {
			logger.Debug("  Модель %d: ID=%d, Provider=%d, IsActive=%v", i+1, m.ModelId, m.Provider, m.IsActive)
		}
		return 0, fmt.Errorf("Google модель не найдена", userId)
	}

	logger.Debug("getActiveModelId: найдена Google модель с ModelId=%d", model.ModelId)
	return model.ModelId, nil
}
