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
	// RealtimeOpenAIModel фиксированная realtime-модель OpenAI
	RealtimeOpenAIModel = "gpt-realtime-mini"
	//RealtimeOpenAIModel = "gpt-realtime"

	// RealtimeGoogleModel — Live-модель для Google Live API (AI Studio).
	RealtimeGoogleModel = "gemini-3.1-flash-live-preview"

	// RealtimeOpenAIURL базовый WebSocket URL для OpenAI Realtime API
	RealtimeOpenAIURL = "wss://api.openai.com/v1/realtime"

	// RealtimeGoogleURL — WebSocket endpoint Google Live API.
	RealtimeGoogleURL = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

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

// AllProviders содержит все зарегистрированные провайдеры в порядке добавления.
// При добавлении нового провайдера достаточно добавить его сюда и в switch-и String/FromString/IsValid.
var AllProviders = []ProviderType{
	ProviderOpenAI,
	ProviderMistral,
	ProviderGoogle,
}

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
	for _, known := range AllProviders {
		if p == known {
			return true
		}
	}
	return false
}

type DB interface {
	// SaveUserModel сохраняет модель в user_gpt и создает связь в user_models (всё в одной транзакции)
	// Автоматически определяет IsActive (первая модель пользователя становится активной)
	// provider - тип провайдера (1=OpenAI, 2=Mistral)
	SaveUserModel(userID uint32, provider ProviderType, name, assistantId string, data []byte, model uint8, ids json.RawMessage, operator bool) error

	// ReadUserModelByProvider получает сжатые данные модели по провайдеру
	// Возвращает: compressedData, vecIds, error
	ReadUserModelByProvider(userID uint32, provider ProviderType) ([]byte, *VecIds, error)

	// GetUserVectorStorage получает ID векторного хранилища (deprecated: используйте ReadUserModelByProvider)
	GetUserVectorStorage(userID uint32) (string, error)

	// GetOrSetUserStorageLimit получает или устанавливает лимит хранилища
	GetOrSetUserStorageLimit(userID uint32, setStorage int64) (remaining uint64, totalLimit uint64, err error)

	// GetAllUserModels GetUserModels получает все модели пользователя из user_models
	GetAllUserModels(userID uint32) ([]UserModelRecord, error)

	// GetActiveModel получает активную модель пользователя
	GetActiveModel(userID uint32) (*UserModelRecord, error)

	// GetModelByProvider получает АКТИВНУЮ модель пользователя по провайдеру
	GetModelByProvider(userID uint32, provider ProviderType) (*UserModelRecord, error)

	// GetModelByProviderAnyStatus получает модель пользователя по провайдеру независимо от статуса активности
	GetModelByProviderAnyStatus(userID uint32, provider ProviderType) (*UserModelRecord, error)

	// SetActiveModel переключает активную модель (в транзакции)
	SetActiveModel(userID uint32, modelId uint64) error

	// SetActiveModelByProvider устанавливает активную модель по провайдеру
	SetActiveModelByProvider(userID uint32, provider ProviderType) error

	// RemoveModelFromUser удаляет связь модель-пользователь
	RemoveModelFromUser(userID uint32, modelId uint64) error

	// ============================================================================
	// VECTOR EMBEDDINGS - Методы работы с векторными эмбеддингами в MariaDB
	// ВАЖНО: model_id ссылается на user_create.ModelId для привязки эмбеддингов к модели
	// ============================================================================

	// SaveEmbedding сохраняет векторный эмбеддинг документа в БД с привязкой к модели
	SaveEmbedding(userID uint32, modelId uint64, provider ProviderType, docID, docName, content string, embedding []float32, metadata DocumentMetadata) error

	// ListModelEmbeddings возвращает список всех документов конкретной модели и провайдера с эмбеддингами
	ListModelEmbeddings(modelId uint64, provider ProviderType) ([]VectorDocument, error)

	// DeleteEmbedding удаляет эмбеддинг документа по ID модели и docID
	DeleteEmbedding(modelId uint64, docID string) error

	// DeleteAllModelEmbeddings удаляет все эмбеддинги конкретной модели
	DeleteAllModelEmbeddings(modelId uint64) error

	// SearchSimilarEmbeddings ищет похожие документы в рамках конкретной модели и провайдера используя VEC_Distance_Cosine
	SearchSimilarEmbeddings(modelId uint64, provider ProviderType, queryEmbedding []float32, limit int) ([]VectorDocument, error)

	// GetUserTimeZone получает часовой пояс пользователя из БД
	UserTimeZone(userID uint32) (string, error)

	// UserAPIKey — персональные API-ключи провайдеров для каждого пользователя.
	// GetUserAPIKey возвращает ("", nil) если ключ не задан.
	GetUserAPIKey(userID uint32, provider ProviderType) (string, error)
	SetUserAPIKey(userID uint32, provider ProviderType, key string) error
	DeleteUserAPIKey(userID uint32, provider ProviderType) error
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
	db            DB
}

