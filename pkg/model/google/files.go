package google

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// ============================================================================
// GOOGLE GEMINI + MARIADB VECTOR STORAGE
// - Embedding API: генерация эмбеддингов через Google (text-embedding-004, 768 dim)
// - Vector Storage: хранение эмбеддингов в MariaDB 12
// - Similarity Search: поиск по косинусному сходству в БД
// ============================================================================
func (m *GoogleModel) DeleteTempFile(fileID string) error {
	if m.client == nil {
		return fmt.Errorf("google клиент не инициализирован")
	}
	if fileID == "" {
		return fmt.Errorf("fileID не может быть пустым")
	}
	err := m.client.DeleteAudioFile(fileID)
	if err != nil {
		logger.Warn("DeleteTempFile: ошибка удаления файла %s: %v", fileID, err)
		return err
	}
	//logger.Debug("DeleteTempFile: файл %s успешно удалён", fileID)
	return nil
}

func (m *GoogleModel) GetFileAsReader(userId uint32, url string) (io.Reader, error) {
	if url == "" {
		return nil, fmt.Errorf("не указан источник файла")
	}
	if strings.HasPrefix(url, "google_file:") {
		fileURI := strings.TrimPrefix(url, "google_file:")
		content, err := m.downloadFileFromGoogle(fileURI)
		if err != nil {
			return nil, fmt.Errorf("ошибка получения файла из Google File API: %w", err)
		}
		return bytes.NewReader(content), nil
	}
	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка подготовки запроса: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки файла: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("ошибка HTTP: статус %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func (m *GoogleModel) downloadFileFromGoogle(fileURI string) ([]byte, error) {
	if m.client == nil {
		return nil, fmt.Errorf("google client не инициализирован")
	}
	downloadURL := fmt.Sprintf("%s?key=%s", fileURI, m.client.GetAPIKey())
	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания запроса: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения содержимого: %v", err)
	}
	//logger.Debug("Файл скачан из Google File API, размер: %d bytes", len(content))
	return content, nil
}

// ============================================================================
// EMBEDDING API - Генерация эмбеддингов через Google
// ============================================================================

// GenerateEmbedding генерирует векторный эмбеддинг для текста через Google Embedding API
// Использует модель text-embedding-004 (768 dimensions)
// Возвращает []float32 с эмбеддингом или ошибку
//
// ПРИМЕЧАНИЕ: Использует функцию create.GenerateGoogleEmbedding() из пакета create
// для избежания дублирования кода с GoogleAgentClient.GenerateEmbedding()
//
// Используется внутри UploadDocumentWithEmbedding, SearchSimilarDocuments и других публичных методов GoogleModel
func (m *GoogleModel) GenerateEmbedding(text string) ([]float32, error) {
	return create.GenerateGoogleEmbedding(m.ctx, m.client.GetAPIKey(), text)
}

// ============================================================================
// VECTOR STORAGE - Работа с эмбеддингами в MariaDB
// ============================================================================

func (m *GoogleModel) deleteDocument(modelId uint64, docID string) error {
	return m.db.DeleteEmbedding(modelId, docID)
}

func (m *GoogleModel) listModelDocuments(modelId uint64) ([]create.VectorDocument, error) {
	return m.db.ListModelEmbeddings(modelId)
}

func (m *GoogleModel) searchSimilarEmbeddings(modelId uint64, queryEmbedding []float32, limit int) ([]create.VectorDocument, error) {
	return m.db.SearchSimilarEmbeddings(modelId, queryEmbedding, limit)
}

func (m *GoogleModel) saveEmbedding(userId uint32, modelId uint64, docID, docName, content string, embedding []float32, metadata create.DocumentMetadata) error {
	return m.db.SaveEmbedding(userId, modelId, docID, docName, content, embedding, metadata)
}
