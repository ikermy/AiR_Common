package create

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"

	"github.com/ikermy/air_common/pkg/model/commdom"
)

func compressModelData(data *commdom.UniversalModelData) ([]byte, error) {
	modelJSON, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("ошибка сериализации данных модели: %w", err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(modelJSON); err != nil {
		return nil, fmt.Errorf("ошибка сжатия данных модели: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("ошибка закрытия gzip writer: %w", err)
	}
	return compressed.Bytes(), nil
}

func decompressModelData(compressedData []byte, vecIds *commdom.VecIds) (*commdom.UniversalModelData, error) {
	reader, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return nil, fmt.Errorf("ошибка распаковки данных модели: %w", err)
	}
	decompressed, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения распакованных данных: %w", err)
	}
	modelData := &commdom.UniversalModelData{}
	if err := json.Unmarshal(decompressed, modelData); err != nil {
		return nil, fmt.Errorf("ошибка десериализации данных модели: %w", err)
	}
	if vecIds != nil {
		if len(vecIds.FileIds) > 0 {
			modelData.FileIds = vecIds.FileIds
		}
		if len(vecIds.VectorId) > 0 {
			modelData.VecIds.VectorId = vecIds.VectorId
		}
	}
	if modelData.RealtimeVAD != nil {
		modelData.RealtimeVAD = applyRealtimeVADDefaults(modelData.RealtimeVAD)
	}
	return modelData, nil
}

// DecompressModelData распаковывает и десериализует данные модели из БД.
// UniversalModelData имеет те же JSON-теги что и формат хранения, поэтому
// используется прямой json.Unmarshal вместо ручного поля-за-полем парсинга.
//
// После десериализации:
//   - vecIds (FileIds и VectorId) переносятся из отдельного поля БД
//   - RealtimeVAD получает дефолтные значения для nil-полей
func (m *UniversalModel) DecompressModelData(compressedData []byte, vecIds *commdom.VecIds) (*commdom.UniversalModelData, error) {
	return decompressModelData(compressedData, vecIds)
}
