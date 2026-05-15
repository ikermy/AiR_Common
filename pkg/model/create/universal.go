package create

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/mode"
)

//var OpenAIExtandingCacheModels = []string{
//	"gpt-5.2", "gpt-5.1-codex-max", "gpt-5.1", "gpt-5.1-codex",
//	"gpt-5.1-codex-mini", "gpt-5.1-chat-latest", "gpt-5", "gpt-5-codex", "gpt-4.1",
//}

var OpenAIExtandingCacheModels = []string{
	"gpt-5.5-instant",
	"gpt-5.5-pro",
	"gpt-5.4-pro",
	"gpt-5.4-mini",
	"gpt-5.3-codex",
	"gpt-5-mini",
	"gpt-4.1",
	"gpt-4.1-mini",
}

const (
	// RealtimeDefaultModel фиксированная realtime-модель OpenAI
	RealtimeDefaultModel = "gpt-realtime-mini"
	//RealtimeDefaultModel = "gpt-realtime"

	// RealtimeBaseURL базовый WebSocket URL для OpenAI Realtime API
	RealtimeBaseURL = "wss://api.openai.com/v1/realtime"

	// Параметры сессии Realtime API
	RealtimeTemperature  = 0.7
	RealtimeMaxOutTokens = 500

	GoogleVideoModel = "veo-3.1-fast-generate-preview"
	GoogleAudioModel = "gemini-2.5-flash-lite"

	DialogHistoryLimit     = uint8(20)         // Максимальное количество сообщений в истории диалога для Google Gemini
	DialogLiveTimeout      = 180 * time.Second // Тайм-аут времени жизни диалога + секунд до сброса локальной истории сообщений
	TxChanBuffer           = 100               // Буфер канала ответов ассистента критично для режима Streaming
	RxChanBuffer           = 10                // Буфер канала сообщений от пользователя критично для режима когда отключенное игнорирование вопросов пользователя
	MaxFunctionCalls       = 10                // Лимит для предотвращения бесконечных циклов
	SimilarEmbeddingsLimit = 3                 // Макс. количество похожих эмбеддингов для возврата при поиске в БД (можно увеличить при необходимости, но влияет на производительность
	ApplayRAGTimeaut       = 15 * time.Second  // Тайм-аут для применения RAG (поиск в документах) к ответу модели, чтобы не задерживать ответ слишком долго
)

// ============================================================================
// PROVIDER TYPE
// ============================================================================

// ProviderType определяет тип провайдера модели (используется в БД)
type ProviderType uint8

const (
	ProviderOpenAI  ProviderType = 1
	ProviderMistral ProviderType = 2
	ProviderGoogle  ProviderType = 3
)

// String возвращает строковое представление типа провайдера
func (p ProviderType) String() string {
	switch p {
	case ProviderOpenAI:
		return "openai"
	case ProviderMistral:
		return "mistral"
	case ProviderGoogle:
		return "google"
	default:
		return "unknown"
	}
}

// FromString преобразует строку в ProviderType
func FromString(s string) (ProviderType, error) {
	switch s {
	case "openai":
		return ProviderOpenAI, nil
	case "mistral":
		return ProviderMistral, nil
	case "google":
		return ProviderGoogle, nil
	default:
		return 0, fmt.Errorf("неизвестный провайдер: %s", s)
	}
}

// FromUint8 преобразует uint8 в ProviderType
func (p ProviderType) FromUint8(value uint8) ProviderType {
	return ProviderType(value)
}

// IsValid проверяет, является ли тип провайдера валидным
func (p ProviderType) IsValid() bool {
	return p == ProviderOpenAI || p == ProviderMistral || p == ProviderGoogle
}