// New создаёт новый экземпляр UniversalModel для управления моделями
// любой ключь может быть пустым (если не используется соответствующий провайдер)
func New(ctx context.Context, db DB) *UniversalModel {
	m := &UniversalModel{
		ctx: ctx,
		db:  db,
	}

	// Инициализируем OpenAI клиент БЕЗ глобального ключа — глобальные ключи из конфига
	// должны игнорироваться полностью. Персональный ключ читается из БД через keyResolver.
	m.openaiClient = &OpenAIAgentClient{
		url: mode.OpenAIAgentsURL,
		ctx: ctx,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		universalModel: m, // Передаем ссылку на universalModel для доступа к GetRealUserID
	}
	m.openaiClient.SetKeyResolver(func(userID uint32) string {
		if key, err := db.GetUserAPIKey(userID, ProviderOpenAI); err == nil {
			return key
		}
		return ""
	})

	// Инициализируем Mistral клиент БЕЗ глобального ключа — глобальные ключи из конфига
	// должны игнорироваться полностью. Персональный ключ читается из БД через keyResolver.
	m.mistralClient = &MistralAgentClient{
		url:            mode.MistralAgentsURL,
		ctx:            ctx,
		universalModel: m,
	}
	m.mistralClient.SetKeyResolver(func(userID uint32) string {
		if key, err := db.GetUserAPIKey(userID, ProviderMistral); err == nil {
			return key
		}
		return ""
	})

	// Инициализируем google клиент БЕЗ глобального ключа — глобальные ключи из конфига
	// должны игнорироваться полностью. Персональный ключ читается из БД через keyResolver.
	m.googleClient = &GoogleAgentClient{
		url:            mode.GoogleAgentsURL,
		ctx:            ctx,
		universalModel: m,
	}
	m.googleClient.SetKeyResolver(func(userID uint32) string {
		if key, err := db.GetUserAPIKey(userID, ProviderGoogle); err == nil {
			return key
		}
		return ""
	})

	return m
}

// SetMistralMCPFetchers устанавливает MCP-fetchers на mistralClient.
// Вызывается из mistral/model.go после инициализации UniversalModel.
func (m *UniversalModel) SetMistralMCPFetchers(promptFetcher GooglePromptHintFetcher, toolsFetcher GoogleFunctionDeclarationsFetcher) {
	if m.mistralClient != nil {
		m.mistralClient.SetMCPConfigFetchers(promptFetcher, toolsFetcher)
	}
}

// ============================================================================
// USER API KEYS — персональные API-ключи провайдеров
// ============================================================================

// ProvidersAvailability содержит результат проверки API-ключей по всем провайдерам.
type ProvidersAvailability struct {
	Available   []string `json:"available"`   // Провайдеры с действующим API-ключом в БД
	Unavailable []string `json:"unavailable"` // Провайдеры без API-ключа
}

// ProvidersWithApiKeys проверяет каждый зарегистрированный провайдер и возвращает
// два списка: с ключом и без ключа.
func (m *UniversalModel) ProvidersWithApiKeys(userID uint32) ProvidersAvailability {
	result := ProvidersAvailability{
		Available:   make([]string, 0),
		Unavailable: make([]string, 0),
	}
	for _, p := range AllProviders {
		key, err := m.db.GetUserAPIKey(userID, p)
		if err == nil && key != "" {
			result.Available = append(result.Available, p.String())
		} else {
			result.Unavailable = append(result.Unavailable, p.String())
		}
	}
	return result
}

