package openai

import (
	"fmt"
	"time"

	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// CreateModel создаёт новую модель OpenAI
// Делегирует вызов к UniversalModel из пакета create
func (m *Model) CreateModel(userId uint32, provider create.ProviderType, modelData *create.UniversalModelData, fileIDs []create.Ids) (create.UMCR, error) {
	// Создаем экземпляр UniversalModel для делегирования
	modelsManager := &create.UniversalModel{}

	return modelsManager.CreateModel(userId, provider, modelData, fileIDs)
}

// ============================================================================
// Vector Embedding методы (OpenAI Embeddings API + MariaDB)
// ============================================================================

// UploadDocumentWithEmbedding загружает документ с генерацией эмбеддинга
func (m *Model) UploadDocumentWithEmbedding(userId uint32, docName, content string, metadata create.DocumentMetadata) (string, error) {
	// Получаем modelId из БД
	modelId, err := m.getModelId(userId)
	if err != nil {
		return "", fmt.Errorf("ошибка получения modelId: %w", err)
	}

	// Генерируем уникальный ID для документа
	docID := fmt.Sprintf("openai_doc_%d_%d", userId, time.Now().Unix())

	// 1. Генерируем эмбеддинг через OpenAI Embeddings API
	//logger.Debug("OpenAI: генерация эмбеддинга для документа: %s", docName, userId)
	embedding, err := m.GenerateEmbedding(content)
	if err != nil {
		return "", fmt.Errorf("ошибка генерации эмбеддинга: %w", err)
	}

	// 2. Сохраняем эмбеддинг в MariaDB
	err = m.saveEmbedding(userId, modelId, docID, docName, content, embedding, metadata)
	if err != nil {
		return "", fmt.Errorf("ошибка сохранения эмбеддинга: %w", err)
	}

	//logger.Debug("OpenAI: документ успешно загружен docID=%s, docName=%s, embeddingDim=%d",
	//	docID, docName, len(embedding), userId)

	return docID, nil
}

// DeleteDocument удаляет документ из БД по docID
func (m *Model) DeleteDocument(userId uint32, docID string) error {
	modelId, err := m.getModelId(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения modelId: %w", err)
	}

	//logger.Debug("OpenAI: удаление документа docID=%s", docID, userId)
	return m.deleteDocument(modelId, docID)
}

// ListUserDocuments возвращает список документов модели из БД
func (m *Model) ListUserDocuments(userId uint32) ([]create.VectorDocument, error) {
	modelId, err := m.getModelId(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения modelId: %w", err)
	}

	documents, err := m.listModelDocuments(modelId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения списка документов: %w", err)
	}

	//logger.Debug("OpenAI: получено %d документов", len(documents), userId)
	return documents, nil
}

// SearchSimilarDocuments ищет похожие документы используя семантический поиск
func (m *Model) SearchSimilarDocuments(userId uint32, query string, limit int) ([]create.VectorDocument, error) {
	modelId, err := m.getModelId(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения modelId: %w", err)
	}

	// 1. Генерируем эмбеддинг для поискового запроса
	//logger.Debug("OpenAI: генерация эмбеддинга для поиска: %s", query, userId)
	queryEmbedding, err := m.GenerateEmbedding(query)
	if err != nil {
		return nil, fmt.Errorf("ошибка генерации эмбеддинга запроса: %w", err)
	}

	// 2. Ищем похожие документы в БД используя косинусное сходство
	documents, err := m.searchSimilarEmbeddings(modelId, queryEmbedding, limit)
	if err != nil {
		return nil, fmt.Errorf("ошибка поиска похожих документов: %w", err)
	}

	//logger.Debug("OpenAI: найдено %d похожих документов", len(documents), userId)
	return documents, nil
}

// getModelId получает modelId пользователя из БД (для работы с vector_embeddings)
func (m *Model) getModelId(userId uint32) (uint64, error) {
	// Получаем запись модели OpenAI для пользователя из БД
	record, err := m.db.GetModelByProviderAnyStatus(userId, create.ProviderOpenAI)
	if err != nil {
		return 0, fmt.Errorf("ошибка получения модели OpenAI из БД: %w", err)
	}

	if record == nil {
		return 0, fmt.Errorf("модель OpenAI не найдена для пользователя %d", userId)
	}

	return record.ModelId, nil
}