type DB interface {
	// SaveUserModel сохраняет модель в user_gpt и создает связь в user_models (всё в одной транзакции)
	// Автоматически определяет IsActive (первая модель пользователя становится активной)
	// provider - тип провайдера (1=OpenAI, 2=Mistral)
	SaveUserModel(userId uint32, provider ProviderType, name, assistantId string, data []byte, model uint8, ids json.RawMessage, operator bool) error

	// ReadUserModelByProvider получает сжатые данные модели по провайдеру
	// Возвращает: compressedData, vecIds, error
	ReadUserModelByProvider(userId uint32, provider ProviderType) ([]byte, *VecIds, error)

	// GetUserVectorStorage получает ID векторного хранилища (deprecated: используйте ReadUserModelByProvider)
	GetUserVectorStorage(userId uint32) (string, error)

	// GetOrSetUserStorageLimit получает или устанавливает лимит хранилища
	GetOrSetUserStorageLimit(userID uint32, setStorage int64) (remaining uint64, totalLimit uint64, err error)

	// GetAllUserModels GetUserModels получает все модели пользователя из user_models
	GetAllUserModels(userId uint32) ([]UserModelRecord, error)

	// GetActiveModel получает активную модель пользователя
	GetActiveModel(userId uint32) (*UserModelRecord, error)

	// GetModelByProvider получает АКТИВНУЮ модель пользователя по провайдеру
	GetModelByProvider(userId uint32, provider ProviderType) (*UserModelRecord, error)

	// GetModelByProviderAnyStatus получает модель пользователя по провайдеру независимо от статуса активности
	GetModelByProviderAnyStatus(userId uint32, provider ProviderType) (*UserModelRecord, error)

	// SetActiveModel переключает активную модель (в транзакции)
	SetActiveModel(userId uint32, modelId uint64) error

	// SetActiveModelByProvider устанавливает активную модель по провайдеру
	SetActiveModelByProvider(userId uint32, provider ProviderType) error

	// RemoveModelFromUser удаляет связь модель-пользователь
	RemoveModelFromUser(userId uint32, modelId uint64) error

	// ============================================================================
	// VECTOR EMBEDDINGS - Методы работы с векторными эмбеддингами в MariaDB
	// ВАЖНО: model_id ссылается на user_create.ModelId для привязки эмбеддингов к модели
	// ============================================================================

	// SaveEmbedding сохраняет векторный эмбеддинг документа в БД с привязкой к модели
	SaveEmbedding(userId uint32, modelId uint64, provider ProviderType, docID, docName, content string, embedding []float32, metadata DocumentMetadata) error

	// ListModelEmbeddings возвращает список всех документов конкретной модели и провайдера с эмбеддингами
	ListModelEmbeddings(modelId uint64, provider ProviderType) ([]VectorDocument, error)

	// DeleteEmbedding удаляет эмбеддинг документа по ID модели и docID
	DeleteEmbedding(modelId uint64, docID string) error

	// DeleteAllModelEmbeddings удаляет все эмбеддинги конкретной модели
	DeleteAllModelEmbeddings(modelId uint64) error

	// SearchSimilarEmbeddings ищет похожие документы в рамках конкретной модели и провайдера используя VEC_Distance_Cosine
	SearchSimilarEmbeddings(modelId uint64, provider ProviderType, queryEmbedding []float32, limit int) ([]VectorDocument, error)

	// GetUserTimeZone получает часовой пояс пользователя из БД
	UserTimeZone(userId uint32) (string, error)
}

// DocumentMetadata представляет метаданные документа с эмбеддингом
type DocumentMetadata struct {
	Source    string `json:"source,omitempty"`     // Источник документа (например, "file_upload", "manual")
	FileName  string `json:"file_name,omitempty"`  // Имя файла (если загружен из файла)
	FileID    string `json:"file_id,omitempty"`    // ID файла в системе провайдера (Google, OpenAI и т.д.)
	CreatedAt string `json:"created_at,omitempty"` // Время создания в формате RFC3339
	Tags      string `json:"tags,omitempty"`       // Теги для категоризации документа
	Category  string `json:"category,omitempty"`   // Категория документа
	Custom    string `json:"custom,omitempty"`     // Любые дополнительные пользовательские данные в формате JSON
}

// VectorDocument представляет документ с эмбеддингом из БД
type VectorDocument struct {
	ID        string           `json:"id"`
	UserID    uint32           `json:"user_id"`
	Name      string           `json:"name"`
	Content   string           `json:"content"`
	Embedding []float32        `json:"embedding"`
	Metadata  DocumentMetadata `json:"metadata,omitempty"`
	CreatedAt interface{}      `json:"created_at"` // time.Time в БД, но может быть string в JSON
}

// UserModelRecord представляет запись из таблицы user_models
type UserModelRecord struct {
	FileIds  []Ids        `json:"file_ids"`
	AssistId string       `json:"assist_id"`
	ModelId  uint64       `json:"model_id"`
	Provider ProviderType `json:"provider"`
	IsActive bool         `json:"is_active"`
	AllIds   []byte       `json:"all_ids"` // Raw JSON с FileIds и VectorId из БД
}

// Ids представляет идентификатор файла в OpenAI с его именем
type Ids struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// VecIds содержит ID файлов и векторных хранишь
type VecIds struct {
	FileIds  []Ids    `json:"FileIds"`  // Совпадает с форматом в БД
	VectorId []string `json:"VectorId"` // Совпадает с форматом в БД
}