// SetUserAPIKey сохраняет персональный API-ключ пользователя для указанного провайдера.
func (m *UniversalModel) SetUserAPIKey(userID uint32, provider ProviderType, key string) error {
	return m.db.SetUserAPIKey(userID, provider, key)
}

// GetUserAPIKey возвращает персональный API-ключ пользователя. Пустая строка — ключ не задан.
func (m *UniversalModel) GetUserAPIKey(userID uint32, provider ProviderType) (string, error) {
	return m.db.GetUserAPIKey(userID, provider)
}

// DeleteUserAPIKey удаляет персональный API-ключ пользователя для провайдера.
func (m *UniversalModel) DeleteUserAPIKey(userID uint32, provider ProviderType) error {
	return m.db.DeleteUserAPIKey(userID, provider)
}

type GptType struct {
	Name string `json:"name"`
	ID   uint8  `json:"id"`
}

// GOAuth хранит флаги доступа к Google OAuth сервисам (Calendar, Sheets).
// Используется MCP-сервером для определения доступных инструментов.
// Провайдеры (OpenAI/Mistral/Google) не используют эти флаги напрямую —
// инструменты и инструкции приходят через FetchToolsList/FetchSystemPrompt.
type GOAuth struct {
	Calendar bool `json:"calendar"`
	Sheets   bool `json:"sheets"`
}

// Enabled возвращает true если хотя бы один GOAuth сервис доступен
func (g GOAuth) Enabled() bool {
	return g.Calendar || g.Sheets
}

// UniversalModelData универсальная структура хранения данных моделей
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
	// GOAuth — флаги доступа к Google OAuth сервисам (Calendar, Sheets).
	// Используется MCP-сервером. Провайдеры получают инструменты только через FetchToolsList.
	GOAuth GOAuth `json:"g_oauth"`
	//////////////////////////////////
	Espero   EsperoConfig `json:"espero"` // Настройки ожидания из ModelDataRequest.Espero
	GptType  *GptType     `json:"gpttype"`
	Provider ProviderType `json:"provider"` // "openai=1", "mistral=2..."
}

// RealtimeVAD универсальные параметры голосовой активности (VAD) и генерации.
// Общие поля работают для всех провайдеров. Провайдер-специфичные параметры
// вынесены в отдельные вложенные структуры (Google и т.д.).
// Все поля опциональны — nil/0 означает «использовать значение по умолчанию».
type RealtimeVAD struct {
	// ── Общие параметры VAD (все провайдеры) ────────────────────────────────
	SilenceDurationMs *int  `json:"silence_duration_ms,omitempty"` // мс тишины до конца фразы, дефолт 500
	InterruptResponse *bool `json:"interrupt_response,omitempty"`  // прерывать ответ при речи, дефолт true

	// ── Общие параметры генерации (все провайдеры) ──────────────────────────
	Temperature *float64 `json:"temperature,omitempty"` // 0.0–2.0

	// ── Транскрипция входящей речи (все провайдеры) ─────────────────────────
	InputAudioTranscription *bool `json:"input_audio_transcription,omitempty"` // STT пользователя, дефолт true

	// ── Управление приветствием (все провайдеры) ────────────────────────────
	InitialGreeting *bool   `json:"initial_greeting,omitempty"` // включить/отключить приветствие, дефолт true
	Greeting        *string `json:"greeting,omitempty"`         // явная фраза (nil → авто-генерация)

	// ── Выбор голоса (все провайдеры) ───────────────────────────────────────
	// OpenAI: имена типа "verse", "alloy"; Google: если не задан Google.VoiceName, используется это поле.
	Voice *string `json:"voice,omitempty"` // имя голоса, дефолт зависит от провайдера

	// ── OpenAI-специфичные параметры ────────────────────────────────────────
	Threshold               *float64  `json:"threshold,omitempty"`                  // VAD порог, дефолт 0.5
	PrefixPaddingMs         *int      `json:"prefix_padding_ms,omitempty"`          // мс перед речью, дефолт 200
	MaxResponseOutputTokens *IntOrInf `json:"max_response_output_tokens,omitempty"` // число или "inf"

	// ── Google-специфичные параметры ────────────────────────────────────────
	// При наличии переопределяют соответствующие общие поля для Google провайдера.
	Google *GoogleRealtimeVAD `json:"google,omitempty"`
}

