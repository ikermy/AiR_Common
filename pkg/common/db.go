package common

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
)

type CreatorType uint8

const (
	User CreatorType = 1
	AI   CreatorType = 2
)

// DecompressAndExtractMetadata Функция для распаковки сжатых данных и извлечения полей Meta и MetaAction
func DecompressAndExtractMetadata(compressedData []byte) (string, []string, error) {
	// Создаем reader для распаковки данных
	gzipReader, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return "", nil, fmt.Errorf("ошибка при создании gzip reader: %w", err)
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
		return "", nil, fmt.Errorf("ошибка при распаковке данных: %w", err)
	}

	// Разбираем JSON
	var modelData map[string]interface{}
	if err := json.Unmarshal(decompressedData, &modelData); err != nil {
		return "", nil, fmt.Errorf("ошибка при разборе JSON модели: %w", err)
	}

	// Извлекаем поля MetaAction
	var metaAction string
	var triggers []string

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

	return metaAction, triggers, nil
}
