package comdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/crypto"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// SaveEmbedding сохраняет эмбеддинг документа в MariaDB с привязкой к модели
// Поддерживает динамические размерности: 512 (OpenAI small), 768 (Google), 1536 (OpenAI medium), 3072 (OpenAI large)
// Использует нативный тип VECTOR(3072) с padding нулями для эффективного хранения
func (d *DB) SaveEmbedding(userID uint32, modelId uint64, provider create.ProviderType, docID, docName, content string, embedding []float32, metadata create.DocumentMetadata) error {
	ctx, cancel := context.WithTimeout(d.MainCTX(), time.Duration(sqlTimeToCancel)*time.Second)
	defer cancel()

	// Валидация размерности (поддержка Google 768 и OpenAI 512/1536/3072)
	embeddingDim := len(embedding)
	if embeddingDim != 512 && embeddingDim != 768 && embeddingDim != 1536 && embeddingDim != 3072 {
		return fmt.Errorf("неподдерживаемая размерность эмбеддинга: %d (допустимо: 512, 768, 1536, 3072)", embeddingDim)
	}

	// Дополняем нулями до 3072 для совместимости с VECTOR(3072)
	paddedEmbedding := make([]float32, 3072)
	copy(paddedEmbedding, embedding)

	// Конвертируем []float32 в строку для VECTOR(3072)
	// MariaDB VECTOR принимает формат: '[0.1, 0.2, 0.3, ...]'
	embeddingStr := vectorToString(paddedEmbedding)

	// Конвертируем метаданные в JSON
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("ошибка сериализации метаданных: %w", err)
	}

	// Если настроен мастер-ключ, шифруем имя документа и его содержание перед сохранением
	if d.MasterKeyResolver != nil {
		if mk, ok := d.MasterKeyResolver(userID); ok {
			if !crypto.IsEncryptedWithMasterKey(docName) {
				if enc, err := crypto.EncryptFieldWithMasterKey(mk, docName); err == nil {
					docName = enc
				}
			}
			if !crypto.IsEncryptedWithMasterKey(content) {
				if enc, err := crypto.EncryptFieldWithMasterKey(mk, content); err == nil {
					content = enc
				}
			}
		}
	}

	query := `INSERT INTO vector_embeddings (user_id, model_id, provider, doc_id, doc_name, content, embedding, embedding_dim, metadata)
             VALUES (?, ?, ?, ?, ?, ?, VEC_FromText(?), ?, ?)
             ON DUPLICATE KEY UPDATE 
                 provider = VALUES(provider),
                 doc_name = VALUES(doc_name),
                 content = VALUES(content),
                 embedding = VALUES(embedding),
                 embedding_dim = VALUES(embedding_dim),
                 metadata = VALUES(metadata)`

	_, err = d.Conn().ExecContext(ctx, query, userID, modelId, provider.String(), docID, docName, content, embeddingStr, embeddingDim, metadataJSON)
	if err != nil {
		return fmt.Errorf("SaveEmbedding: ошибка сохранения эмбеддинга для modelId=%d, docID=%s: %v", modelId, docID, err)
	}

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

	return nil
}

// DeleteAllModelEmbeddings удаляет все эмбеддинги конкретной модели
func (d *DB) DeleteAllModelEmbeddings(modelId uint64) error {
	ctx, cancel := context.WithTimeout(d.MainCTX(), time.Duration(sqlTimeToCancel)*time.Second)
	defer cancel()

	query := `DELETE FROM vector_embeddings WHERE model_id = ?`
	_, err := d.Conn().ExecContext(ctx, query, modelId)
	if err != nil {
		return fmt.Errorf("ошибка удаления эмбеддингов модели: %w", err)
	}

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

// ListModelEmbeddings возвращает список всех эмбеддингов для модели с обрезкой padding
// Читает реальную размерность из embedding_dim и обрезает вектор
func (d *DB) ListModelEmbeddings(modelId uint64, provider create.ProviderType) ([]create.VectorDocument, error) {
	ctx, cancel := context.WithTimeout(d.MainCTX(), time.Duration(sqlTimeToCancel)*time.Second)
	defer cancel()

	// Конвертируем ProviderType в строку для фильтрации
	providerStr := provider.String()

	query := `SELECT user_id, provider, doc_id, doc_name, content, VEC_ToText(embedding) as embedding_text, embedding_dim, metadata, created_at 
	          FROM vector_embeddings 
	          WHERE model_id = ? AND provider = ?
	          ORDER BY created_at DESC`

	rows, err := d.Conn().QueryContext(ctx, query, modelId, providerStr)
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
		var provider sql.NullString
		var embeddingDim int

		err := rows.Scan(&doc.UserID, &provider, &doc.ID, &doc.Name, &doc.Content, &embeddingStr, &embeddingDim, &metadataJSON, &doc.CreatedAt)
		if err != nil {
			continue
		}

		// Парсим VECTOR в []float32
		fullEmbedding, err := stringToVector(embeddingStr)
		if err != nil {
			continue
		}

		// Обрезаем до реальной размерности (убираем padding)
		if embeddingDim > 0 && embeddingDim <= len(fullEmbedding) {
			doc.Embedding = fullEmbedding[:embeddingDim]
		} else {
			doc.Embedding = fullEmbedding
		}

		if len(metadataJSON) > 0 {
			if err := json.Unmarshal(metadataJSON, &doc.Metadata); err != nil {
				return nil, fmt.Errorf("ListModelEmbeddings: ошибка десериализации метаданных: %v", err)
			}
		}

		// РАСШИФРОВКА
		if d.MasterKeyResolver != nil {
			if mk, ok := d.MasterKeyResolver(doc.UserID); ok {
				if crypto.IsEncryptedWithMasterKey(doc.Name) {
					if decomp, err := crypto.DecryptFieldWithMasterKey(mk, doc.Name); err == nil {
						doc.Name = decomp
					}
				}
				if crypto.IsEncryptedWithMasterKey(doc.Content) {
					if decomp, err := crypto.DecryptFieldWithMasterKey(mk, doc.Content); err == nil {
						doc.Content = decomp
					}
				}
			}
		}

		documents = append(documents, doc)
	}

	return documents, nil
}

