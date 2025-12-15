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
	// Возвращает: compressedData, vecIds, assistantId, error
	ReadUserModelByProvider(userId uint32, provider ProviderType) ([]byte, *VecIds, string, error)

	// GetUserVectorStorage получает ID векторного хранилища (deprecated: используйте ReadUserModelByProvider)
	GetUserVectorStorage(userId uint32) (string, error)
	// GetOrSetUserStorageLimit получает или устанавливает лимит хранилища
	GetOrSetUserStorageLimit(userID uint32, setStorage int64) (remaining uint64, totalLimit uint64, err error)

	// GetUserModels получает все модели пользователя из user_models
	GetUserModels(userId uint32) ([]UserModelRecord, error)
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
	UserId   uint32       `json:"user_id"`
	ModelId  uint64       `json:"model_id"`
	Provider ProviderType `json:"provider"`
	IsActive bool         `json:"is_active"`
}

// Ids представляет идентификатор файла с именем
type Ids struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// VecIds содержит ID файлов и векторных хранилищ
type VecIds struct {
	FileIds  []Ids    `json:"file_ids"`
	VectorId []string `json:"vector_id"`
}

type Models struct {
	ctx           context.Context
	client        *openai.Client
	mistralClient *MistralAgentClient // Клиент для работы с Mistral
	authKey       string
	db            DB
}