// GoogleRealtimeVAD Google-специфичные параметры для Multimodal Live API.
// Поля с совпадающим смыслом (VoiceName, SilenceDurationMs, BargeIn, InputAudioTranscription)
// имеют приоритет над общими полями RealtimeVAD при работе с Google провайдером.
type GoogleRealtimeVAD struct {
	// Голос и язык
	VoiceName    *string `json:"voice_name,omitempty"`    // prebuilt_voice_config.voice_name, дефолт "Puck"
	LanguageCode *string `json:"language_code,omitempty"` // speech_config.language_code, напр. "ru-RU"

	// Транскрипция
	InputAudioTranscription  *bool `json:"input_audio_transcription,omitempty"`  // STT пользователя, дефолт true
	OutputAudioTranscription *bool `json:"output_audio_transcription,omitempty"` // субтитры модели, дефолт false

	// VAD
	AutomaticActivityDetection *bool `json:"automatic_activity_detection,omitempty"` // авто-VAD, дефолт true
	BargeIn                    *bool `json:"barge_in,omitempty"`                     // перебивание модели, дефолт true
	SilenceDurationMs          *int  `json:"silence_duration_ms,omitempty"`          // мс тишины, дефолт 500
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
func (m *UniversalModel) CreateModel(userID uint32, provider ProviderType, modelData *UniversalModelData, fileIDs []Ids) (UMCR, error) {

	if modelData == nil {
		return UMCR{}, fmt.Errorf("modelData не может быть nil")
	}

	if modelData.GptType == nil || modelData.GptType.Name == "" {
		return UMCR{}, fmt.Errorf("modelData.GptType.Name не может быть пустым")
	}

	switch provider {
	case ProviderOpenAI:
		return m.createModel(userID, modelData, fileIDs)
	case ProviderMistral:
		return m.createMistralModel(userID, modelData, fileIDs)
	case ProviderGoogle:
		return m.createGoogleModel(userID, modelData, fileIDs)
	default:
		return UMCR{}, fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// SaveModel сохраняет модель в БД в универсальном формате
// Работает для любого провайдера (OpenAI, Mistral..)
// Автоматически устанавливает модель как активную если это первая модель пользователя
func (m *UniversalModel) SaveModel(userID uint32, umcr UMCR, data *UniversalModelData) error {
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
		userID,
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
func (m *UniversalModel) ReadModel(userID uint32, provider *ProviderType) (*UniversalModelData, error) {
	var record *UserModelRecord
	var err error

	// Если провайдер не указан - получаем активную модель
	if provider == nil {
		record, err = m.db.GetActiveModel(userID)
		if err != nil {
			return nil, fmt.Errorf("ошибка получения активной модели: %w", err)
		}
		if record == nil {
			//logger.Debug("Активная модель не найдена", userID)
			return nil, nil
		}
		//logger.Debug("Получение активной модели (Provider: %s)", record.Provider, userID)
	} else {
		// Получаем модель конкретного провайдера
		record, err = m.db.GetModelByProvider(userID, *provider)
		if err != nil {
			return nil, fmt.Errorf("ошибка получения модели провайдера %s: %w", *provider, err)
		}
		if record == nil {
			//logger.Debug("Модель провайдера %s не найдена", *provider, userID)
			return nil, nil
		}
	}

	// Получаем данные из БД по провайдеру
	compressedData, vecIds, err := m.db.ReadUserModelByProvider(userID, record.Provider)
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
	//	modelData.Provider, modelData.Name, record.IsActive, userID)

	return modelData, nil
}

// GetModelAsJSON получает ВСЕ модели пользователя и возвращает их как JSON
// Предназначен для HTTP API endpoints - возвращает готовый JSON для отправки клиенту.
// Возвращает объект с моделями по провайдерам и информацией об активной модели:
func (m *UniversalModel) GetModelAsJSON(userID uint32) (json.RawMessage, error) {
	// Получаем все модели пользователя
	response, err := m.GetAllUserModelsResponse(userID)
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
func (m *UniversalModel) DeleteModel(userID uint32, provider ProviderType, deleteFiles bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Получение информации о модели пользователя...")
	}

	// Получаем все модели пользователя
	allModels, err := m.db.GetAllUserModels(userID)
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
		err = m.deleteModel(userID, modelRecord, deleteFiles, progressCallback)
		if err != nil {
			return err
		}

	case ProviderMistral:
		err = m.deleteMistralModel(userID, modelRecord, deleteFiles, progressCallback)
		if err != nil {
			return err
		}

	case ProviderGoogle:
		err = m.deleteGoogleModel(userID, modelRecord, deleteFiles, progressCallback)
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

	err = m.db.RemoveModelFromUser(userID, modelRecord.ModelId)
	if err != nil {
		return fmt.Errorf("ошибка удаления связи из user_models: %w", err)
	}

	// Если удалённая модель была активной - переключаем на оставшуюся
	if modelRecord.IsActive {
		remainingModels, err := m.db.GetAllUserModels(userID)
		if err != nil {
			//logger.Warn("Ошибка получения оставшихся моделей: %v", err, userID)
		} else if len(remainingModels) > 0 {
			// Переключаем на первую оставшуюся модель по провайдеру
			newActiveProvider := remainingModels[0].Provider
			err = m.db.SetActiveModelByProvider(userID, newActiveProvider)
			if err != nil {
				//logger.Error("Ошибка автоматического переключения активной модели: %v", err, userID)
			} else {
				//logger.Debug("Активная модель автоматически переключена на провайдер %s после удаления",
				//	newActiveProvider.String(), userID)
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
func (m *UniversalModel) UpdateModelToDB(userID uint32, data *UniversalModelData) error {
	// Проверяем существование модели
	provider := data.Provider
	existing, err := m.ReadModel(userID, &provider)
	if err != nil {
		return fmt.Errorf("ошибка проверки существующей модели: %w", err)
	}

	if existing == nil {
		return fmt.Errorf("модель провайдера %s не найдена для пользователя %d", provider, userID)
	}

	// Получаем все модели пользователя и находим нужную
	allModels, err := m.db.GetAllUserModels(userID)
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
		return fmt.Errorf("запись модели провайдера %s не найдена для пользователя %d", provider, userID)
	}

	// Сериализуем vecIds в JSON
	vecIdsJSON, err := json.Marshal(data.VecIds)
	if err != nil {
		return fmt.Errorf("failed to marshal vector IDs: %w", err)
	}

	// Сохраняем обновленные данные
	return m.SaveModel(userID, UMCR{
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
func (m *UniversalModel) UpdateModelEveryWhere(userID uint32, data *UniversalModelData) error {
	// Получаем текущую модель (любого статуса активности)
	provider := data.Provider
	record, err := m.db.GetModelByProviderAnyStatus(userID, provider)
	if err != nil {
		return fmt.Errorf("ошибка получения текущей модели: %w", err)
	}

	if record == nil {
		return fmt.Errorf("модель провайдера %s не найдена для пользователя %d", provider, userID)
	}

	// Распаковываем существующую модель из БД
	compressedData, vecIds, err := m.db.ReadUserModelByProvider(userID, provider)
	if err != nil {
		return fmt.Errorf("ошибка получения данных текущей модели: %w", err)
	}

	if compressedData == nil {
		return fmt.Errorf("данные модели провайдера %s не найдены для пользователя %d", provider, userID)
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
		return m.updateOpenAIModelInPlace(userID, existing, data)

	case ProviderMistral:
		return m.updateMistralModelInPlace(userID, existing, data)

	case ProviderGoogle:
		return m.updateGoogleModelInPlace(userID, existing, data)

	default:
		return fmt.Errorf("неизвестный провайдер: %s", data.Provider)
	}
}

// ============================================================================
// Методы для работы с множественными моделями
// ============================================================================

// GetUserModels получает все модели пользователя
func (m *UniversalModel) GetUserModels(userID uint32) ([]UniversalModelData, error) {
	records, err := m.db.GetAllUserModels(userID)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	if len(records) == 0 {
		return []UniversalModelData{}, nil
	}

	models := make([]UniversalModelData, 0, len(records))
	for _, record := range records {
		// Читаем данные модели по провайдеру
		compressedData, vecIds, err := m.db.ReadUserModelByProvider(userID, record.Provider)
		if err != nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): ошибка чтения данных: %v", record.ModelId, record.Provider, err, userID)
			continue
		}

		if compressedData == nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): данные отсутствуют", record.ModelId, record.Provider, userID)
			continue
		}

		// Распаковка данных
		modelData, err := m.DecompressModelData(compressedData, vecIds)
		if err != nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): ошибка распаковки: %v", record.ModelId, record.Provider, err, userID)
			continue
		}

		// Обновляем провайдера и AssistantId из БД
		modelData.Provider = record.Provider
		models = append(models, *modelData)
	}

	//logger.Debug("Загружено %d моделей", len(models), userID)
	return models, nil
}

// GetAllUserModelsResponse получает все модели пользователя в формате для API
// Возвращает объект с моделями по провайдерам и информацией об активной модели
func (m *UniversalModel) GetAllUserModelsResponse(userID uint32) (*UserModelsResponse, error) {
	records, err := m.db.GetAllUserModels(userID)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}
	response := &UserModelsResponse{
		Models: make(map[string]*UniversalModelData),
	}

	var activeProvider ProviderType

	for _, record := range records {
		// Читаем данные модели по провайдеру
		compressedData, vecIds, err := m.db.ReadUserModelByProvider(userID, record.Provider)
		if err != nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): ошибка чтения данных: %v",
			//	record.ModelId, record.Provider, err, userID)
			continue
		}

		if compressedData == nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): данные отсутствуют",
			//	record.ModelId, record.Provider, userID)
			continue
		}

		// Распаковка данных
		modelData, err := m.DecompressModelData(compressedData, vecIds)
		if err != nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): ошибка распаковки: %v",
			//	record.ModelId, record.Provider, err, userID)
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
func (m *UniversalModel) GetActiveUserModel(userID uint32) (*UniversalModelData, error) {
	record, err := m.db.GetActiveModel(userID)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения активной модели: %w", err)
	}

	if record == nil {
		//logger.Debug("Активная модель не найдена", userID)
		return nil, nil
	}

	// Читаем данные модели по провайдеру
	compressedData, vecIds, err := m.db.ReadUserModelByProvider(userID, record.Provider)
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
	//	modelData.Provider, modelData.Name, userID)

	return modelData, nil
}

