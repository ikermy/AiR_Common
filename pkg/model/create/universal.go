package create

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ikermy/air_common/pkg/model/commdom"
)

// Список моделей которые поддерживают 24h - extending кеширование
var OpenAIExtandingCacheModels = []string{
	"gpt-5.5",
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
// DB INTERFACE
// ============================================================================

// DB interface defines the persistence contract for model management.
type DB interface {
	// SyncProviderModels синхронизирует каталог моделей провайдера с уже полученным списком моделей.
	// При удалении неподдерживаемой модели из провайдера она удаляется из gpt_models и
	// очищает ссылку в user_models (GptModelId = NULL), чтобы пользователь мог выбрать другую.
	SyncProviderModels(union commdom.Union, modelNames []string) (commdom.ProviderModelsSyncResult, error)

	// SaveUserModel сохраняет модель в user_gpt и создает связь в user_models (всё в одной транзакции)
	// Автоматически определяет IsActive (первая модель пользователя становится активной)
	// provider - тип провайдера (1=OpenAI, 2=Mistral)
	SaveUserModel(userID uint32, provider commdom.ProviderType, name, assistantId string, data []byte, def commdom.DefaultProvidersModels, ids json.RawMessage, operator bool) error

	// ReadUserModelByProvider получает сжатые данные модели по провайдеру
	// Возвращает: compressedData, vecIds, error
	ReadUserModelByProvider(userID uint32, provider commdom.ProviderType) ([]byte, *commdom.VecIds, error)

	// GetUserVectorStorage получает ID векторного хранилища (deprecated: используйте ReadUserModelByProvider)
	GetUserVectorStorage(userID uint32) (string, error)

	// GetOrSetUserStorageLimit получает или устанавливает лимит хранилища
	GetOrSetUserStorageLimit(userID uint32, setStorage int64) (remaining uint64, totalLimit uint64, err error)

	// GetAllUserModels GetUserModels получает все модели пользователя из user_models
	GetAllUserModels(userID uint32) ([]commdom.UserModelRecord, error)

	// GetActiveModel получает активную модель пользователя
	GetActiveModel(userID uint32) (*commdom.UserModelRecord, error)

	// GetModelByProvider получает АКТИВНУЮ модель пользователя по провайдеру
	GetModelByProvider(userID uint32, provider commdom.ProviderType) (*commdom.UserModelRecord, error)

	// GetModelByProviderAnyStatus получает модель пользователя по провайдеру независимо от статуса активности
	GetModelByProviderAnyStatus(userID uint32, provider commdom.ProviderType) (*commdom.UserModelRecord, error)

	// SetActiveModel переключает активную модель (в транзакции)
	SetActiveModel(userID uint32, modelId uint64) error

	// SetActiveModelByProvider устанавливает активную модель по провайдеру
	SetActiveModelByProvider(userID uint32, provider commdom.ProviderType) error

	// RemoveModelFromUser удаляет связь модель-пользователь
	RemoveModelFromUser(userID uint32, modelId uint64) error

	// ============================================================================
	// VECTOR EMBEDDINGS - Методы работы с векторными эмбеддингами в MariaDB
	// ВАЖНО: model_id ссылается на user_domain.ModelId для привязки эмбеддингов к модели
	// ============================================================================

	// SaveEmbedding сохраняет векторный эмбеддинг документа в БД с привязкой к модели
	SaveEmbedding(userID uint32, modelId uint64, provider commdom.ProviderType, docID, docName, content string, embedding []float32, metadata commdom.DocumentMetadata) error

	// ListModelEmbeddings возвращает список всех документов конкретной модели и провайдера с эмбеддингами
	ListModelEmbeddings(modelId uint64, provider commdom.ProviderType) ([]commdom.VectorDocument, error)

	// DeleteEmbedding удаляет эмбеддинг документа по ID модели и docID
	DeleteEmbedding(modelId uint64, docID string) error

	// DeleteAllModelEmbeddings удаляет все эмбеддинги конкретной модели
	DeleteAllModelEmbeddings(modelId uint64) error

	// SearchSimilarEmbeddings ищет похожие документы в рамках конкретной модели и провайдера используя VEC_Distance_Cosine
	SearchSimilarEmbeddings(modelId uint64, provider commdom.ProviderType, queryEmbedding []float32, limit int) ([]commdom.VectorDocument, error)

	// GetUserTimeZone получает часовой пояс пользователя из БД
	UserTimeZone(userID uint32) (string, error)

	// UserAPIKey — персональные API-ключи провайдеров для каждого пользователя.
	// GetUserAPIKey возвращает ("", nil) если ключ не задан.
	GetUserAPIKey(userID uint32, provider commdom.ProviderType) (string, error)
	SetUserAPIKey(userID uint32, provider commdom.ProviderType, key string) error
	DeleteUserAPIKey(userID uint32, provider commdom.ProviderType) error
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
// RealtimeVAD универсальные параметры голосовой активности (VAD) и генерации.
// Общие поля работают для всех провайдеров. Провайдер-специфичные параметры
// вынесены в отдельные вложенные структуры (Google и т.д.).
// Все поля опциональны — nil/0 означает «использовать значение по умолчанию».
/* type RealtimeVAD struct {
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

	// ── Mistral-специфичные параметры ───────────────────────────────────────
	// Используются каскадным pipeline Voxtral STT → Mistral LLM → Voxtral TTS.
	Mistral *MistralRealtimeVAD `json:"mistral,omitempty"`
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
*/

// MistralRealtimeVAD содержит настройки каскадного голосового pipeline Mistral.
// RealtimeModel выбирается отдельно из realtime_models, а эти поля управляют
// STT, sentence-to-speech и voice cloning.
/* type MistralRealtimeVAD struct {
	STTModel         *string                  `json:"stt_model,omitempty"`
	TTSModel         *string                  `json:"tts_model,omitempty"`
	Voice            *string                  `json:"voice,omitempty"`
	VoiceID          *string                  `json:"voice_id,omitempty"`
	ReferenceAudioID *string                  `json:"reference_audio_id,omitempty"`
	VoiceClone       *MistralVoiceCloneConfig `json:"voice_clone,omitempty"`
	SpeechFormat     *string                  `json:"speech_format,omitempty"` // wav или mp3
	STTLanguage      *string                  `json:"stt_language,omitempty"`
}

// MistralVoiceCloneConfig stores voice-cloning resource references.
// Raw reference audio is deliberately not persisted in model data.
type MistralVoiceCloneConfig struct {
	Enabled             bool   `json:"enabled"`
	ProfileID           string `json:"profile_id,omitempty"`
	ReferenceAudioID    string `json:"reference_audio_id,omitempty"`
	ReferenceFormat     string `json:"reference_format,omitempty"`
	ReferenceDurationMs int    `json:"reference_duration_ms,omitempty"`
}

// MistralVoiceProfile is a persisted reference to a voice managed by Mistral
// or by the application voice-profile storage. Raw audio is never included.
type MistralVoiceProfile struct {
	ID                  string `json:"id"`
	Name                string `json:"name,omitempty"`
	TTSModel            string `json:"tts_model,omitempty"`
	ReferenceAudioID    string `json:"reference_audio_id,omitempty"`
	ReferenceFormat     string `json:"reference_format,omitempty"`
	ReferenceDurationMs int    `json:"reference_duration_ms,omitempty"`
}

// Validate checks the mutually exclusive voice profile references.
func (v *MistralVoiceCloneConfig) Validate() error {
	if v == nil || !v.Enabled {
		return nil
	}
	if v.ProfileID == "" && v.ReferenceAudioID == "" {
		return fmt.Errorf("для voice cloning не задан profile_id или reference_audio_id")
	}
	if v.ProfileID != "" && v.ReferenceAudioID != "" {
		return fmt.Errorf("нельзя одновременно задавать profile_id и reference_audio_id")
	}
	if v.ReferenceDurationMs != 0 && (v.ReferenceDurationMs < 2000 || v.ReferenceDurationMs > 10000) {
		return fmt.Errorf("длительность reference audio должна быть от 2000 до 10000 мс")
	}
	return nil
}
*/
