package model

import (
	"context"
	"io"
	"sync/atomic"

	"github.com/ikermy/AiR_Common/pkg/com"
	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// DB алиас для интерфейса БД
type DB = comdb.Exterior

// Inter интерфейс для работы с моделями Assistant
type Inter interface {
	NewMessage(operator Operator, msgType string, content *AssistResponse, name *string, files ...FileUpload) Message
	GetFileAsReader(userID uint32, url string) (io.Reader, error)
	GetOrSetRespGPT(assist Assistant, dialogID, respId uint64, respName string) (*RespModel, error)
	GetCh(respId uint64) (*Ch, error)
	GetRespIdBydialogID(dialogID uint64) (uint64, error)
	SaveAllContextDuringExit()
	Request(userID uint32, dialogID uint64, text string, files ...FileUpload) (AssistResponse, error)
	RequestStreaming(userID uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...FileUpload) error
	CleanDialogData(dialogID uint64)
	DeleteTempFile(fileID string) error
	TranscribeAudio(userID uint32, audioData []byte, fileName string) (string, error)
	CleanUp()
	DisconnectUser(userID uint32)
	InvalidateUserAgentConfigCache(userID uint32)
	Shutdown(shutCh chan<- com.LogMsg)
}

// RouterInterface минимальный интерфейс для доступа к методам роутера
type RouterInterface interface {
	GetRealUserID(userID uint32) (uint64, error)
	ProvidersWithApiKeys(userID uint32) create.ProvidersAvailability
	RevokeUserAPIKey(userID uint32, provider create.ProviderType) error
}

// OpenAIManager расширяет Inter методами управления моделями OpenAI
type OpenAIManager interface {
	Inter
	CreateModel(userID uint32, provider create.ProviderType, modelData *create.UniversalModelData, fileIDs []create.Ids) (create.UMCR, error)
	UploadDocumentWithEmbedding(userID uint32, docName, content string, metadata create.DocumentMetadata) (string, error)
	SearchSimilarDocuments(userID uint32, query string, limit int) ([]create.VectorDocument, error)
	DeleteDocument(userID uint32, docID string) error
	ListUserDocuments(userID uint32) ([]create.VectorDocument, error)
}

// MistralManager расширяет Inter для Mistral-специфичных методов работы с библиотеками
type MistralManager interface {
	Inter
	CreateModel(userID uint32, provider create.ProviderType, modelData *create.UniversalModelData, fileIDs []create.Ids) (create.UMCR, error)
	UploadFileToProvider(userID uint32, fileName string, fileData []byte) (string, error)
	DeleteDocumentFromLibrary(userID uint32, documentID string) error
	AddFileToLibrary(userID uint32, fileID, fileName string) error
}

// GoogleManager расширяет Inter для Google-специфичных методов
type GoogleManager interface {
	Inter
	CreateModel(userID uint32, provider create.ProviderType, modelData *create.UniversalModelData, fileIDs []create.Ids) (create.UMCR, error)
	UploadDocumentWithEmbedding(userID uint32, docName, content string, metadata create.DocumentMetadata) (string, error)
	SearchSimilarDocuments(userID uint32, query string, limit int) ([]create.VectorDocument, error)
	DeleteDocument(userID uint32, docID string) error
	ListUserDocuments(userID uint32) ([]create.VectorDocument, error)
}

// ActionHandler интерфейс для обработки функций ассистента
type ActionHandler interface {
	RunAction(ctx context.Context, functionName, arguments string, provider create.ProviderType, userID uint32) string
}

// MCPToolDefinition описание инструмента от MCP сервера (tools/list).
// inputSchema не содержит user_id — он передаётся через X-Session-ID заголовок.
type MCPToolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

// MCPConfigProvider расширяет ActionHandler методами получения конфигурации от MCP-сервера.
// Реализуется UniversalActionHandler (pkg/model/action_handler.go).
type MCPConfigProvider interface {
	ActionHandler
	FetchToolsList(ctx context.Context, userID uint32, provider create.ProviderType) ([]MCPToolDefinition, error)
	FetchSystemPrompt(ctx context.Context, userID uint32, provider create.ProviderType) (string, error)
}

// RealtimeEvent — событие голосовой сессии Realtime API (OpenAI / Google Live).
// Type:
//
//	"audio_delta"           — дельта аудио (не используется, аудио идёт через AudioOut канал)
//	"transcript_delta"      — частичный текст ответа модели
//	"input_transcript_done" — транскрипт речи пользователя готов
//	"response_done"         — нормальное завершение хода модели (turnComplete)
//	"interrupted"           — пользователь перебил модель (barge-in); клиент ДОЛЖЕН
//	                          немедленно остановить воспроизведение и очистить буфер
//	"function_result"       — результат вызова инструмента
//	"token_usage"           — статистика токенов
//	"error"                 — ошибка сессии
type RealtimeEvent struct {
	Type  string
	Text  string
	Data  []byte
	Err   error
	Files []File
}

// RealtimeProvider опциональный интерфейс для голосовых сессий реального времени.
// Реализуется OpenAIModel (OpenAI Realtime API) и GoogleModel (Google Multimodal Live API).
type RealtimeProvider interface {
	StartRealtimeSession(userID uint32, dialogID, respId uint64) error
	CloseRealtimeSession(respId uint64)
	SendRealtimeAudio(respId uint64, pcm16 []byte) error
	SubscribeEvents(respId uint64) (<-chan RealtimeEvent, error)
	UnsubscribeEvents(respId uint64, sub <-chan RealtimeEvent)
	GetRealtimeAudio(respId uint64) (<-chan []byte, error)
	GetRealtimeDrain(respId uint64) (<-chan struct{}, error)
	GetRealtimeGenerating(respId uint64) *atomic.Bool
	SetRealtimeDisconnectCallback(respId uint64, callback func(respId uint64)) error
}

// DeltaProcessor интерфейс унифицированной обработки стриминговых дельт.
// Реализуется Startpoint для клиентских каналов (Telegram/WhatsApp/Instagram и т.д.).
type DeltaProcessor interface {
	ProcessStreamDelta(respId uint64, rawChunk string) (text string, complete bool, err error)
	GetStreamDisplayText(respId uint64) string
	ResetStreamAccumulator(respId uint64)
}