// GetUserModelByProvider получает модель пользователя по провайдеру
func (m *UniversalModel) GetUserModelByProvider(userID uint32, provider ProviderType) (*UniversalModelData, error) {
	record, err := m.db.GetModelByProviderAnyStatus(userID, provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения модели по провайдеру %s: %w", provider, err)
	}

	if record == nil {
		//logger.Debug("Модель провайдера %s не найдена", provider, userID)
		return nil, nil
	}

	// Читаем данные модели по провайдеру
	compressedData, vecIds, err := m.db.ReadUserModelByProvider(userID, record.Provider)
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
	//	provider, modelData.Provider, userID)

	return modelData, nil
}

// SetActiveModelByProvider SetActiveModel переключает активную модель пользователя (в транзакции)
func (m *UniversalModel) SetActiveModelByProvider(userID uint32, provider ProviderType) error {
	err := m.db.SetActiveModelByProvider(userID, provider)
	if err != nil {
		return fmt.Errorf("ошибка переключения активной модели: %w", err)
	}

	//logger.Debug("Активная модель переключена на %d", provider, userID)
	return nil
}

// DecompressModelData распаковывает и десериализует данные модели из БД.
// UniversalModelData имеет те же JSON-теги что и формат хранения, поэтому
// используется прямой json.Unmarshal вместо ручного поля-за-полем парсинга.
//
// После десериализации:
//   - vecIds (FileIds и VectorId) переносятся из отдельного поля БД
//   - RealtimeVAD получает дефолтные значения для nil-полей
func (m *UniversalModel) DecompressModelData(compressedData []byte, vecIds *VecIds) (*UniversalModelData, error) {
	reader, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return nil, fmt.Errorf("ошибка распаковки данных модели: %w", err)
	}
	defer func() { _ = reader.Close() }()

	decompressed, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения распакованных данных: %w", err)
	}

	modelData := &UniversalModelData{}
	if err := json.Unmarshal(decompressed, modelData); err != nil {
		return nil, fmt.Errorf("ошибка десериализации данных модели: %w", err)
	}

	// FileIds и VectorId хранятся в отдельном поле БД (Ids в user_gpt),
	// а не внутри сжатых данных модели — применяем их поверх.
	if vecIds != nil {
		if len(vecIds.FileIds) > 0 {
			modelData.FileIds = vecIds.FileIds
		}
		if len(vecIds.VectorId) > 0 {
			modelData.VecIds.VectorId = vecIds.VectorId
		}
	}

	// Применяем дефолтные значения для nil-полей RealtimeVAD
	if modelData.RealtimeVAD != nil {
		modelData.RealtimeVAD = applyRealtimeVADDefaults(modelData.RealtimeVAD)
	}

	return modelData, nil
}

