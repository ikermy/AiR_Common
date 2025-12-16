package models

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/sashabaranov/go-openai"
)

// ProviderType определяет тип провайдера модели (используется в БД)
type ProviderType uint8

const (
	// ProviderOpenAI представляет провайдера OpenAI
	ProviderOpenAI ProviderType = 1
	// ProviderMistral представляет провайдера Mistral
	ProviderMistral ProviderType = 2
)

// String возвращает строковое представление типа провайдера
func (p ProviderType) String() string {
	switch p {
	case ProviderOpenAI:
		return "openai"
	case ProviderMistral:
		return "mistral"
	default:
		return "unknown"
	}
}

// FromUint8 преобразует uint8 в ProviderType
func (p ProviderType) FromUint8(value uint8) ProviderType {
	return ProviderType(value)
}

// IsValid проверяет, является ли тип провайдера валидным
func (p ProviderType) IsValid() bool {
	return p == ProviderOpenAI || p == ProviderMistral
}

type DB interface {
	// SaveUserModel сохраняет модель в user_gpt и создает связь в user_models (всё в одной транзакции)
	// Автоматически определяет IsActive (первая модель пользователя становится активной)
	// provider - тип провайдера (1=OpenAI, 2=Mistral)
	SaveUserModel(userId uint32, name, assistantId string, data []byte, model uint8, ids json.RawMessage, operator bool, provider ProviderType) error

	// ReadUserModelByProvider получает сжатые данные модели по провайдеру
	// Возвращает: compressedData, vecIds, error
	ReadUserModelByProvider(userId uint32, provider ProviderType) ([]byte, *VecIds, error)

	// GetUserVectorStorage получает ID векторного хранилища (deprecated: используйте ReadUserModelByProvider)
	GetUserVectorStorage(userId uint32) (string, error)
	// GetOrSetUserStorageLimit получает или устанавливает лимит хранилища
	GetOrSetUserStorageLimit(userID uint32, setStorage int64) (remaining uint64, totalLimit uint64, err error)

	// GetUserModels получает все модели пользователя из user_models
	GetAllUserModels(userId uint32) ([]UserModelRecord, error)
	// GetActiveModel получает активную модель пользователя
	GetActiveModel(userId uint32) (*UserModelRecord, error)
	// GetModelByProvider получает модель пользователя по провайдеру
	GetModelByProvider(userId uint32, provider ProviderType) (*UserModelRecord, error)

	// SetActiveModel переключает активную модель (в транзакции)
	SetActiveModel(userId uint32, modelId uint64) error
	// RemoveModelFromUser удаляет связь модель-пользователь
	RemoveModelFromUser(userId uint32, modelId uint64) error
}

// UserModelRecord представляет запись из таблицы user_models
type UserModelRecord struct {
	FileIds  []Ids        `json:"file_ids"`
	AssistId string       `json:"assist_id"`
	ModelId  uint64       `json:"model_id"`
	Provider ProviderType `json:"provider"`
	IsActive bool         `json:"is_active"`
}

// Ids представляет идентификатор файла в OpenAI с его именем
type Ids struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// VecIds содержит ID файлов и векторных хранилищ
type VecIds struct {
	FileIds  []Ids    `json:"FileIds"`  // Совпадает с форматом в БД
	VectorId []string `json:"VectorId"` // Совпадает с форматом в БД
}

// Universal Model Create Request данные после суспешного создания модели
type UMCR struct {
	AssistID string       `json:"assist_id"`
	AllIds   []byte       `json:"all_ids"`
	Provider ProviderType `json:"provider"`
}

type UniversalModel struct {
	ctx           context.Context
	openaiClient  *openai.Client
	mistralClient *MistralAgentClient // Клиент для работы с Mistral
	authKey       string
	db            DB
}

// New создаёт новый экземпляр UniversalModel для управления моделями
// openaiKey - API ключ OpenAI (может быть пустым, если OpenAI не используется)
// mistralKey - API ключ Mistral (может быть пустым, если Mistral не используется)
func New(ctx context.Context, db DB, openaiKey, mistralKey string) *UniversalModel {
	m := &UniversalModel{
		ctx:     ctx,
		db:      db,
		authKey: openaiKey, // Сохраняем для совместимости
	}

	// Инициализируем OpenAI клиент, если ключ предоставлен
	if openaiKey != "" {
		m.openaiClient = openai.NewClient(openaiKey)
	}

	// Инициализируем Mistral клиент, если ключ предоставлен
	if mistralKey != "" {
		m.mistralClient = &MistralAgentClient{
			apiKey: mistralKey,
			url:    mode.MistralAgentsURL,
			ctx:    ctx,
		}
	}

	return m
}

