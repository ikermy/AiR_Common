package comdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
)

// SaveEmbedding сохраняет эмбеддинг документа в MariaDB с привязкой к модели
// Использует нативный тип VECTOR(768) для эффективного хранения
func (d *DB) SaveEmbedding(userId uint32, modelId uint64, docID, docName, content string, embedding []float32, metadata create.DocumentMetadata) error {
	ctx, cancel := context.WithTimeout(d.MainCTX(), time.Duration(sqlTimeToCancel)*time.Second)
	defer cancel()

	// Валидация размерности
	if len(embedding) != 768 {
		return fmt.Errorf("неверная размерность эмбеддинга: ожидается 768, получено %d", len(embedding))
	}

	// Конвертируем []float32 в строку для VECTOR(768)
	// MariaDB VECTOR принимает формат: '[0.1, 0.2, 0.3, ...]'
	embeddingStr := vectorToString(embedding)

	// Конвертируем метаданные в JSON
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("ошибка сериализации метаданных: %w", err)
	}

	query := `INSERT INTO vector_embeddings (user_id, model_id, doc_id, doc_name, content, embedding, metadata)
             VALUES (?, ?, ?, ?, ?, VEC_FromText(?), ?)
             ON DUPLICATE KEY UPDATE 
                 doc_name = VALUES(doc_name),
                 content = VALUES(content),
                 embedding = VALUES(embedding),
                 metadata = VALUES(metadata)`

	_, err = d.Conn().ExecContext(ctx, query, userId, modelId, docID, docName, content, embeddingStr, metadataJSON)
	if err != nil {
		logger.Error("SaveEmbedding: ошибка сохранения эмбеддинга для modelId=%d, docID=%s: %v", modelId, docID, err)
		return fmt.Errorf("ошибка сохранения эмбеддинга: %w", err)
	}

	logger.Debug("SaveEmbedding: сохранён эмбеддинг для modelId=%d, docID=%s, размерность=%d", modelId, docID, len(embedding))
	return nil
}

// GetEmbedding получает эмбеддинг документа по ID модели и docID
// Читает из нативного типа VECTOR(768)
func (d *DB) GetEmbedding(modelId uint64, docID string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(d.MainCTX(), time.Duration(sqlTimeToCancel)*time.Second)
	defer cancel()

	var embeddingStr string
	query := `SELECT VEC_ToText(embedding) FROM vector_embeddings WHERE model_id = ? AND doc_id = ?`

	err := d.Conn().QueryRowContext(ctx, query, modelId, docID).Scan(&embeddingStr)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("эмбеддинг не найден для docID=%s", docID)
		}
		return nil, fmt.Errorf("ошибка получения эмбеддинга: %w", err)
	}

	// Парсим строку '[0.1, 0.2, ...]' в []float32
	embedding, err := stringToVector(embeddingStr)
	if err != nil {
		return nil, fmt.Errorf("ошибка парсинга эмбеддинга: %w", err)
	}

	return embedding, nil
}

