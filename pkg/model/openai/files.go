package openai

import (
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// ============================================================================
// OPENAI EMBEDDINGS + MARIADB VECTOR STORAGE
// - Embedding API: генерация эмбеддингов через OpenAI (text-embedding-3-small, 512 dim)
// - Vector Storage: хранение эмбеддингов в MariaDB 12
// - Similarity Search: поиск по косинусному сходству в БД
// ============================================================================

// GenerateEmbedding генерирует векторный эмбеддинг для текста через OpenAI Embeddings API
// Использует модель text-embedding-3-small (512 dimensions)
// Возвращает []float32 с эмбеддингом или ошибку
//
// ПРИМЕЧАНИЕ: Использует функцию create.GenerateOpenAIEmbedding() из пакета create
// для избежания дублирования кода с OpenAIAgentClient.GenerateEmbedding()
//
// Используется внутри UploadDocumentWithEmbedding, SearchSimilarDocuments и других публичных методов OpenAIModel
func (m *Model) GenerateEmbedding(text string) ([]float32, error) {
	return create.GenerateOpenAIEmbedding(m.ctx, m.client.GetAPIKey(), text)
}

// ============================================================================
// VECTOR STORAGE - Работа с эмбеддингами в MariaDB
// ============================================================================

func (m *Model) deleteDocument(modelId uint64, docID string) error {
	return m.db.DeleteEmbedding(modelId, docID)
}

func (m *Model) listModelDocuments(modelId uint64) ([]create.VectorDocument, error) {
	return m.db.ListModelEmbeddings(modelId, create.ProviderOpenAI)
}

func (m *Model) searchSimilarEmbeddings(modelId uint64, queryEmbedding []float32, limit int) ([]create.VectorDocument, error) {
	return m.db.SearchSimilarEmbeddings(modelId, create.ProviderOpenAI, queryEmbedding, limit)
}

func (m *Model) saveEmbedding(userID uint32, modelId uint64, docID, docName, content string, embedding []float32, metadata create.DocumentMetadata) error {
	return m.db.SaveEmbedding(userID, modelId, create.ProviderOpenAI, docID, docName, content, embedding, metadata)
}