type GptType struct {
	Name string `json:"name"`
	ID   uint8  `json:"id"`
}

// UniversalModelData универсальная структура хранения данных моделей
type UniversalModelData struct {
	Name        string        `json:"name"`        // Из ModelDataRequest.Name
	Prompt      string        `json:"prompt"`      // Алиас для Instructions (обратная совместимость)
	MetaAction  string        `json:"mact"`        // Из ModelDataRequest.MetaAction
	Triggers    []string      `json:"trig"`        // Из ModelDataRequest.Triggers
	FileIds     []Ids         `json:"fileIds"`     // ID файлов из user_gpt.Ids
	VecIds      VecIds        `json:"vecIds"`      // ID векторных хранилищ из user_gpt.Ids
	Operator    bool          `json:"operator"`    // Из ModelDataRequest.Operator
	Search      bool          `json:"search"`      // Из ModelDataRequest.Search
	Interpreter bool          `json:"interpreter"` // Из ModelDataRequest.Interpreter
	S3          bool          `json:"s3"`          // Из ModelDataRequest.S3
	Espero      *EsperoConfig `json:"espero"`      // Настройки ожидания из ModelDataRequest.Espero
	GptType     *GptType      `json:"gpttype"`
	Provider    ProviderType  `json:"provider"` // "openai=1" или "mistral=2"
}

// EsperoConfig представляет настройки ожидания из ModelDataRequest
type EsperoConfig struct {
	Limit  uint16 `json:"limit"`  // Лимит символов
	Wait   uint8  `json:"wait"`   // Время ожидания
	Ignore bool   `json:"ignore"` // Игнорировать ожидание
}

// UserModelsResponse представляет ответ со всеми моделями пользователя
type UserModelsResponse struct {
	Models         map[string]*UniversalModelData `json:"models"`          // Модели по провайдерам ("openai", "mistral")
	ActiveProvider string                         `json:"active_provider"` // Активный провайдер
}