// DeleteEmbedding удаляет эмбеддинг документа по model_id и doc_id
func (d *DB) DeleteEmbedding(modelId uint64, docID string) error {
	ctx, cancel := context.WithTimeout(d.MainCTX(), time.Duration(sqlTimeToCancel)*time.Second)
	defer cancel()

	query := `DELETE FROM vector_embeddings WHERE model_id = ? AND doc_id = ?`
	result, err := d.Conn().ExecContext(ctx, query, modelId, docID)
	if err != nil {
		return fmt.Errorf("ошибка удаления эмбеддинга: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("эмбеддинг не найден для удаления: docID=%s", docID)
	}

	logger.Debug("DeleteEmbedding: удалён эмбеддинг modelId=%d, docID=%s", modelId, docID)
	return nil
}

// DeleteAllModelEmbeddings удаляет все эмбеддинги конкретной модели
func (d *DB) DeleteAllModelEmbeddings(modelId uint64) error {
	ctx, cancel := context.WithTimeout(d.MainCTX(), time.Duration(sqlTimeToCancel)*time.Second)
	defer cancel()

	query := `DELETE FROM vector_embeddings WHERE model_id = ?`
	result, err := d.Conn().ExecContext(ctx, query, modelId)
	if err != nil {
		return fmt.Errorf("ошибка удаления эмбеддингов модели: %w", err)
	}

	rowsAffected, _ := result.RowsAffected()
	logger.Debug("DeleteAllModelEmbeddings: удалено %d эмбеддингов для modelId=%d", rowsAffected, modelId)
	return nil
}

// CountModelEmbeddings возвращает количество эмбеддингов конкретной модели
func (d *DB) CountModelEmbeddings(modelId uint64) (int, error) {
	ctx, cancel := context.WithTimeout(d.MainCTX(), time.Duration(sqlTimeToCancel)*time.Second)
	defer cancel()

	var count int
	query := `SELECT COUNT(*) FROM vector_embeddings WHERE model_id = ?`

	err := d.Conn().QueryRowContext(ctx, query, modelId).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("ошибка подсчета эмбеддингов для modelId=%d: %w", modelId, err)
	}

	return count, nil
}

// ListModelEmbeddings возвращает список всех эмбеддингов конкретной модели
func (d *DB) ListModelEmbeddings(modelId uint64) ([]create.VectorDocument, error) {
	ctx, cancel := context.WithTimeout(d.MainCTX(), time.Duration(sqlTimeToCancel)*time.Second)
	defer cancel()

	query := `SELECT user_id, doc_id, doc_name, content, VEC_ToText(embedding), metadata, created_at 
           FROM vector_embeddings 
           WHERE model_id = ? 
           ORDER BY created_at DESC`

	rows, err := d.Conn().QueryContext(ctx, query, modelId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения списка эмбеддингов: %w", err)
	}
	defer rows.Close()

	// Инициализируем пустой срез, чтобы JSON возвращал [] вместо null
	documents := make([]create.VectorDocument, 0)
	for rows.Next() {
		var doc create.VectorDocument
		var embeddingStr string
		var metadataJSON []byte

		err := rows.Scan(&doc.UserID, &doc.ID, &doc.Name, &doc.Content, &embeddingStr, &metadataJSON, &doc.CreatedAt)
		if err != nil {
			logger.Warn("ListModelEmbeddings: ошибка сканирования строки: %v", err)
			continue
		}

		// Парсим VECTOR в []float32
		doc.Embedding, err = stringToVector(embeddingStr)
		if err != nil {
			logger.Warn("ListModelEmbeddings: ошибка парсинга эмбеддинга: %v", err)
			continue
		}

		if len(metadataJSON) > 0 {
			if err := json.Unmarshal(metadataJSON, &doc.Metadata); err != nil {
				logger.Warn("ListModelEmbeddings: ошибка десериализации метаданных: %v", err)
			}
		}

		documents = append(documents, doc)
	}

	return documents, nil
}