// SearchSimilarEmbeddings ищет похожие эмбеддинги в рамках конкретной модели используя нативную функцию VEC_Distance_Cosine в MariaDB 12
// Поддерживает динамические размерности: сравнивает только первые N измерений вектора согласно embedding_dim
// Фильтрует по provider для поиска только среди документов своего провайдера
// Это намного быстрее чем вычисление в Go, т.к. выполняется на уровне БД с векторными индексами
func (d *DB) SearchSimilarEmbeddings(modelId uint64, provider create.ProviderType, queryEmbedding []float32, limit int) ([]create.VectorDocument, error) {
	ctx, cancel := context.WithTimeout(d.MainCTX(), time.Duration(sqlTimeToCancel)*time.Second)
	defer cancel()

	// Валидация размерности
	queryDim := len(queryEmbedding)
	if queryDim != 512 && queryDim != 768 && queryDim != 1536 && queryDim != 3072 {
		return nil, fmt.Errorf("неподдерживаемая размерность эмбеддинга запроса: %d (допустимо: 512, 768, 1536, 3072)", queryDim)
	}

	// Дополняем нулями до 3072 для совместимости с VECTOR(3072)
	paddedQuery := make([]float32, 3072)
	copy(paddedQuery, queryEmbedding)

	// Конвертируем queryEmbedding в строку для VECTOR
	queryStr := vectorToString(paddedQuery)

	// Конвертируем ProviderType в строку для фильтрации
	providerStr := provider.String()

	// Используем нативную функцию VEC_Distance_Cosine для вычисления сходства
	// Фильтруем по размерности и провайдеру для корректного сравнения
	// Чем меньше distance, тем больше похожи векторы
	// Косинусное расстояние = 1 - косинусное сходство
	// Поэтому сортируем по возрастанию distance
	query := `SELECT 
                user_id,
                provider,
                doc_id, 
                doc_name, 
                content, 
                VEC_ToText(embedding) as embedding_text,
                embedding_dim,
                metadata, 
                created_at,
                VEC_Distance_Cosine(embedding, VEC_FromText(?)) as distance
              FROM vector_embeddings 
              WHERE model_id = ? AND embedding_dim = ? AND provider = ?
              ORDER BY distance ASC
              LIMIT ?`

	rows, err := d.Conn().QueryContext(ctx, query, queryStr, modelId, queryDim, providerStr, limit)
	if err != nil {
		return nil, fmt.Errorf("ошибка поиска похожих эмбеддингов: %w", err)
	}
	defer rows.Close()

	var documents []create.VectorDocument
	for rows.Next() {
		var doc create.VectorDocument
		var embeddingStr string
		var metadataJSON []byte
		var provider sql.NullString
		var embeddingDim int
		var distance float32 // Не используется в результате, но нужен для Scan

		err := rows.Scan(&doc.UserID, &provider, &doc.ID, &doc.Name, &doc.Content, &embeddingStr, &embeddingDim, &metadataJSON, &doc.CreatedAt, &distance)
		if err != nil {
			continue
		}

		// Парсим VECTOR в []float32
		fullEmbedding, err := stringToVector(embeddingStr)
		if err != nil {
			continue
		}

		// Обрезаем до реальной размерности (убираем padding)
		if embeddingDim > 0 && embeddingDim <= len(fullEmbedding) {
			doc.Embedding = fullEmbedding[:embeddingDim]
		} else {
			doc.Embedding = fullEmbedding
		}

		if len(metadataJSON) > 0 {
			if err := json.Unmarshal(metadataJSON, &doc.Metadata); err != nil {
				return nil, fmt.Errorf("SearchSimilarEmbeddings: ошибка десериализации метаданных: %v", err)
			}
		}

		// РАСШИФРОВКА
		if d.MasterKeyResolver != nil {
			if mk, ok := d.MasterKeyResolver(doc.UserID); ok {
				if crypto.IsEncryptedWithMasterKey(doc.Name) {
					if decomp, err := crypto.DecryptFieldWithMasterKey(mk, doc.Name); err == nil {
						doc.Name = decomp
					}
				}
				if crypto.IsEncryptedWithMasterKey(doc.Content) {
					if decomp, err := crypto.DecryptFieldWithMasterKey(mk, doc.Content); err == nil {
						doc.Content = decomp
					}
				}
			}
		}

		documents = append(documents, doc)
	}

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