// CreateModel создаёт новую модель (универсальный метод)
// Работает для любого провайдера (OpenAI, Mistral)
func (m *UniversalModel) CreateModel(
	userId uint32, provider ProviderType, gptName string, modelName string, modelJSON []byte, fileIDs []Ids) (UMCR, error) {

	switch provider {
	case ProviderOpenAI:
		return m.createOpenAIModel(userId, gptName, modelName, modelJSON, fileIDs)
	case ProviderMistral:
		return m.createMistralModel(userId, gptName, modelName, modelJSON)
	default:
		return UMCR{}, fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// SaveModel сохраняет модель в БД в универсальном формате
// Работает для любого провайдера (OpenAI, Mistral)
// Автоматически устанавливает модель как активную если это первая модель пользователя
func (m *UniversalModel) SaveModel(userId uint32, umcr UMCR, data *UniversalModelData) error {
	// Сериализуем данные модели в JSON
	modelJSON, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("ошибка сериализации данных модели: %w", err)
	}

	// Сжимаем данные с помощью gzip для экономии места
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(modelJSON); err != nil {
		return fmt.Errorf("ошибка сжатия данных модели: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("ошибка закрытия gzip writer: %w", err)
	}

	err = m.db.SaveUserModel(
		userId,
		data.Name,
		umcr.AssistID,
		compressed.Bytes(),
		data.GptType.ID,
		umcr.AllIds,
		data.Operator,
		umcr.Provider, // Передаём провайдера
	)
	if err != nil {
		return fmt.Errorf("ошибка сохранения модели в БД: %w", err)
	}

	logger.Info("Модель успешно сохранена (провайдер: %s, ID: %d)", umcr.Provider, data.GptType.ID, userId)

	return nil
}

// ReadModel получает модель из БД в универсальном формате
// Если provider != nil - получает модель конкретного провайдера
// Если provider == nil - получает активную модель пользователя
// Работает для любого провайдера (OpenAI, Mistral)
func (m *UniversalModel) ReadModel(userId uint32, provider *ProviderType) (*UniversalModelData, error) {
	var record *UserModelRecord
	var err error

	// Если провайдер не указан - получаем активную модель
	if provider == nil {
		record, err = m.db.GetActiveModel(userId)
		if err != nil {
			return nil, fmt.Errorf("ошибка получения активной модели: %w", err)
		}
		if record == nil {
			logger.Debug("Активная модель не найдена", userId)
			return nil, nil
		}
		logger.Debug("Получение активной модели (Provider: %s)", record.Provider, userId)
	} else {
		// Получаем модель конкретного провайдера
		record, err = m.db.GetModelByProvider(userId, *provider)
		if err != nil {
			return nil, fmt.Errorf("ошибка получения модели провайдера %s: %w", *provider, err)
		}
		if record == nil {
			logger.Debug("Модель провайдера %s не найдена", *provider, userId)
			return nil, nil
		}
		logger.Debug("Получение модели провайдера %s", *provider, userId)
	}

	// Получаем данные из БД по провайдеру
	compressedData, vecIds, err := m.db.ReadUserModelByProvider(userId, record.Provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения модели из БД: %w", err)
	}

	if compressedData == nil {
		return nil, nil
	}

	// Используем вспомогательный метод для распаковки
	modelData, err := m.decompressModelData(compressedData, vecIds, userId)
	if err != nil {
		return nil, err
	}

	// Устанавливаем провайдера и AssistantId из БД
	modelData.Provider = record.Provider

	logger.Info("Модель успешно загружена (Provider: %s, Name: %s, IsActive: %v)",
		modelData.Provider, modelData.Name, record.IsActive, userId)

	return modelData, nil
}

// GetModelAsJSON получает ВСЕ модели пользователя и возвращает их как JSON
// Предназначен для HTTP API endpoints - возвращает готовый JSON для отправки клиенту.
// Возвращает объект с моделями по провайдерам и информацией об активной модели:
//
//	{
//	  "models": {
//	    "openai": { "name": "...", "fileIds": [...], ... },
//	    "mistral": { "name": "...", ... }
//	  },
//	  "active_provider": "openai"
//	}
//
// Если у пользователя нет моделей - возвращает пустой объект {}.
// Параметр provider игнорируется (оставлен для обратной совместимости).
//
// Использование в HTTP handler:
//
//	jsonData, err := openaiClient.GetAllModelAsJSON(userId, nil)
//	if err != nil { return err }
//	w.Header().Set("Content-Type", "application/json")
//	w.Write(jsonData)
func (m *UniversalModel) GetModelAsJSON(userId uint32) (json.RawMessage, error) {
	// Получаем все модели пользователя
	response, err := m.GetAllUserModelsResponse(userId)
	if err != nil {
		return nil, err
	}

	// Если нет моделей, возвращаем пустой JSON объект
	if len(response.Models) == 0 {
		return json.RawMessage(`{}`), nil
	}

	// Сериализуем в JSON
	result, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("ошибка сериализации моделей в JSON: %w", err)
	}

	return result, nil
}

// DeleteModel удаляет модель из БД и удаляет связанные ресурсы,
// работает для любого провайдера (OpenAI, Mistral)
// Если удаляется активная модель и есть другие модели - автоматически переключает активную
// progressCallback - функция для отправки статуса через WebSocket (с эмодзи)
func (m *UniversalModel) DeleteModel(userId uint32, provider ProviderType, deleteFiles bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Получение информации о модели пользователя...")
	}

	// Получаем запись из user_models для проверки IsActive
	modelRecord, err := m.db.GetModelByProvider(userId, provider)
	if err != nil || modelRecord == nil {
		return fmt.Errorf("ошибка получения записи модели: %w", err)
	}

	// В зависимости от провайдера удаляем модель
	switch modelRecord.Provider {
	case ProviderOpenAI:
		err = m.deleteOpenAIModel(userId, modelRecord, deleteFiles, progressCallback)
		if err != nil {
			return err
		}

	case ProviderMistral:
		err = m.deleteMistralModel(userId, modelRecord, deleteFiles, progressCallback)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("неизвестный провайдер: %s", modelRecord.Provider)
	}

	// Удаляем связь из user_models
	if progressCallback != nil {
		progressCallback("🔄 Удаление связи пользователь-модель...")
	}

	err = m.db.RemoveModelFromUser(userId, modelRecord.ModelId)
	if err != nil {
		return fmt.Errorf("ошибка удаления связи из user_models: %w", err)
	}

	// Если удалённая модель была активной - переключаем на оставшуюся
	if modelRecord.IsActive {
		remainingModels, err := m.db.GetAllUserModels(userId)
		if err != nil {
			logger.Warn("Ошибка получения оставшихся моделей: %v", err, userId)
		} else if len(remainingModels) > 0 {
			// Переключаем на первую оставшуюся модель
			newActiveModelId := remainingModels[0].ModelId
			err = m.db.SetActiveModel(userId, newActiveModelId)
			if err != nil {
				logger.Error("Ошибка автоматического переключения активной модели: %v", err, userId)
			} else {
				logger.Info("Активная модель автоматически переключена на ModelId=%d после удаления",
					newActiveModelId, userId)
				if progressCallback != nil {
					progressCallback(fmt.Sprintf("✅ Активная модель переключена на оставшуюся (ID: %d)", newActiveModelId))
				}
			}
		}
	}

	if progressCallback != nil {
		progressCallback(fmt.Sprintf("✅ Модель %s успешно удалена", modelRecord.Provider))
	}

	return nil
}