// New создаёт новый экземпляр Models для управления моделями
// openaiKey - API ключ OpenAI (может быть пустым, если OpenAI не используется)
// mistralKey - API ключ Mistral (может быть пустым, если Mistral не используется)
func New(ctx context.Context, db DB, openaiKey, mistralKey string) *Models {
	m := &Models{
		ctx:     ctx,
		db:      db,
		authKey: openaiKey, // Сохраняем для совместимости
	}

	// Инициализируем OpenAI клиент, если ключ предоставлен
	if openaiKey != "" {
		m.client = openai.NewClient(openaiKey)
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

// UniversalModelData представляет структуру данных модели для возврата клиенту
// Объединяет данные из старого формата (ModelDataRequest) с метаданными из БД
type UniversalModelData struct {
	Provider     ProviderType           `json:"provider"`     // Из user_models (1=OpenAI, 2=Mistral)
	AssistantId  string                 `json:"assistant_id"` // ID модели из API провайдера (из user_gpt.AssistantId)
	Name         string                 `json:"name"`         // Из ModelDataRequest.Name
	Model        string                 `json:"model"`        // Тип модели из GptType
	Instructions string                 `json:"instructions"` // Из ModelDataRequest.Prompt (переименован)
	MetaAction   string                 `json:"mact"`         // Из ModelDataRequest.MetaAction
	Triggers     []string               `json:"trig"`         // Из ModelDataRequest.Triggers
	FileIds      []Ids                  `json:"fileIds"`      // ID файлов из user_gpt.Ids
	VectorIds    []string               `json:"vectorIds"`    // ID векторов из user_gpt.Ids (опционально)
	S3Enabled    bool                   `json:"s3_enabled"`   // Флаг доступности S3 хранилища (remaining > 0)
	Operator     bool                   `json:"operator"`     // Из ModelDataRequest.Operator
	Search       bool                   `json:"search"`       // Из ModelDataRequest.Search
	Interpreter  bool                   `json:"interpreter"`  // Из ModelDataRequest.Interpreter
	S3           bool                   `json:"s3"`           // Из ModelDataRequest.S3
	RawData      map[string]interface{} `json:"-"`            // Сырые данные модели (не отправляется клиенту)

	// Алиасы для совместимости с методами записи (SaveModel, CreateModel)
	ModelID    string   `json:"model_id,omitempty"`    // Алиас для AssistantId
	ModelName  string   `json:"model_name,omitempty"`  // Алиас для Name
	ModelType  uint8    `json:"model_type,omitempty"`  // Числовой тип модели
	FileIDs    []Ids    `json:"file_ids,omitempty"`    // Алиас для FileIds
	VectorIDs  []string `json:"vector_ids,omitempty"`  // Алиас для VectorIds
	IsOperator bool     `json:"is_operator,omitempty"` // Алиас для Operator
}

// CreateModel создаёт новую модель (универсальный метод)
// Работает для любого провайдера (OpenAI, Mistral)
func (m *Models) CreateModel(userId uint32, provider ProviderType, gptName string, gptId uint8, modelName string, modelJSON []byte, fileIDs []Ids) (string, error) {
	switch provider {
	case ProviderOpenAI:
		return m.createOpenAIModel(userId, gptName, gptId, modelName, modelJSON, fileIDs)
	case ProviderMistral:
		return m.createMistralModel(userId, gptName, gptId, modelName, modelJSON)
	default:
		return "", fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// SaveModel сохраняет модель в БД в универсальном формате
// Работает для любого провайдера (OpenAI, Mistral)
// Автоматически устанавливает модель как активную если это первая модель пользователя
func (m *Models) SaveModel(userId uint32, data *UniversalModelData) error {
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

	// Создаем структуру для хранения ID файлов и векторов
	vecIds := VecIds{
		FileIds:  data.FileIDs,
		VectorId: data.VectorIDs,
	}
	idsJSON, err := json.Marshal(vecIds)
	if err != nil {
		return fmt.Errorf("ошибка сериализации ID файлов: %w", err)
	}

	// Сохраняем модель в БД (user_gpt + user_models в одной транзакции)
	// Метод автоматически создаст связь в user_models и установит IsActive для первой модели
	err = m.db.SaveUserModel(
		userId,
		data.ModelName,
		data.ModelID,
		compressed.Bytes(),
		data.ModelType,
		idsJSON,
		data.IsOperator,
		data.Provider, // Передаём провайдера
	)
	if err != nil {
		return fmt.Errorf("ошибка сохранения модели в БД: %w", err)
	}

	logger.Info("Модель успешно сохранена (провайдер: %s, ID: %s)", data.Provider, data.ModelID, userId)

	return nil
}

// ReadModel получает модель из БД в универсальном формате
// Если provider != nil - получает модель конкретного провайдера
// Если provider == nil - получает активную модель пользователя
// Работает для любого провайдера (OpenAI, Mistral)
func (m *Models) ReadModel(userId uint32, provider *ProviderType) (*UniversalModelData, error) {
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
	compressedData, vecIds, assistantId, err := m.db.ReadUserModelByProvider(userId, record.Provider)
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
	modelData.AssistantId = assistantId

	logger.Info("Модель успешно загружена (Provider: %s, ID: %s, IsActive: %v)",
		modelData.Provider, modelData.ModelID, record.IsActive, userId)

	return modelData, nil
}

// GetModelAsJSON получает модель из БД и возвращает её как JSON
// Предназначен для HTTP API endpoints - возвращает готовый JSON для отправки клиенту.
// Безопасно обрабатывает отсутствие модели (возвращает пустой объект {}).
// Если provider != nil - получает модель конкретного провайдера
// Если provider == nil - получает активную модель
//
// Использование в HTTP handler:
//
//	jsonData, err := client.GetModelAsJSON(userId, nil) // активная модель
//	if err != nil { return err }
//	w.Header().Set("Content-Type", "application/json")
//	w.Write(jsonData)
func (m *Models) GetModelAsJSON(userId uint32, provider *ProviderType) (json.RawMessage, error) {
	modelData, err := m.ReadModel(userId, provider)
	if err != nil {
		return nil, err
	}

	// Если модель не найдена, возвращаем пустой JSON объект
	if modelData == nil {
		return json.RawMessage(`{}`), nil
	}

	// Сериализуем в JSON
	result, err := json.Marshal(modelData)
	if err != nil {
		return nil, fmt.Errorf("ошибка сериализации модели в JSON: %w", err)
	}

	return result, nil
}

// DeleteModel удаляет модель из БД и удаляет связанные ресурсы,
// работает для любого провайдера (OpenAI, Mistral)
// Если удаляется активная модель и есть другие модели - автоматически переключает активную
// progressCallback - функция для отправки статуса через WebSocket (с эмодзи)
func (m *Models) DeleteModel(userId uint32, provider ProviderType, deleteFiles bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Получение информации о модели пользователя...")
	}

	// Получаем модель для определения деталей
	modelData, err := m.ReadModel(userId, &provider)
	if err != nil {
		return fmt.Errorf("ошибка получения модели: %w", err)
	}

	if modelData == nil {
		return fmt.Errorf("модель провайдера %s не найдена для пользователя %d", provider, userId)
	}

	// Получаем запись из user_models для проверки IsActive
	record, err := m.db.GetModelByProvider(userId, provider)
	if err != nil {
		return fmt.Errorf("ошибка получения записи модели: %w", err)
	}

	wasActive := record != nil && record.IsActive

	// В зависимости от провайдера удаляем модель
	switch modelData.Provider {
	case ProviderOpenAI:
		err = m.deleteOpenAIModel(userId, modelData, deleteFiles, progressCallback)
		if err != nil {
			return err
		}

	case ProviderMistral:
		err = m.deleteMistralModel(userId, modelData, deleteFiles, progressCallback)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("неизвестный провайдер: %s", modelData.Provider)
	}

	// Удаляем связь из user_models
	if progressCallback != nil {
		progressCallback("🔄 Удаление связи пользователь-модель...")
	}

	if record != nil {
		err = m.db.RemoveModelFromUser(userId, record.ModelId)
		if err != nil {
			return fmt.Errorf("ошибка удаления связи из user_models: %w", err)
		}
	}

	// Если удалённая модель была активной - переключаем на оставшуюся
	if wasActive {
		remainingModels, err := m.db.GetUserModels(userId)
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
		progressCallback(fmt.Sprintf("✅ Модель %s успешно удалена", modelData.Provider))
	}

	return nil
}

// UpdateModelToDB обновляет существующую модель (только БД, без обновления в API провайдера)
// Используйте UpdateModelEveryWhere для полного обновления
func (m *Models) UpdateModelToDB(userId uint32, data *UniversalModelData) error {
	// Проверяем существование модели
	provider := data.Provider
	existing, err := m.ReadModel(userId, &provider)
	if err != nil {
		return fmt.Errorf("ошибка проверки существующей модели: %w", err)
	}

	if existing == nil {
		return fmt.Errorf("модель провайдера %s не найдена для пользователя %d", provider, userId)
	}

	// Сохраняем обновленные данные
	return m.SaveModel(userId, data)
}

// UpdateModelEveryWhere полностью обновляет модель:
// - Обновляет модель в API провайдера (OpenAI Assistant или Mistral Agent)
// - Управляет файлами и векторными хранилищами
// - Сохраняет изменения в БД
func (m *Models) UpdateModelEveryWhere(userId uint32, data *UniversalModelData, modelJSON []byte) error {
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
func (m *Models) GetUserModels(userId uint32) ([]UniversalModelData, error) {
	records, err := m.db.GetUserModels(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	if len(records) == 0 {
		return []UniversalModelData{}, nil
	}

	models := make([]UniversalModelData, 0, len(records))
	for _, record := range records {
		// Читаем данные модели по провайдеру
		compressedData, vecIds, assistantId, err := m.db.ReadUserModelByProvider(userId, record.Provider)
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
		modelData.AssistantId = assistantId
		models = append(models, *modelData)
	}

	logger.Info("Загружено %d моделей", len(models), userId)
	return models, nil
}

// GetActiveUserModel получает активную модель пользователя
func (m *Models) GetActiveUserModel(userId uint32) (*UniversalModelData, error) {
	record, err := m.db.GetActiveModel(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения активной модели: %w", err)
	}

	if record == nil {
		logger.Debug("Активная модель не найдена", userId)
		return nil, nil
	}

	// Читаем данные модели по провайдеру
	compressedData, vecIds, assistantId, err := m.db.ReadUserModelByProvider(userId, record.Provider)
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
	modelData.AssistantId = assistantId

	logger.Info("Загружена активная модель (Provider: %s, ID: %s)",
		modelData.Provider, modelData.ModelID, userId)

	return modelData, nil
}

// GetUserModelByProvider получает модель пользователя по провайдеру
func (m *Models) GetUserModelByProvider(userId uint32, provider ProviderType) (*UniversalModelData, error) {
	record, err := m.db.GetModelByProvider(userId, provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения модели по провайдеру %s: %w", provider, err)
	}

	if record == nil {
		logger.Debug("Модель провайдера %s не найдена", provider, userId)
		return nil, nil
	}

	// Читаем данные модели по провайдеру
	compressedData, vecIds, assistantId, err := m.db.ReadUserModelByProvider(userId, record.Provider)
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
	modelData.AssistantId = assistantId

	logger.Info("Загружена модель провайдера %s (ID: %s)",
		provider, modelData.ModelID, userId)

	return modelData, nil
}

// SetActiveModel переключает активную модель пользователя (в транзакции)
func (m *Models) SetActiveModel(userId uint32, modelId uint64) error {
	err := m.db.SetActiveModel(userId, modelId)
	if err != nil {
		return fmt.Errorf("ошибка переключения активной модели: %w", err)
	}

	logger.Info("Активная модель переключена на ModelId=%d", modelId, userId)
	return nil
}

// decompressModelData - распаковывает данные модели из БД и преобразует в UniversalModelData
// Данные в БД хранятся в формате ModelDataRequest (name, prompt, mact, trig, и т.д.)
func (m *Models) decompressModelData(compressedData []byte, vecIds *VecIds, userId uint32) (*UniversalModelData, error) {
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
	modelData := &UniversalModelData{
		RawData: rawData,
	}

	// Извлекаем поля из ModelDataRequest
	if name, ok := rawData["name"].(string); ok {
		modelData.Name = name
	}
	if prompt, ok := rawData["prompt"].(string); ok {
		modelData.Instructions = prompt // prompt → instructions
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

	// Извлекаем gpttype для определения model
	if gptType, ok := rawData["gpttype"].(map[string]interface{}); ok {
		if model, ok := gptType["name"].(string); ok {
			modelData.Model = model
		}
	}

	// AssistantId НЕ хранится в Data - он приходит из user_gpt.AssistantId
	// Будет установлен позже из БД

	// Добавляем fileIds и vectorIds из БД (поле Ids в user_gpt)
	if vecIds != nil {
		modelData.FileIds = vecIds.FileIds
		if len(vecIds.VectorId) > 0 {
			modelData.VectorIds = vecIds.VectorId
		}
	}

	// Получаем информацию о хранилище и устанавливаем s3_enabled
	remaining, _, err := m.db.GetOrSetUserStorageLimit(userId, 0)
	if err == nil && remaining > 0 {
		modelData.S3Enabled = true
	}

	return modelData, nil
}