// UMCR Universal Model Create Request данные после успешного создания модели
type UMCR struct {
	AssistID string       `json:"assist_id"`
	AllIds   []byte       `json:"all_ids"`
	Provider ProviderType `json:"provider"`
}

type UniversalModel struct {
	ctx           context.Context
	openaiClient  *OpenAIAgentClient  // Клиент для работы с OpenAI
	mistralClient *MistralAgentClient // Клиент для работы с Mistral
	googleClient  *GoogleAgentClient  // Клиент для работы с Google
	landingPort   string
	db            DB
}

// New создаёт новый экземпляр UniversalModel для управления моделями
// любой ключь может быть пустым (если не используется соответствующий провайдер)
func New(ctx context.Context, db DB, conf *conf.Conf) *UniversalModel {
	m := &UniversalModel{
		ctx:         ctx,
		db:          db,
		landingPort: conf.WEB.Land, // TODO нужно изменить правила Nginx и убрать порт
	}

	// Инициализируем OpenAI клиент, если ключ предоставлен
	m.openaiClient = &OpenAIAgentClient{
		apiKey: conf.GPT.OpenAIKey,
		url:    mode.OpenAIAgentsURL,
		ctx:    ctx,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		universalModel: m, // Передаем ссылку на universalModel для доступа к GetRealUserID
	}

	// Инициализируем Mistral клиент, если ключ предоставлен
	m.mistralClient = &MistralAgentClient{
		apiKey:         conf.GPT.MistralKey,
		url:            mode.MistralAgentsURL,
		ctx:            ctx,
		universalModel: m,
	}

	// Инициализируем google клиент, если ключ предоставлен
	m.googleClient = &GoogleAgentClient{
		apiKey:         conf.GPT.GoogleKey,
		url:            mode.GoogleAgentsURL,
		ctx:            ctx,
		universalModel: m,
	}

	return m
}

// SetMistralMCPFetchers устанавливает MCP-fetchers на mistralClient.
// Вызывается из mistral/model.go после инициализации UniversalModel.
func (m *UniversalModel) SetMistralMCPFetchers(promptFetcher GooglePromptHintFetcher, toolsFetcher GoogleFunctionDeclarationsFetcher) {
	if m.mistralClient != nil {
		m.mistralClient.SetMCPConfigFetchers(promptFetcher, toolsFetcher)
	}
}

type GptType struct {
	Name string `json:"name"`
	ID   uint8  `json:"id"`
}

// UniversalModelData универсальная структура хранения данных моделей
// Примечание: Calendar/Sheets (ранее GOAuth) теперь управляются исключительно MCP сервером —
// клиент не хранит эти флаги, инструменты и инструкции приходят через FetchToolsList/FetchSystemPrompt.
type UniversalModelData struct {
	Name        string       `json:"name"`                   // Имя модели только для удобства идентификации
	Prompt      string       `json:"prompt"`                 // Промпт модели
	MetaAction  string       `json:"mact"`                   // Заданная цель модели (уведомление о достижении целы) вызывается меткой в структуре ответа "target"
	Triggers    []string     `json:"trig"`                   // Триггеры модели
	FileIds     []Ids        `json:"fileIds"`                // ID файлов для загрузки в векторное хранилище?
	VecIds      VecIds       `json:"vecIds"`                 // ID файлов в векторном хранилище
	Operator    bool         `json:"operator"`               // Вызов ответом от модели "operator" флаг переключения на оператора
	Search      bool         `json:"search"`                 // Поиск по векторному хранилищу, если загружены файлы для дообучения модели
	Interpreter bool         `json:"interpreter"`            // Генерация кода (Code Interpreter) для OpenAI — нативный инструмент провайдера
	S3          bool         `json:"s3"`                     // Работа моделей с файлами в S3-хранилище
	Haunter     bool         `json:"haunter"`                // Модель будет использоваться для поиска лидов
	Image       bool         `json:"image"`                  // Генерация изображений (Mistral, Google) — нативный инструмент провайдера
	WebSearch   bool         `json:"web_search"`             // Веб-поиск — нативный инструмент провайдера (google_search / web_search)
	Realtime    bool         `json:"realtime"`               // Голосовой режим реального времени (только OpenAI Realtime API)
	RealtimeVAD *RealtimeVAD `json:"realtime_vad,omitempty"` // Параметры VAD и генерации для Realtime режима
	// Google-специфичные возможности
	Video bool `json:"video"` // Генерация видео (Google Veo/Imagen 3) — нативный инструмент провайдера
	//////////////////////////////////
	Espero   EsperoConfig `json:"espero"` // Настройки ожидания из ModelDataRequest.Espero
	GptType  *GptType     `json:"gpttype"`
	Provider ProviderType `json:"provider"` // "openai=1", "mistral=2..."
}