// UpdateModelToDB обновляет существующую модель (только БД, без обновления в API провайдера)
// Используйте UpdateModelEveryWhere для полного обновления
func (m *UniversalModel) UpdateModelToDB(userId uint32, data *UniversalModelData) error {
	// Проверяем существование модели
	provider := data.Provider
	existing, err := m.ReadModel(userId, &provider)
	if err != nil {
		return fmt.Errorf("ошибка проверки существующей модели: %w", err)
	}

	if existing == nil {
		return fmt.Errorf("модель провайдера %s не найдена для пользователя %d", provider, userId)
	}

	existingModelData, err := m.db.GetModelByProvider(userId, provider)
	if err != nil || existingModelData == nil {
		return fmt.Errorf("ошибка получения записи модели: %w", err)
	}

	// Сериализуем vecIds в JSON
	vecIdsJSON, err := json.Marshal(data.VecIds)
	if err != nil {
		return fmt.Errorf("failed to marshal vector IDs: %w", err)
	}

	// Сохраняем обновленные данные
	return m.SaveModel(userId, UMCR{
		AssistID: existingModelData.AssistId,
		AllIds:   vecIdsJSON,
		Provider: data.Provider,
	}, data)
}

// UpdateModelEveryWhere полностью обновляет модель:
// - Обновляет модель в API провайдера (OpenAI Assistant или Mistral Agent)
// - Управляет файлами и векторными хранилищами
// - Сохраняет изменения в БД
func (m *UniversalModel) UpdateModelEveryWhere(userId uint32, data *UniversalModelData, modelJSON []byte) error {
	// Получаем текущую модель
	provider := data.Provider
	existing, err := m.ReadModel(userId, &provider)
	if err != nil {
		return fmt.Errorf("ошибка получения текущей модели: %w", err)
	}

	if existing == nil {
		return fmt.Errorf("модель провайдера %s не найдена для пользователя %d", provider, userId)
	}

	// Проверяем, что провайдер не изменился
	if data.Provider != existing.Provider {
		return fmt.Errorf("нельзя изменить провайдера модели (было: %s, стало: %s)", existing.Provider, data.Provider)
	}

	// Обновляем в зависимости от провайдера
	switch data.Provider {
	case ProviderOpenAI:
		return m.updateOpenAIModelInPlace(userId, existing, data, modelJSON)

	case ProviderMistral:
		return m.updateMistralModelInPlace(userId, existing, data, modelJSON)

	default:
		return fmt.Errorf("неизвестный провайдер: %s", data.Provider)
	}
}

// ============================================================================
// Методы для работы с множественными моделями
// ============================================================================

