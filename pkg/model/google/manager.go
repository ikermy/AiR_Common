package google

import (
	"fmt"
	"time"

	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// CreateModel создаёт новую модель Google
func (m *Model) CreateModel(userID uint32, provider create.ProviderType, modelData *create.UniversalModelData, fileIDs []create.Ids) (create.UMCR, error) {
	// Создаем экземпляр universalModel для делегирования
	modelsManager := &create.UniversalModel{}

	return modelsManager.CreateModel(userID, provider, modelData, fileIDs)
}

// ============================================================================
// VECTOR EMBEDDING METHODS - Делегирование к методам из files.go
// ============================================================================

// UploadDocumentWithEmbedding загружает документ и сохраняет эмбеддинг в MariaDB
// Автоматически использует modelId активной Google модели пользователя
func (m *Model) UploadDocumentWithEmbedding(userID uint32, docName, content string, metadata create.DocumentMetadata) (string, error) {
	// Получаем modelId активной Google модели
	modelId, err := m.getActiveModelId(userID)
	if err != nil {
		return "", fmt.Errorf("ошибка получения modelId: %w", err)
	}

	// 1. Генерируем эмбеддинг через Google API
	embedding, err := m.GenerateEmbedding(userID, content)
	if err != nil {
		return "", fmt.Errorf("ошибка генерации эмбеддинга: %w", err)
	}

	// 2. Создаём уникальный ID с префиксом google_doc_ для автоопределения провайдера
	docID := fmt.Sprintf("google_doc_%d_%d", userID, time.Now().Unix())

	// 3. Сохраняем в MariaDB с привязкой к modelId
	err = m.saveEmbedding(userID, modelId, docID, docName, content, embedding, metadata)
	if err != nil {
		return "", fmt.Errorf("ошибка сохранения в БД: %w", err)
	}

	//logger.Debug("UploadDocumentWithEmbedding: документ '%s' загружен для modelId=%d, эмбеддинг сохранён в MariaDB (dim=%d)",
	//	docName, modelId, len(embedding))
	return docID, nil
}

// SearchSimilarDocuments ищет похожие документы по запросу через векторный поиск
func (m *Model) SearchSimilarDocuments(userID uint32, query string, limit int) ([]create.VectorDocument, error) {
	// Получаем modelId активной Google модели
	modelId, err := m.getActiveModelId(userID)
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
		//logger.Debug("SearchSimilarDocuments: нет эмбеддингов для modelId=%d, пропуск поиска", modelId)
		return []create.VectorDocument{}, nil
	}

	//logger.Debug("SearchSimilarDocuments: найдено %d эмбеддингов для modelId=%d, выполняем поиск", count, modelId)

	// Генерируем эмбеддинг для поискового запроса
	queryEmbedding, err := m.GenerateEmbedding(userID, query)
	if err != nil {
		return nil, fmt.Errorf("ошибка генерации эмбеддинга запроса: %w", err)
	}

	// Ищем похожие документы в БД
	return m.searchSimilarEmbeddings(modelId, queryEmbedding, limit)
}

// DeleteDocument удаляет документ из БД по docID
func (m *Model) DeleteDocument(userID uint32, docID string) error {
	// Получаем modelId активной Google модели
	modelId, err := m.getActiveModelId(userID)
	if err != nil {
		return fmt.Errorf("ошибка получения modelId: %w", err)
	}

	return m.deleteDocument(modelId, docID)
}

// ListUserDocuments возвращает список документов модели из БД
func (m *Model) ListUserDocuments(userID uint32) ([]create.VectorDocument, error) {
	// Получаем modelId активной Google модели
	modelId, err := m.getActiveModelId(userID)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения modelId: %w", err)
	}

	return m.listModelDocuments(modelId)
}

// getActiveModelId получает modelId активной Google модели пользователя
func (m *Model) getActiveModelId(userID uint32) (uint64, error) {
	// Получаем все модели пользователя и находим Google модель
	allModels, err := m.db.GetAllUserModels(userID)
	if err != nil {
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
		//logger.Error("getActiveModelId: Google модель не найдена", userID)
		//for i, m := range allModels {
		//	logger.Debug("  Модель %d: ID=%d, Provider=%d, IsActive=%v", i+1, m.ModelId, m.Provider, m.IsActive)
		//}
		return 0, fmt.Errorf("Google модель не найдена для пользователя %d", userID)
	}

	//logger.Debug("getActiveModelId: найдена Google модель с ModelId=%d", model.ModelId)
	return model.ModelId, nil
}