// RealtimeVAD параметры голосовой активности (VAD) и генерации для OpenAI Realtime API.
// Все поля опциональны — nil/0 означает «использовать значение по умолчанию».
type RealtimeVAD struct {
	// VAD — детекция речи (server_vad)
	Threshold         *float64 `json:"threshold,omitempty"`           // 0.0–1.0, дефолт 0.5
	PrefixPaddingMs   *int     `json:"prefix_padding_ms,omitempty"`   // мс перед речью, дефолт 200
	SilenceDurationMs *int     `json:"silence_duration_ms,omitempty"` // мс тишины до конца фразы, дефолт 500
	InterruptResponse *bool    `json:"interrupt_response,omitempty"`  // прерывать ответ при речи, дефолт true

	// Параметры генерации (передаются только в session.update, НЕ в response.create)
	Temperature             *float64  `json:"temperature,omitempty"`                // 0.6–1.2
	MaxResponseOutputTokens *IntOrInf `json:"max_response_output_tokens,omitempty"` // число или "inf"

	// Транскрибировать входящую речь в текст для логирования в БД
	InputAudioTranscription *bool `json:"input_audio_transcription,omitempty"` // дефолт true

	// Управление приветствием при начале диалога
	InitialGreeting *bool `json:"initial_greeting,omitempty"` // включить/отключить приветствие (дефолт true)
	// Промпт приветствия добавляется в sendInitialGreeting
	Greeting *string `json:"greeting,omitempty"` // явная фраза приветствия (если nil — использовать дефолт)

	// Выбор голоса модели
	Voice *string `json:"voice,omitempty"` // имя голоса для генерации речи (дефолт verse)
}

// IntOrInf хранит значение max_response_output_tokens: 0 → "inf", >0 → число.
type IntOrInf struct {
	Value int // 0 означает "inf"
}

func (v IntOrInf) MarshalJSON() ([]byte, error) {
	if v.Value == 0 {
		return []byte(`"inf"`), nil
	}
	return json.Marshal(v.Value)
}

func (v *IntOrInf) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s == "inf" {
			v.Value = 0
			return nil
		}
		return fmt.Errorf("IntOrInf: неизвестная строка %q", s)
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("IntOrInf: ожидалось число или \"inf\": %w", err)
	}
	v.Value = n
	return nil
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
// Работает для любого провайдера (OpenAI, Mistral...)
func (m *UniversalModel) CreateModel(userId uint32, provider ProviderType, modelData *UniversalModelData, fileIDs []Ids) (UMCR, error) {

	if modelData == nil {
		return UMCR{}, fmt.Errorf("modelData не может быть nil")
	}

	if modelData.GptType == nil || modelData.GptType.Name == "" {
		return UMCR{}, fmt.Errorf("modelData.GptType.Name не может быть пустым")
	}

	switch provider {
	case ProviderOpenAI:
		return m.createModel(userId, modelData, fileIDs)
	case ProviderMistral:
		return m.createMistralModel(userId, modelData, fileIDs)
	case ProviderGoogle:
		return m.createGoogleModel(userId, modelData, fileIDs)
	default:
		return UMCR{}, fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// SaveModel сохраняет модель в БД в универсальном формате
// Работает для любого провайдера (OpenAI, Mistral..)
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
		umcr.Provider,
		data.Name,
		umcr.AssistID,
		compressed.Bytes(),
		data.GptType.ID,
		umcr.AllIds,
		data.Operator,
	)
	if err != nil {
		return fmt.Errorf("ошибка сохранения модели в БД: %w", err)
	}

	return nil
}

// ReadModel получает модель из БД в универсальном формате
// Если provider != nil - получает модель конкретного провайдера
// Если provider == nil - получает активную модель пользователя
// Работает для любого провайдера (OpenAI, Mistral...)
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
			//logger.Debug("Активная модель не найдена", userId)
			return nil, nil
		}
		//logger.Debug("Получение активной модели (Provider: %s)", record.Provider, userId)
	} else {
		// Получаем модель конкретного провайдера
		record, err = m.db.GetModelByProvider(userId, *provider)
		if err != nil {
			return nil, fmt.Errorf("ошибка получения модели провайдера %s: %w", *provider, err)
		}
		if record == nil {
			//logger.Debug("Модель провайдера %s не найдена", *provider, userId)
			return nil, nil
		}
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
	modelData, err := m.DecompressModelData(compressedData, vecIds)
	if err != nil {
		return nil, err
	}

	// Устанавливаем провайдера и AssistantId из БД
	modelData.Provider = record.Provider

	//logger.Debug("Модель успешно загружена (Provider: %s, Name: %s, IsActive: %v)",
	//	modelData.Provider, modelData.Name, record.IsActive, userId)

	return modelData, nil
}

