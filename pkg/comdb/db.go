package comdb

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"time"
)

type CreatorType uint8

type Message struct {
	Creator   CreatorType `json:"creator"`
	Message   string      `json:"message"`
	Timestamp time.Time   `json:"timestamp"`
}
type Espero struct {
	Limit  uint16 `json:"limit"`
	Wait   uint8  `json:"wait"`
	Ignore bool   `json:"ignore"`
}

const (
	AI        CreatorType = 1
	User      CreatorType = 2
	UserVoice CreatorType = 3
)

// DecompressAndExtractMetadata Функция для распаковки сжатых данных и извлечения полей Meta и MetaAction
func DecompressAndExtractMetadata(compressedData []byte) (string, []string, *Espero, error) {
	// Создаем reader для распаковки данных
	gzipReader, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return "", nil, nil, fmt.Errorf("ошибка при создании gzip reader: %w", err)
	}
	defer func(gzipReader *gzip.Reader) {
		err := gzipReader.Close()
		if err != nil {
			log.Printf("ошибка закрытия gzip reader: %v", err)
		}
	}(gzipReader)

	// Читаем распакованные данные
	decompressedData, err := io.ReadAll(gzipReader)
	if err != nil {
		return "", nil, nil, fmt.Errorf("ошибка при распаковке данных: %w", err)
	}

	// Разбираем JSON
	var modelData map[string]interface{}
	if err := json.Unmarshal(decompressedData, &modelData); err != nil {
		return "", nil, nil, fmt.Errorf("ошибка при разборе JSON модели: %w", err)
	}

	// Извлекаем поля MetaAction
	var metaAction string
	var triggers []string
	var espero *Espero = &Espero{}

	if ma, ok := modelData["mact"].(string); ok {
		metaAction = ma
	}

	// Извлекаем и конвертируем поле triggers
	if t, ok := modelData["trig"]; ok {
		if trigArray, ok := t.([]interface{}); ok {
			for _, item := range trigArray {
				if str, ok := item.(string); ok {
					triggers = append(triggers, str)
				}
			}
		}
	}

	// Извлекаем поля espero
	if esp, ok := modelData["espero"].(map[string]interface{}); ok {
		if limit, ok := esp["limit"].(float64); ok {
			espero.Limit = uint16(limit)
		}
		if wait, ok := esp["wait"].(float64); ok {
			espero.Wait = uint8(wait)
		}
		if ignore, ok := esp["ignore"].(bool); ok {
			espero.Ignore = ignore
		}
	}

	return metaAction, triggers, espero, nil
}