// SearchSimilarEmbeddings ищет похожие эмбеддинги в рамках конкретной модели используя нативную функцию VEC_Distance_Cosine в MariaDB 12
// Это намного быстрее чем вычисление в Go, т.к. выполняется на уровне БД с векторными индексами
func (d *DB) SearchSimilarEmbeddings(modelId uint64, queryEmbedding []float32, limit int) ([]create.VectorDocument, error) {
	ctx, cancel := context.WithTimeout(d.MainCTX(), time.Duration(sqlTimeToCancel)*time.Second)
	defer cancel()

	// Валидация размерности
	if len(queryEmbedding) != 768 {
		return nil, fmt.Errorf("неверная размерность эмбеддинга запроса: ожидается 768, получено %d", len(queryEmbedding))
	}

	// Конвертируем queryEmbedding в строку для VECTOR
	queryStr := vectorToString(queryEmbedding)

	// Используем нативную функцию VEC_Distance_Cosine для вычисления сходства
	// Чем меньше distance, тем больше похожи векторы
	// Косинусное расстояние = 1 - косинусное сходство
	// Поэтому сортируем по возрастанию distance
	query := `SELECT 
                user_id,
                doc_id, 
                doc_name, 
                content, 
                VEC_ToText(embedding) as embedding_text,
                metadata, 
                created_at,
                VEC_Distance_Cosine(embedding, VEC_FromText(?)) as distance
              FROM vector_embeddings 
              WHERE model_id = ?
              ORDER BY distance ASC
              LIMIT ?`

	rows, err := d.Conn().QueryContext(ctx, query, queryStr, modelId, limit)
	if err != nil {
		return nil, fmt.Errorf("ошибка поиска похожих эмбеддингов: %w", err)
	}
	defer rows.Close()

	var documents []create.VectorDocument
	for rows.Next() {
		var doc create.VectorDocument
		var embeddingStr string
		var metadataJSON []byte
		var distance float32 // Не используется в результате, но нужен для Scan

		err := rows.Scan(&doc.UserID, &doc.ID, &doc.Name, &doc.Content, &embeddingStr, &metadataJSON, &doc.CreatedAt, &distance)
		if err != nil {
			logger.Warn("SearchSimilarEmbeddings: ошибка сканирования строки: %v", err)
			continue
		}

		// Парсим VECTOR в []float32
		doc.Embedding, err = stringToVector(embeddingStr)
		if err != nil {
			logger.Warn("SearchSimilarEmbeddings: ошибка парсинга эмбеддинга: %v", err)
			continue
		}

		if len(metadataJSON) > 0 {
			if err := json.Unmarshal(metadataJSON, &doc.Metadata); err != nil {
				logger.Warn("SearchSimilarEmbeddings: ошибка десериализации метаданных: %v", err)
			}
		}

		documents = append(documents, doc)
	}

	logger.Debug("SearchSimilarEmbeddings: найдено %d похожих документов для modelId=%d (используя VEC_Distance_Cosine)", len(documents), modelId)
	return documents, nil
}

// cosineSimilarity вычисляет косинусное сходство между двумя векторами
func cosineSimilarity(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var dotProduct, normA, normB float32
	for i := 0; i < len(a); i++ {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dotProduct / (float32(math.Sqrt(float64(normA))) * float32(math.Sqrt(float64(normB))))
}

// ============================================================================
// HELPER FUNCTIONS - Конвертация между []float32 и VECTOR(768)
// ============================================================================

// vectorToString конвертирует []float32 в строку формата '[0.1, 0.2, ...]'
// для использования с VEC_FromText() в MariaDB
func vectorToString(vec []float32) string {
	if len(vec) == 0 {
		return "[]"
	}

	result := "["
	for i, v := range vec {
		if i > 0 {
			result += ","
		}
		result += fmt.Sprintf("%.9g", v) // 9 значащих цифр для float32
	}
	result += "]"
	return result
}

// stringToVector парсит строку '[0.1, 0.2, ...]' в []float32
// используется при чтении из VEC_ToText()
func stringToVector(s string) ([]float32, error) {
	// Удаляем пробелы и проверяем формат
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("неверный формат вектора: %s", s)
	}

	// Убираем скобки
	s = s[1 : len(s)-1]
	if s == "" {
		return []float32{}, nil
	}

	// Разбиваем по запятым
	parts := strings.Split(s, ",")
	result := make([]float32, len(parts))

	for i, part := range parts {
		part = strings.TrimSpace(part)
		var val float64
		_, err := fmt.Sscanf(part, "%f", &val)
		if err != nil {
			return nil, fmt.Errorf("ошибка парсинга значения '%s': %w", part, err)
		}
		result[i] = float32(val)
	}

	return result, nil
}