// GetModelAsJSON получает ВСЕ модели пользователя и возвращает их как JSON
// Предназначен для HTTP API endpoints - возвращает готовый JSON для отправки клиенту.
// Возвращает объект с моделями по провайдерам и информацией об активной модели:
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

	// Получаем все модели пользователя
	allModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	// Находим модель с нужным провайдером
	var modelRecord *UserModelRecord
	for i := range allModels {
		if allModels[i].Provider == provider {
			modelRecord = &allModels[i]
			break
		}
	}

	if modelRecord == nil {
		return fmt.Errorf("модель с провайдером %s не найдена для пользователя", provider.String())
	}

	// В зависимости от провайдера удаляем модель
	switch modelRecord.Provider {
	case ProviderOpenAI:
		err = m.deleteModel(userId, modelRecord, deleteFiles, progressCallback)
		if err != nil {
			return err
		}

	case ProviderMistral:
		err = m.deleteMistralModel(userId, modelRecord, deleteFiles, progressCallback)
		if err != nil {
			return err
		}

	case ProviderGoogle:
		err = m.deleteGoogleModel(userId, modelRecord, deleteFiles, progressCallback)
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
			//logger.Warn("Ошибка получения оставшихся моделей: %v", err, userId)
		} else if len(remainingModels) > 0 {
			// Переключаем на первую оставшуюся модель по провайдеру
			newActiveProvider := remainingModels[0].Provider
			err = m.db.SetActiveModelByProvider(userId, newActiveProvider)
			if err != nil {
				//logger.Error("Ошибка автоматического переключения активной модели: %v", err, userId)
			} else {
				//logger.Debug("Активная модель автоматически переключена на провайдер %s после удаления",
				//	newActiveProvider.String(), userId)
				if progressCallback != nil {
					progressCallback(fmt.Sprintf("✅ Активная модель переключена на %s", newActiveProvider.String()))
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

	// Получаем все модели пользователя и находим нужную
	allModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	var existingModelData *UserModelRecord
	for i := range allModels {
		if allModels[i].Provider == provider {
			existingModelData = &allModels[i]
			break
		}
	}

	if existingModelData == nil {
		return fmt.Errorf("запись модели провайдера %s не найдена для пользователя %d", provider, userId)
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
// UpdateModelEveryWhere полностью обновляет модель:
// - Обновляет модель в API провайдера (OpenAI Assistant или Mistral Agent)
// - Управляет файлами и векторными хранилищами
// - Сохраняет изменения в БД
func (m *UniversalModel) UpdateModelEveryWhere(userId uint32, data *UniversalModelData) error {
	// Получаем текущую модель (любого статуса активности)
	provider := data.Provider
	record, err := m.db.GetModelByProviderAnyStatus(userId, provider)
	if err != nil {
		return fmt.Errorf("ошибка получения текущей модели: %w", err)
	}

	if record == nil {
		return fmt.Errorf("модель провайдера %s не найдена для пользователя %d", provider, userId)
	}

	// Распаковываем существующую модель из БД
	compressedData, vecIds, err := m.db.ReadUserModelByProvider(userId, provider)
	if err != nil {
		return fmt.Errorf("ошибка получения данных текущей модели: %w", err)
	}

	if compressedData == nil {
		return fmt.Errorf("данные модели провайдера %s не найдены для пользователя %d", provider, userId)
	}

	existing, err := m.DecompressModelData(compressedData, vecIds)
	if err != nil {
		return fmt.Errorf("ошибка распаковки данных модели: %w", err)
	}

	// Устанавливаем провайдера из БД (он не хранится в Data)
	existing.Provider = provider

	// Проверяем, что провайдер не изменился
	if data.Provider != existing.Provider {
		return fmt.Errorf("нельзя изменить провайдера модели (было: %s, стало: %s)", existing.Provider, data.Provider)
	}

	// Обновляем в зависимости от провайдера
	switch data.Provider {
	case ProviderOpenAI:
		return m.updateOpenAIModelInPlace(userId, existing, data)

	case ProviderMistral:
		return m.updateMistralModelInPlace(userId, existing, data)

	case ProviderGoogle:
		return m.updateGoogleModelInPlace(userId, existing, data)

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
			//logger.Warn("Пропуск модели %d (Provider: %s): ошибка чтения данных: %v", record.ModelId, record.Provider, err, userId)
			continue
		}

		if compressedData == nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): данные отсутствуют", record.ModelId, record.Provider, userId)
			continue
		}

		// Распаковка данных
		modelData, err := m.DecompressModelData(compressedData, vecIds)
		if err != nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): ошибка распаковки: %v", record.ModelId, record.Provider, err, userId)
			continue
		}

		// Обновляем провайдера и AssistantId из БД
		modelData.Provider = record.Provider
		models = append(models, *modelData)
	}

	//logger.Debug("Загружено %d моделей", len(models), userId)
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
			//logger.Warn("Пропуск модели %d (Provider: %s): ошибка чтения данных: %v",
			//	record.ModelId, record.Provider, err, userId)
			continue
		}

		if compressedData == nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): данные отсутствуют",
			//	record.ModelId, record.Provider, userId)
			continue
		}

		// Распаковка данных
		modelData, err := m.DecompressModelData(compressedData, vecIds)
		if err != nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): ошибка распаковки: %v",
			//	record.ModelId, record.Provider, err, userId)
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

	return response, nil
}