// GetUserModels получает все модели пользователя
func (m *UniversalModel) GetUserModels(userId uint32) ([]UniversalModelData, error) {
	records, err := m.db.GetAllUserModels(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	if len(records) == 0 {
		return []UniversalModelData{}, nil
	}

	models := make([]UniversalModelData, 0, len(records))
	for _, record := range records {
		// Читаем данные модели по провайдеру
		compressedData, vecIds, err := m.db.ReadUserModelByProvider(userId, record.Provider)
		if err != nil {
			logger.Warn("Пропуск модели %d (Provider: %s): ошибка чтения данных: %v", record.ModelId, record.Provider, err, userId)
			continue
		}

		if compressedData == nil {
			logger.Warn("Пропуск модели %d (Provider: %s): данные отсутствуют", record.ModelId, record.Provider, userId)
			continue
		}

		// Распаковка данных
		modelData, err := m.decompressModelData(compressedData, vecIds, userId)
		if err != nil {
			logger.Warn("Пропуск модели %d (Provider: %s): ошибка распаковки: %v", record.ModelId, record.Provider, err, userId)
			continue
		}

		// Обновляем провайдера и AssistantId из БД
		modelData.Provider = record.Provider
		models = append(models, *modelData)
	}

	logger.Info("Загружено %d моделей", len(models), userId)
	return models, nil
}

// GetAllUserModelsResponse получает все модели пользователя в формате для API
// Возвращает объект с моделями по провайдерам и информацией об активной модели
func (m *UniversalModel) GetAllUserModelsResponse(userId uint32) (*UserModelsResponse, error) {
	records, err := m.db.GetAllUserModels(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	response := &UserModelsResponse{
		Models: make(map[string]*UniversalModelData),
	}

	var activeProvider ProviderType

	for _, record := range records {
		// Читаем данные модели по провайдеру
		compressedData, vecIds, err := m.db.ReadUserModelByProvider(userId, record.Provider)
		if err != nil {
			logger.Warn("Пропуск модели %d (Provider: %s): ошибка чтения данных: %v",
				record.ModelId, record.Provider, err, userId)
			continue
		}

		if compressedData == nil {
			logger.Warn("Пропуск модели %d (Provider: %s): данные отсутствуют",
				record.ModelId, record.Provider, userId)
			continue
		}

		// Распаковка данных
		modelData, err := m.decompressModelData(compressedData, vecIds, userId)
		if err != nil {
			logger.Warn("Пропуск модели %d (Provider: %s): ошибка распаковки: %v",
				record.ModelId, record.Provider, err, userId)
			continue
		}

		// Устанавливаем провайдера из user_models
		modelData.Provider = record.Provider

		// Сохраняем активный провайдер
		if record.IsActive {
			activeProvider = record.Provider
		}

		// Добавляем модель в map по строковому ключу провайдера
		response.Models[record.Provider.String()] = modelData
	}

	// Устанавливаем активный провайдер
	if activeProvider != 0 {
		response.ActiveProvider = activeProvider.String()
	}

	logger.Info("Загружено %d моделей (активный: %s)", len(response.Models), response.ActiveProvider, userId)
	return response, nil
}

// GetActiveUserModel получает активную модель пользователя
func (m *UniversalModel) GetActiveUserModel(userId uint32) (*UniversalModelData, error) {
	record, err := m.db.GetActiveModel(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения активной модели: %w", err)
	}

	if record == nil {
		logger.Debug("Активная модель не найдена", userId)
		return nil, nil
	}

	// Читаем данные модели по провайдеру
	compressedData, vecIds, err := m.db.ReadUserModelByProvider(userId, record.Provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения данных активной модели: %w", err)
	}

	if compressedData == nil {
		return nil, nil
	}

	modelData, err := m.decompressModelData(compressedData, vecIds, userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка распаковки активной модели: %w", err)
	}

	// Устанавливаем провайдера и AssistantId из БД
	modelData.Provider = record.Provider

	logger.Info("Загружена активная модель (Provider: %s, Name: %s)",
		modelData.Provider, modelData.Name, userId)

	return modelData, nil
}

// GetUserModelByProvider получает модель пользователя по провайдеру
func (m *UniversalModel) GetUserModelByProvider(userId uint32, provider ProviderType) (*UniversalModelData, error) {
	record, err := m.db.GetModelByProvider(userId, provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения модели по провайдеру %s: %w", provider, err)
	}

	if record == nil {
		logger.Debug("Модель провайдера %s не найдена", provider, userId)
		return nil, nil
	}

	// Читаем данные модели по провайдеру
	compressedData, vecIds, err := m.db.ReadUserModelByProvider(userId, record.Provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения данных модели: %w", err)
	}

	if compressedData == nil {
		return nil, nil
	}

	modelData, err := m.decompressModelData(compressedData, vecIds, userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка распаковки модели: %w", err)
	}

	// Устанавливаем провайдера и AssistantId из БД
	modelData.Provider = record.Provider

	logger.Info("Загружена модель провайдера %s (ID: %d)",
		provider, modelData.Provider, userId)

	return modelData, nil
}

// SetActiveModel переключает активную модель пользователя (в транзакции)
func (m *UniversalModel) SetActiveModel(userId uint32, modelId uint64) error {
	err := m.db.SetActiveModel(userId, modelId)
	if err != nil {
		return fmt.Errorf("ошибка переключения активной модели: %w", err)
	}

	logger.Info("Активная модель переключена на ModelId=%d", modelId, userId)
	return nil
}

// decompressModelData - распаковывает данные модели из БД и преобразует в UniversalModelData
// Данные в БД хранятся в формате ModelDataRequest (name, prompt, mact, trig, и т.д.)
func (m *UniversalModel) decompressModelData(compressedData []byte, vecIds *VecIds, userId uint32) (*UniversalModelData, error) {
	// Распаковываем gzip
	reader, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return nil, fmt.Errorf("ошибка распаковки данных модели: %w", err)
	}
	defer reader.Close()

	decompressed, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения распакованных данных: %w", err)
	}

	// Парсим формат ModelDataRequest в map
	var rawData map[string]interface{}
	if err := json.Unmarshal(decompressed, &rawData); err != nil {
		return nil, fmt.Errorf("ошибка десериализации данных модели: %w", err)
	}

	// Создаём UniversalModelData из формата ModelDataRequest
	modelData := &UniversalModelData{}

	// Извлекаем поля из ModelDataRequest
	if name, ok := rawData["name"].(string); ok {
		modelData.Name = name
	}
	if prompt, ok := rawData["prompt"].(string); ok {
		modelData.Prompt = prompt
	}
	if mact, ok := rawData["mact"].(string); ok {
		modelData.MetaAction = mact
	}
	if operator, ok := rawData["operator"].(bool); ok {
		modelData.Operator = operator
	}
	if search, ok := rawData["search"].(bool); ok {
		modelData.Search = search
	}
	if interpreter, ok := rawData["interpreter"].(bool); ok {
		modelData.Interpreter = interpreter
	}
	if s3, ok := rawData["s3"].(bool); ok {
		modelData.S3 = s3
	}

	// Извлекаем espero
	if esperoMap, ok := rawData["espero"].(map[string]interface{}); ok {
		espero := &EsperoConfig{}
		if limit, ok := esperoMap["limit"].(float64); ok {
			espero.Limit = uint16(limit)
		}
		if wait, ok := esperoMap["wait"].(float64); ok {
			espero.Wait = uint8(wait)
		}
		if ignore, ok := esperoMap["ignore"].(bool); ok {
			espero.Ignore = ignore
		}
		modelData.Espero = espero
	}

	// Извлекаем triggers (массив строк)
	if trig, ok := rawData["trig"].([]interface{}); ok {
		triggers := make([]string, 0, len(trig))
		for _, t := range trig {
			if str, ok := t.(string); ok {
				triggers = append(triggers, str)
			}
		}
		modelData.Triggers = triggers
	}

	//// Извлекаем gpttype для определения model
	//if gptType, ok := rawData["gpttype"].(map[string]interface{}); ok {
	//	if model, ok := gptType["name"].(string); ok {
	//		modelData.UniversalModel = model
	//	}
	//}

	// AssistantId НЕ хранится в Data - он приходит из user_gpt.AssistantId
	// Будет установлен позже из БД

	// Добавляем fileIds и vectorIds ТОЛЬКО из БД (поле Ids в user_gpt)
	// Они НЕ хранятся в Data, только в отдельном поле Ids
	if vecIds != nil {
		if len(vecIds.FileIds) > 0 {
			modelData.FileIds = vecIds.FileIds
		}
		//if len(vecIds.VectorId) > 0 {
		//	modelData.VectorIds = vecIds.VectorId
		//}
	}

	// Получаем информацию о хранилище и устанавливаем s3_enabled
	remaining, _, err := m.db.GetOrSetUserStorageLimit(userId, 0)
	if err == nil && remaining > 0 {
		modelData.S3 = true
	}

	return modelData, nil
}