// applyRealtimeVADDefaults применяет дефолтные значения к RealtimeVAD и вложенному GoogleRealtimeVAD.
//
// Общие дефолты: SilenceDurationMs=500, InterruptResponse=true,
// InputAudioTranscription=true, InitialGreeting=true.
//
// OpenAI-специфичные дефолты: Threshold=0.5, PrefixPaddingMs=200.
//
// Google-специфичные дефолты (Google-блок): VoiceName="Puck",
// InputAudioTranscription=true, OutputAudioTranscription=false,
// AutomaticActivityDetection=true, BargeIn=true, SilenceDurationMs=500.
func applyRealtimeVADDefaults(vad *RealtimeVAD) *RealtimeVAD {
	if vad == nil {
		return nil
	}

	// ── Общие параметры ──────────────────────────────────────────────────────

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

	// ── OpenAI-специфичные дефолты ───────────────────────────────────────────

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

	// ── Google-специфичные дефолты ───────────────────────────────────────────
	if vad.Google != nil {
		g := vad.Google

		// VoiceName: дефолт "Puck"
		if g.VoiceName == nil {
			v := GoogleRealtimeDefaultVoice
			g.VoiceName = &v
		}

		// InputAudioTranscription: дефолт true
		if g.InputAudioTranscription == nil {
			v := true
			g.InputAudioTranscription = &v
		}

		// OutputAudioTranscription: дефолт false
		if g.OutputAudioTranscription == nil {
			v := false
			g.OutputAudioTranscription = &v
		}

		// AutomaticActivityDetection: дефолт true
		if g.AutomaticActivityDetection == nil {
			v := true
			g.AutomaticActivityDetection = &v
		}

		// BargeIn: дефолт true
		if g.BargeIn == nil {
			v := true
			g.BargeIn = &v
		}

		// SilenceDurationMs: дефолт 500
		if g.SilenceDurationMs == nil {
			v := GoogleRealtimeSilenceDurationMs
			g.SilenceDurationMs = &v
		}
	}

	return vad
}

// GetRealUserID получает реальный userID через HTTP запрос к landing серверу
// Универсальный метод для всех провайдеров (OpenAI, Mistral)
func (m *UniversalModel) GetRealUserID(userID uint32) (uint64, error) {
	// Строим URL для запроса к landing серверу
	//var url string
	//if mode.ProductionMode {
	//	url = fmt.Sprintf("http://localhost:%s/system/uid?uid=%d", mode.LandingPort, userID)
	//} else {
	//	url = fmt.Sprintf("https://localhost:%s/system/uid?uid=%d", mode.LandingPort, userID)
	//}

	url := fmt.Sprintf("http://airlanding:8081/uid?uid=%d", userID)

	// Создаём HTTP клиент с отключённой проверкой SSL для localhost
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

	// Обрабатываем HTTP ответ
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("неожиданный статус ответа GetRealUserID: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("ошибка чтения ответа GetRealUserID: %v", err)
	}

	// Парсим JSON ответ как число
	var value uint64
	if err := json.Unmarshal(body, &value); err != nil {
		return 0, fmt.Errorf("ошибка парсинга JSON ответа GetRealUserID: %v", err)
	}

	return value, nil
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