// GetActiveUserModel получает активную модель пользователя
func (m *UniversalModel) GetActiveUserModel(userId uint32) (*UniversalModelData, error) {
	record, err := m.db.GetActiveModel(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения активной модели: %w", err)
	}

	if record == nil {
		//logger.Debug("Активная модель не найдена", userId)
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

	modelData, err := m.DecompressModelData(compressedData, vecIds)
	if err != nil {
		return nil, fmt.Errorf("ошибка распаковки активной модели: %w", err)
	}

	// Устанавливаем провайдера и AssistantId из БД
	modelData.Provider = record.Provider

	//logger.Debug("Загружена активная модель (Provider: %s, Name: %s)",
	//	modelData.Provider, modelData.Name, userId)

	return modelData, nil
}

// GetUserModelByProvider получает модель пользователя по провайдеру
func (m *UniversalModel) GetUserModelByProvider(userId uint32, provider ProviderType) (*UniversalModelData, error) {
	record, err := m.db.GetModelByProviderAnyStatus(userId, provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения модели по провайдеру %s: %w", provider, err)
	}

	if record == nil {
		//logger.Debug("Модель провайдера %s не найдена", provider, userId)
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

	modelData, err := m.DecompressModelData(compressedData, vecIds)
	if err != nil {
		return nil, fmt.Errorf("ошибка распаковки модели: %w", err)
	}

	// Устанавливаем провайдера и AssistantId из БД
	modelData.Provider = record.Provider

	//logger.Debug("Загружена модель провайдера %s (ID: %d)",
	//	provider, modelData.Provider, userId)

	return modelData, nil
}

// SetActiveModelByProvider SetActiveModel переключает активную модель пользователя (в транзакции)
func (m *UniversalModel) SetActiveModelByProvider(userId uint32, provider ProviderType) error {
	err := m.db.SetActiveModelByProvider(userId, provider)
	if err != nil {
		return fmt.Errorf("ошибка переключения активной модели: %w", err)
	}

	//logger.Debug("Активная модель переключена на %d", provider, userId)
	return nil
}

// DecompressModelData - распаковывает данные модели из БД и преобразует в UniversalModelData
// Данные в БД хранятся в формате ModelDataRequest (name, prompt, mact, trig, и т.д.)
func (m *UniversalModel) DecompressModelData(compressedData []byte, vecIds *VecIds) (*UniversalModelData, error) {
	// Распаковываем gzip
	reader, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return nil, fmt.Errorf("ошибка распаковки данных модели: %w", err)
	}
	defer func() {
		if e := reader.Close(); e != nil {
			//logger.Warn("error closing gzip reader: %v", e)
		}
	}()

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
	if image, ok := rawData["image"].(bool); ok {
		modelData.Image = image
	}
	if webSearch, ok := rawData["web_search"].(bool); ok {
		modelData.WebSearch = webSearch
	}
	if realtime, ok := rawData["realtime"].(bool); ok {
		modelData.Realtime = realtime
	}
	if vadMap, ok := rawData["realtime_vad"].(map[string]interface{}); ok {
		vad := &RealtimeVAD{}
		if v, ok := vadMap["threshold"].(float64); ok {
			vad.Threshold = &v
		}
		if v, ok := vadMap["prefix_padding_ms"].(float64); ok {
			iv := int(v)
			vad.PrefixPaddingMs = &iv
		}
		if v, ok := vadMap["silence_duration_ms"].(float64); ok {
			iv := int(v)
			vad.SilenceDurationMs = &iv
		}
		if v, ok := vadMap["interrupt_response"].(bool); ok {
			vad.InterruptResponse = &v
		}
		if v, ok := vadMap["temperature"].(float64); ok {
			vad.Temperature = &v
		}
		if v, ok := vadMap["max_response_output_tokens"]; ok {
			ioi := &IntOrInf{}
			if raw, err := json.Marshal(v); err == nil {
				_ = json.Unmarshal(raw, ioi)
			}
			vad.MaxResponseOutputTokens = ioi
		}
		if v, ok := vadMap["input_audio_transcription"].(bool); ok {
			vad.InputAudioTranscription = &v
		}
		if v, ok := vadMap["initial_greeting"].(bool); ok {
			vad.InitialGreeting = &v
		}
		if v, ok := vadMap["greeting"].(string); ok {
			vad.Greeting = &v
		}
		if v, ok := vadMap["voice"].(string); ok {
			vad.Voice = &v
		}
		modelData.RealtimeVAD = vad
	}
	if s3, ok := rawData["s3"].(bool); ok {
		modelData.S3 = s3
	}
	if ha, ok := rawData["haunter"].(bool); ok {
		modelData.Haunter = ha
	}
	if prov, ok := rawData["provider"].(float64); ok {
		modelData.Provider = ProviderType(prov)
	}
	// g_oauth (GOAuth) удалён: Calendar/Sheets теперь управляются исключительно MCP сервером.
	// Поле намеренно игнорируется при десериализации для обратной совместимости с уже сохранёнными данными.
	if esperoMap, ok := rawData["espero"].(map[string]interface{}); ok {
		espero := EsperoConfig{}
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
	if trig, ok := rawData["trig"].([]interface{}); ok {
		triggers := make([]string, 0, len(trig))
		for _, t := range trig {
			if str, ok := t.(string); ok {
				triggers = append(triggers, str)
			}
		}
		modelData.Triggers = triggers
	}

	// Извлекаем gpttype (модель провайдера)
	if gptTypeMap, ok := rawData["gpttype"].(map[string]interface{}); ok {
		gptType := &GptType{}
		if name, ok := gptTypeMap["name"].(string); ok {
			gptType.Name = name
		}
		if id, ok := gptTypeMap["id"].(float64); ok {
			gptType.ID = uint8(id)
		}
		modelData.GptType = gptType
	}

	// AssistantId НЕ хранится в Data - он приходит из user_gpt.AssistantId
	// Будет установлен позже из БД

	// Добавляем fileIds и vectorIds ТОЛЬКО из БД (поле Ids в user_gpt)
	// Они НЕ хранятся в Data, только в отдельном поле Ids
	if vecIds != nil {
		if len(vecIds.FileIds) > 0 {
			modelData.FileIds = vecIds.FileIds
		}
		if len(vecIds.VectorId) > 0 {
			modelData.VecIds.VectorId = vecIds.VectorId
		}
	}

	// Применяем дефолтные значения для RealtimeVAD
	if modelData.RealtimeVAD != nil {
		modelData.RealtimeVAD = applyRealtimeVADDefaults(modelData.RealtimeVAD)
	}

	// Не перезаписываем S3 из лимита хранилища!
	// S3 уже корректно прочитан из сохраненных данных модели
	// Старая логика ошибочно всегда устанавливала S3=true если у пользователя есть лимит

	return modelData, nil
}

// applyRealtimeVADDefaults применяет дефолтные значения к RealtimeVAD
// Дефолты: Threshold=0.5, PrefixPaddingMs=200, SilenceDurationMs=500,
// InterruptResponse=true, InputAudioTranscription=true, InitialGreeting=true
func applyRealtimeVADDefaults(vad *RealtimeVAD) *RealtimeVAD {
	if vad == nil {
		return nil
	}

	// Threshold: дефолт 0.5
	if vad.Threshold == nil {
		v := 0.5
		vad.Threshold = &v
	}

	// PrefixPaddingMs: дефолт 200
	if vad.PrefixPaddingMs == nil {
		v := 200
		vad.PrefixPaddingMs = &v
	}

	// SilenceDurationMs: дефолт 500
	if vad.SilenceDurationMs == nil {
		v := 500
		vad.SilenceDurationMs = &v
	}

	// InterruptResponse: дефолт true
	if vad.InterruptResponse == nil {
		v := true
		vad.InterruptResponse = &v
	}

	// InputAudioTranscription: дефолт true
	if vad.InputAudioTranscription == nil {
		v := true
		vad.InputAudioTranscription = &v
	}

	// InitialGreeting: дефолт true
	if vad.InitialGreeting == nil {
		v := true
		vad.InitialGreeting = &v
	}

	return vad
}

// GetRealUserID получает реальный userId через HTTP запрос к landing серверу
// Универсальный метод для всех провайдеров (OpenAI, Mistral)
func (m *UniversalModel) GetRealUserID(userId uint32) (uint64, error) {
	var url string
	if mode.ProductionMode {
		url = fmt.Sprintf("http://localhost:%s/uid?uid=%d", m.landingPort, userId)
	} else {
		url = fmt.Sprintf("https://localhost:%s/uid?uid=%d", m.landingPort, userId)
	}

	// Создаем HTTP клиент с отключенной проверкой SSL для localhost
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   5 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("ошибка при запросе GetRealUserID: %v", err)
	}
	defer func() {
		if e := resp.Body.Close(); e != nil {
			//logger.Warn("error closing response body: %v", e)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("неожиданный статус ответа GetRealUserID: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("ошибка чтения ответа GetRealUserID: %v", err)
	}

	// Пробуем распарсить как число напрямую
	var userID uint64
	if err := json.Unmarshal(body, &userID); err != nil {
		return 0, fmt.Errorf("ошибка парсинга JSON ответа GetRealUserID: %v", err)
	}

	return userID, nil
}

// ParseModelSchemaJSON парсит статическую JSON Schema в map[string]interface{}
// для использования в response_schema (Google) и json_schema.schema (OpenAI)
// Универсальный метод для обоих провайдеров
// ПРИМЕЧАНИЕ: Эта статическая схема используется только для некоторых случаев.
// OpenAI модели используют динамическую схему из generateModelSchema (open.go)
func ParseModelSchemaJSON(includeAdditionalProperties bool) map[string]interface{} {
	// Базовая схема БЕЗ additionalProperties (для Google)
	var modelSchemaJSON string

	if includeAdditionalProperties {
		// Для OpenAI - С additionalProperties
		modelSchemaJSON = `{
		"type": "object",
		"properties": {
			"message": {
				"type": "string"
			},
			"action": {
				"type": "object",
				"properties": {
					"send_files": {
						"type": "array",
						"default": [],
						"items": {
							"type": "object",
							"properties": {
								"type": {
									"type": "string",
									"enum": ["photo", "video", "audio", "doc"]
								},
								"url": {
									"type": "string"
								},
								"file_name": {
									"type": "string"
								},
								"caption": {
									"type": "string"
								}
							},
							"required": ["type", "url", "file_name", "caption"],
							"additionalProperties": false
						}
					}
				},
				"required": ["send_files"],
				"additionalProperties": false
			},
			"target": { "type": "boolean" },
			"operator": { "type": "boolean" }
		},
		"required": ["message", "action", "target", "operator"],
		"additionalProperties": false
	}`
	} else {
		// Для Google - БЕЗ additionalProperties (Google не поддерживает это поле)
		modelSchemaJSON = `{
		"type": "object",
		"properties": {
			"message": {
				"type": "string"
			},
			"action": {
				"type": "object",
				"properties": {
					"send_files": {
						"type": "array",
						"default": [],
						"items": {
							"type": "object",
							"properties": {
								"type": {
									"type": "string",
									"enum": ["photo", "video", "audio", "doc"]
								},
								"url": {
									"type": "string"
								},
								"file_name": {
									"type": "string"
								},
								"caption": {
									"type": "string"
								}
							},
							"required": ["type", "url", "file_name", "caption"]
						}
					}
				},
				"required": ["send_files"]
			},
			"target": { "type": "boolean" },
			"operator": { "type": "boolean" }
		},
		"required": ["message", "action", "target", "operator"]
	}`
	}

	var schema map[string]interface{}
	err := json.Unmarshal([]byte(modelSchemaJSON), &schema)
	if err != nil {
		// Это не должно произойти, т.к. modelSchemaJSON - валидный JSON
		//logger.Error("[ParseModelSchemaJSON] Ошибка парсинга ModelSchemaJSON: %v", err)
		return map[string]interface{}{} // Возвращаем пустую схему в крайнем случае
	}
	return schema
}
