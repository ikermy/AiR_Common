package model

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ikermy/AiR_Common/pkg/com"
	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// ============================================================================
// ТИПЫ ДАННЫХ И ИНТЕРФЕЙСЫ
// ============================================================================

// DB алиас для интерфейса БД
type DB = comdb.Exterior

// Inter интерфейс для работы с моделями Assistant
type Inter interface {
	NewMessage(operator Operator, msgType string, content *AssistResponse, name *string, files ...FileUpload) Message
	GetFileAsReader(userId uint32, url string) (io.Reader, error)
	GetOrSetRespGPT(assist Assistant, dialogID, respId uint64, respName string) (*RespModel, error)
	GetCh(respId uint64) (*Ch, error)
	GetRespIdBydialogID(dialogID uint64) (uint64, error)
	SaveAllContextDuringExit()
	Request(userId uint32, dialogID uint64, text string, files ...FileUpload) (AssistResponse, error)
	RequestStreaming(userId uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...FileUpload) error
	CleanDialogData(dialogID uint64)
	DeleteTempFile(fileID string) error
	TranscribeAudio(userId uint32, audioData []byte, fileName string) (string, error)
	CleanUp()                                     // Фоновая очистка устаревших записей
	InvalidateUserAgentConfigCache(userId uint32) // Инвалидирует кэш конфигурации модели для пользователя
	Shutdown(shutCh chan<- com.LogMsg)
}

// RouterInterface минимальный интерфейс для доступа к методам роутера
type RouterInterface interface {
	GetRealUserID(userId uint32) (uint64, error)
}

// OpenAIManager интерфейс для управления моделями (создание, удаление, работа с файлами)
// Расширяет базовый интерфейс Inter дополнительными методами управления
// Использует типы из пакета github.com/ikermy/AiR_Common/pkg/model/create (package models)
type OpenAIManager interface {
	Inter // Встраиваем базовый интерфейс

	// CreateModel создаёт новую модель у провайдера
	// fileIDs должен быть типа []create.Ids из пакета pkg/model/create
	CreateModel(userId uint32, provider create.ProviderType, modelData *create.UniversalModelData, fileIDs []create.Ids) (create.UMCR, error)

	// Vector Embedding methods - работа со встроенным векторным хранилищем (OpenAI Embeddings API + MariaDB)
	UploadDocumentWithEmbedding(userId uint32, docName, content string, metadata create.DocumentMetadata) (string, error)
	SearchSimilarDocuments(userId uint32, query string, limit int) ([]create.VectorDocument, error)
	DeleteDocument(userId uint32, docID string) error
	ListUserDocuments(userId uint32) ([]create.VectorDocument, error)
}

// MistralManager расширяет Inter для Mistral-специфичных методов работы с библиотеками
type MistralManager interface {
	Inter // Встраиваем базовый интерфейс

	// CreateModel создаёт новую модель у провайдера
	CreateModel(userId uint32, provider create.ProviderType, modelData *create.UniversalModelData, fileIDs []create.Ids) (create.UMCR, error)

	// Методы для работы с библиотеками и документами Mistral
	// Один пользователь = одна библиотека
	UploadFileToProvider(userId uint32, fileName string, fileData []byte) (string, error)
	DeleteDocumentFromLibrary(userId uint32, documentID string) error
	AddFileToLibrary(userId uint32, fileID, fileName string) error
}

type GoogleManager interface {
	Inter // Встраиваем базовый интерфейс
	// CreateModel создаёт новую модель у провайдера
	CreateModel(userId uint32, provider create.ProviderType, modelData *create.UniversalModelData, fileIDs []create.Ids) (create.UMCR, error)

	// Vector Embedding methods - работа со встроенным векторным хранилищем
	UploadDocumentWithEmbedding(userId uint32, docName, content string, metadata create.DocumentMetadata) (string, error)
	SearchSimilarDocuments(userId uint32, query string, limit int) ([]create.VectorDocument, error)
	DeleteDocument(userId uint32, docID string) error
	ListUserDocuments(userId uint32) ([]create.VectorDocument, error)
}

// ActionHandler интерфейс для обработки функций ассистента
type ActionHandler interface {
	RunAction(ctx context.Context, functionName, arguments string, provider create.ProviderType, userId uint32) string
}

// MCPToolDefinition описание инструмента, полученное от MCP сервера (tools/list).
// inputSchema НЕ содержит user_id — он передаётся через X-Session-ID заголовок.
type MCPToolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"` // JSON Schema параметров без user_id
}

// MCPConfigProvider расширяет ActionHandler методами получения конфигурации от MCP-сервера.
// Реализуется UniversalActionHandler (pkg/model/action_handler.go).
// Используется в buildAgentConfiguration (pkg/model/openai/model.go) через type assertion:
//
//	if mcpProvider, ok := m.actionHandler.(model.MCPConfigProvider); ok { ... }
type MCPConfigProvider interface {
	ActionHandler
	// FetchToolsList вызывает MCP tools/list и возвращает function-инструменты для данного пользователя.
	// Нативные OpenAI инструменты (code_interpreter, web_search) добавляются отдельно.
	FetchToolsList(ctx context.Context, userId uint32, provider create.ProviderType) ([]MCPToolDefinition, error)
	// FetchSystemPrompt вызывает MCP prompts/get?name=system и возвращает prompt hint.
	// Вызывающий код сам добавляет modelData.Prompt перед ним.
	FetchSystemPrompt(ctx context.Context, userId uint32, provider create.ProviderType) (string, error)
}

// RealtimeEvent — событие голосовой сессии OpenAI Realtime API.
// Передаётся из pump-горутин в WebSocket-хендлер клиента.
// Type: "audio_delta" | "transcript_delta" | "input_transcript_done" |
//
//	"response_done" | "function_result" | "error"
type RealtimeEvent struct {
	Type  string
	Text  string
	Data  []byte
	Err   error
	Files []File // файлы, накопленные за response-цикл — передаются клиенту в response_done
}

// RealtimeProvider опциональный интерфейс для голосовых сессий реального времени.
// Реализуется только OpenAIModel (Mistral и Google не поддерживают Realtime API).
type RealtimeProvider interface {
	// StartRealtimeSession создаёт WSS-соединение к OpenAI Realtime API.
	// RespModel с RealtimeEnabled=true должен существовать к моменту вызова.
	StartRealtimeSession(userId uint32, dialogID, respId uint64) error
	// CloseRealtimeSession завершает голосовую сессию respId.
	CloseRealtimeSession(respId uint64)
	// SendRealtimeAudio ставит PCM16-чанк в очередь отправки к OpenAI.
	SendRealtimeAudio(respId uint64, pcm16 []byte) error
	// SubscribeEvents регистрирует подписчика на управляющие события сессии.
	// Возвращает канал событий. Вызывается WebSocket-клиентом при подключении.
	// Telegram-звонок не подписывается — pumpFromOpenAI не блокируется при отсутствии подписчиков.
	SubscribeEvents(respId uint64) (<-chan RealtimeEvent, error)
	// UnsubscribeEvents удаляет подписчика и закрывает его канал.
	// Вызывается WebSocket-клиентом при отключении.
	UnsubscribeEvents(respId uint64, sub <-chan RealtimeEvent)
	// GetRealtimeAudio возвращает канал PCM16-дельт от ассистента для respId.
	GetRealtimeAudio(respId uint64) (<-chan []byte, error)
	// GetRealtimeDrain возвращает канал сигналов DrainPlayback (VAD speech_started) для respId.
	GetRealtimeDrain(respId uint64) (<-chan struct{}, error)
	// GetRealtimeGenerating возвращает указатель на флаг IsGenerating (true = OpenAI генерирует ответ).
	// Используется для аттенюации входящего аудио во время генерации (подавление эха).
	GetRealtimeGenerating(respId uint64) *atomic.Bool
	// SetRealtimeDisconnectCallback устанавливает callback вызываемый при критическом таймауте watchdog.
	// Используется для завершения звонка (Telegram) при том что модель совсем не отвечает.
	// callback получает respId сессии для очистки соответствующей callSession.
	SetRealtimeDisconnectCallback(respId uint64, callback func(respId uint64)) error
}

// ============================================================================
// HELPER ФУНКЦИИ ДЛЯ СОЗДАНИЯ РЕСПОНДЕНТОВ
// ============================================================================

// CreateBaseResponder создаёт базовые компоненты для респондента
// Используется всеми провайдерами для устранения дублирования кода
// Возвращает: context, cancel функцию, канал Ch и время TTL
func CreateBaseResponder(parentCtx context.Context, ttl time.Duration,
	assist Assistant, dialogID uint64, respName string) (context.Context, context.CancelFunc, *Ch, time.Time) {

	userCtx, cancel := context.WithCancel(parentCtx)

	ch := &Ch{
		TxCh:     make(chan Message, create.TxChanBuffer),
		RxCh:     make(chan Message, create.RxChanBuffer),
		UserID:   assist.UserId,
		DialogID: dialogID,
		RespName: respName,
	}

	ttlTime := time.Now().Add(ttl)

	return userCtx, cancel, ch, ttlTime
}

// NotifyWaitChannels уведомляет ожидающие горутины о создании респондента
// Используется всеми провайдерами для обработки waitChannels после создания респондента
func NotifyWaitChannels(waitChannels *sync.Map, respId uint64) {
	if waitChIface, exists := waitChannels.Load(respId); exists {
		waitCh := waitChIface.(chan struct{})
		close(waitCh)
		waitChannels.Delete(respId)
	}
}

// ============================================================================
// СТРУКТУРЫ ДАННЫХ
// ============================================================================

// Notifications структура для хранения настроек уведомлений о событиях
type Notifications struct {
	Start  bool
	End    bool
	Target bool
}

// Target структура для хранения целей модели
type Target struct {
	MetaAction string
	Triggers   []string
}

// Assistant информация об ассистенте
type Assistant struct {
	// Размещаем поля от большего к меньшему
	AssistId   string
	AssistName string
	Metas      Target
	Events     Notifications
	UserId     uint32
	Limit      uint32
	Provider   create.ProviderType // Тип провайдера модели (OpenAI, Mistral)
	Espero     uint8
	Ignore     bool
}

// RespModel универсальная структура респондента для всех провайдеров
// Провайдеро-специфичные данные хранятся во внутренних структурах (openai.RespModel, mistral.RespModel и т.д.)
// и конвертируются в эту структуру через методы convertToModelRespModel
type RespModel struct {
	Ctx      context.Context
	Cancel   context.CancelFunc
	Chan     map[uint64]*Ch // Map каналов для поддержки множественных DialogID
	TTL      time.Time
	Assist   Assistant
	RespName string
	Services Services // Для запуска только по одному экземпляру на респондента
}

// Services структура для отслеживания активных сервисов
type Services struct {
	Listener   *atomic.Bool
	Respondent *atomic.Bool
}

// Action действия для выполнения
type Action struct {
	SendFiles []File `json:"send_files,omitempty"` // Массив файлов для отправки
}

// FileType тип файла
type FileType string

const (
	Photo FileType = "photo"
	Video FileType = "video"
	Audio FileType = "audio"
	Doc   FileType = "doc"
)

// File информация о файле
type File struct {
	Type     FileType `json:"type,omitempty"`      // Тип файла
	URL      string   `json:"url,omitempty"`       // URL файла для загрузки
	FileName string   `json:"file_name,omitempty"` // Имя файла для сохранения
	Caption  string   `json:"caption,omitempty"`   // Подпись к файлу
}

// AssistResponse представляет ответ от AI-ассистента
type AssistResponse struct {
	Message  string `json:"message,omitempty"`  // Текстовое сообщение ответа
	Action   Action `json:"action,omitempty"`   // Действия для выполнения
	Meta     bool   `json:"target,omitempty"`   // Флаг достижения цели
	Operator bool   `json:"operator,omitempty"` // Флаг вызова оператора
}

// Ch канал для обмена сообщениями
type Ch struct {
	TxCh     chan Message
	RxCh     chan Message
	UserID   uint32
	DialogID uint64
	RespName string
	txClosed atomic.Bool // Флаг закрытия TxCh
	rxClosed atomic.Bool // Флаг закрытия RxCh
}

// IsTxOpen проверяет, открыт ли канал TxCh для записи
func (ch *Ch) IsTxOpen() bool {
	return !ch.txClosed.Load()
}

// IsRxOpen проверяет, открыт ли канал RxCh для записи
func (ch *Ch) IsRxOpen() bool {
	return !ch.rxClosed.Load()
}

// SendToTx безопасно отправляет сообщение в TxCh
func (ch *Ch) SendToTx(msg Message) error {
	if !ch.IsTxOpen() {
		return fmt.Errorf("канал TxCh закрыт для DialogID %d", ch.DialogID)
	}

	defer func() {
		if r := recover(); r != nil {
			//logger.Error("Паника при отправке в TxCh для DialogID %d: %v", ch.DialogID, r)
		}
	}()

	select {
	case ch.TxCh <- msg:
		return nil
	case <-time.After(1 * time.Second):
		return fmt.Errorf("таймаут отправки в TxCh для DialogID %d", ch.DialogID)
	}
}

// SendToRx безопасно отправляет сообщение в RxCh
func (ch *Ch) SendToRx(msg Message) error {
	if !ch.IsRxOpen() {
		return fmt.Errorf("канал RxCh закрыт для DialogID %d", ch.DialogID)
	}

	defer func() {
		if r := recover(); r != nil {
			//logger.Error("Паника при отправке в RxCh для DialogID %d: %v", ch.DialogID, r)
		}
	}()

	select {
	case ch.RxCh <- msg:
		return nil
	default:
		return fmt.Errorf("канал RxCh переполнен для DialogID %d", ch.DialogID)
	}
}

// Close безопасно закрывает каналы Ch
func (ch *Ch) Close() error {
	ch.CloseTx()
	ch.CloseRx()
	return nil
}

// CloseTx безопасно закрывает TxCh
func (ch *Ch) CloseTx() {
	// Проверяем, не закрыт ли уже канал
	if !ch.IsTxOpen() {
		return
	}
	ch.txClosed.Store(true)
	time.Sleep(10 * time.Millisecond)
	safeCloseMessage(ch.TxCh)
}

// CloseRx безопасно закрывает RxCh
func (ch *Ch) CloseRx() {
	// Проверяем, не закрыт ли уже канал
	if !ch.IsRxOpen() {
		return
	}
	ch.rxClosed.Store(true)
	time.Sleep(10 * time.Millisecond)
	safeCloseMessage(ch.RxCh)
}

// safeCloseMessage закрывает канал и обрабатывает панику
func safeCloseMessage(ch chan Message) {
	defer func() {
		if r := recover(); r != nil {
			//logger.Error("Паника при закрытии канала: %v", r)
		}
	}()
	close(ch)
}

// StartCh структура для передачи данных для запуска слушателя
type StartCh struct {
	Ctx      context.Context
	Provider string // "telegram", "whatsapp", "instagram" - для логирования
	Model    *RespModel
	Chanel   *Ch
	TreadId  uint64
	RespId   uint64
}

// Operator информация об операторе
type Operator struct {
	SenderName  string // Имя отправителя
	SetOperator bool   // В вопросе от модели если нужен оператор
	Operator    bool   // true, если ответ от оператора, false - от модели
}

// Message представляет сообщение в системе
type Message struct {
	Operator  Operator
	Type      string
	Content   AssistResponse
	Name      string
	Timestamp time.Time
	Files     []FileUpload `json:"files,omitempty"`
}

// FileUpload представляет файл для отправки для code interpreter
type FileUpload struct {
	Name     string    `json:"name"`
	Content  io.Reader `json:"-"`
	MimeType string    `json:"mime_type"`
	URL      string    `json:"url,omitempty"` // Опциональный URL для изображений (вместо Content)
}

// IsImageMimeType проверяет, является ли MIME-тип изображением
func (f *FileUpload) IsImageMimeType() bool {
	switch f.MimeType {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/jpg":
		return true
	default:
		return false
	}
}

// HasURL проверяет, содержит ли FileUpload валидный URL
func (f *FileUpload) HasURL() bool {
	return f.URL != "" && (strings.HasPrefix(f.URL, "http://") || strings.HasPrefix(f.URL, "https://"))
}

// ============================================================================
// MODEL ROUTER
// ============================================================================

// ModelRouter маршрутизирует запросы к разным моделям на основе Provider
type ModelRouter struct {
	openai        Inter
	mistral       Inter
	google        Inter
	modelsManager *create.UniversalModel // Менеджер для создания/удаления моделей
	ctx           context.Context
	conf          *conf.Conf
	db            DB
	landingPort   string // Порт landing сервера для GetRealUserID
}

// RouterOption определяет опцию для настройки ModelRouter
// Каждая опция создаёт UniversalActionHandler самостоятельно
type RouterOption func(*ModelRouter, context.Context, *conf.Conf, DB) error

// NewModelRouter создаёт новый маршрутизатор моделей с опциями
// Примеры использования:
//
//	// OpenAI + Mistral:
//	router, err := create.NewModelRouter(ctx, conf, db,
//	    openai.NewAsRouterOption(),
//	    mistral.NewAsRouterOption())
//
//	// Только OpenAI:
//	router, err := create.NewModelRouter(ctx, conf, db,
//	    openai.NewAsRouterOption())
//
//	// Только Mistral:
//	router, err := create.NewModelRouter(ctx, conf, db,
//	    mistral.NewAsRouterOption())
func NewModelRouter(ctx context.Context, conf *conf.Conf, db DB, options ...RouterOption) *ModelRouter {
	router := &ModelRouter{
		ctx:         ctx,
		conf:        conf,
		db:          db,
		landingPort: conf.WEB.Land, // Инициализируем landingPort
	}

	// Инициализируем менеджер моделей ДО применения опций
	// чтобы модели могли использовать GetRealUserID через router
	if managerDB, ok := db.(create.DB); ok {
		router.modelsManager = create.New(ctx, managerDB, conf)
	} else {
		log.Fatalf("DB не реализует create.DB, невозможна инициализация ModelRouter")
	}

	// Применяем опции (каждая опция создаёт свой UniversalActionHandler)
	for _, option := range options {
		if err := option(router, ctx, conf, db); err != nil {
			log.Fatalf("ошибка применения опции: %v", err)
		}
	}

	// Устанавливаем UniversalModel в Google модель для доступа к GetRealUserID
	if router.google != nil {
		// Используем type assertion для доступа к SetUniversalModel
		if googleModel, ok := router.google.(interface{ SetUniversalModel(*create.UniversalModel) }); ok {
			if router.modelsManager == nil {
				log.Fatal("КРИТИЧЕСКАЯ ОШИБКА: modelsManager == nil, не можем установить UniversalModel!")
			} else {
				googleModel.SetUniversalModel(router.modelsManager)
			}
		} else {
			log.Fatal("КРИТИЧЕСКАЯ ОШИБКА: Google модель не реализует метод SetUniversalModel!")
		}
		//} else {
		//	logger.Debug("Google модель не инициализирована, пропускаем установку UniversalModel")
	}

	// Проверяем, что хотя бы один провайдер инициализирован
	if router.openai == nil && router.mistral == nil && router.google == nil {
		log.Fatal("не инициализирован ни один провайдер моделей (используйте openai.NewAsRouterOption(), mistral.NewAsRouterOption() или google.NewAsRouterOption())")
	}

	return router
}

// WithOpenAIModel добавляет готовую реализацию OpenAI модели
// Используется пакетом openai для регистрации провайдера
func WithOpenAIModel(model Inter) RouterOption {
	return func(r *ModelRouter, ctx context.Context, conf *conf.Conf, db DB) error {
		if model == nil {
			return fmt.Errorf("OpenAI модель не может быть nil")
		}
		r.openai = model
		return nil
	}
}

// WithMistralModel добавляет готовую реализацию Mistral модели
// Используется пакетом mistral для регистрации провайдера
func WithMistralModel(model Inter) RouterOption {
	return func(r *ModelRouter, ctx context.Context, conf *conf.Conf, db DB) error {
		if model == nil {
			return fmt.Errorf("Mistral модель не может быть nil")
		}
		r.mistral = model
		return nil
	}
}

// WithGoogleModel добавляет готовую реализацию Google модели
// Используется пакетом google для регистрации провайдера
func WithGoogleModel(model Inter) RouterOption {
	return func(r *ModelRouter, ctx context.Context, conf *conf.Conf, db DB) error {
		if model == nil {
			return fmt.Errorf("Google модель не может быть nil")
		}
		r.google = model
		return nil
	}
}

// HasOpenAI проверяет, инициализирован ли провайдер OpenAI
func (r *ModelRouter) HasOpenAI() bool {
	return r.openai != nil
}

// HasMistral проверяет, инициализирован ли провайдер Mistral
func (r *ModelRouter) HasMistral() bool {
	return r.mistral != nil
}

// HasGoogle проверяет, инициализирован ли провайдер Google
func (r *ModelRouter) HasGoogle() bool { return r.google != nil }

// GetAvailableProviders возвращает список доступных провайдеров
func (r *ModelRouter) GetAvailableProviders() []string {
	providers := []string{}
	if r.openai != nil {
		providers = append(providers, "OpenAI")
	}
	if r.mistral != nil {
		providers = append(providers, "Mistral")
	}
	if r.google != nil {
		providers = append(providers, "Google")
	}
	return providers
}

// getModel возвращает модель по типу провайдера
func (r *ModelRouter) getModel(provider create.ProviderType) (Inter, error) {
	switch provider {
	case create.ProviderOpenAI:
		if r.openai == nil {
			return nil, fmt.Errorf("модель OpenAI не инициализирована")
		}
		return r.openai, nil
	case create.ProviderMistral:
		if r.mistral == nil {
			return nil, fmt.Errorf("модель Mistral не инициализирована")
		}
		return r.mistral, nil
	case create.ProviderGoogle:
		if r.google == nil {
			return nil, fmt.Errorf("модель Google не инициализирована")
		}
		return r.google, nil
	default:
		return nil, fmt.Errorf("неизвестный провайдер: %v", provider)
	}
}

// GetProviderModel возвращает модель конкретного провайдера (для тестирования)
func (r *ModelRouter) GetProviderModel(provider create.ProviderType) interface{} {
	switch provider {
	case create.ProviderOpenAI:
		return r.openai
	case create.ProviderMistral:
		return r.mistral
	case create.ProviderGoogle:
		return r.google
	default:
		return nil
	}
}

// NewMessage делегирует вызов к нужной модели
func (r *ModelRouter) NewMessage(operator Operator, msgType string, content *AssistResponse, name *string, files ...FileUpload) Message {
	// Используем OpenAI по умолчанию для создания сообщений
	if r.openai != nil {
		return r.openai.NewMessage(operator, msgType, content, name, files...)
	}
	if r.mistral != nil {
		return r.mistral.NewMessage(operator, msgType, content, name, files...)
	}
	if r.google != nil {
		return r.google.NewMessage(operator, msgType, content, name, files...)
	}
	// Fallback — создаём сообщение напрямую
	return Message{
		Operator:  operator,
		Type:      msgType,
		Content:   *content,
		Name:      *name,
		Timestamp: time.Now(),
		Files:     files,
	}
}

// GetFileAsReader делегирует к нужной модели
func (r *ModelRouter) GetFileAsReader(userId uint32, url string) (io.Reader, error) {
	manager, err := r.GetActiveUserManager(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения активного менеджера для userId %d: %w", userId, err)
	}

	return manager.GetFileAsReader(userId, url)
}

// GetOrSetRespGPT делегирует к модели на основе Provider из Assistant
func (r *ModelRouter) GetOrSetRespGPT(assist Assistant, dialogID, respId uint64, respName string) (*RespModel, error) {
	// Если провайдер не установлен - это ошибка, у пользователя нет созданной модели
	if assist.Provider == 0 {
		return nil, fmt.Errorf("провайдер не установлен для userId=%d: у пользователя не создана модель ассистента. "+
			"Создайте модель через API или панель управления", assist.UserId)
	}

	m, err := r.getModel(assist.Provider)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить модель для провайдера %s (userId=%d): %w", assist.Provider, assist.UserId, err)
	}
	return m.GetOrSetRespGPT(assist, dialogID, respId, respName)
}

// GetCh получает канал от любой модели (они хранятся в sync.Map)
func (r *ModelRouter) GetCh(respId uint64) (*Ch, error) {
	// Пробуем получить из OpenAI
	if r.openai != nil {
		ch, err := r.openai.GetCh(respId)
		if err == nil {
			return ch, nil
		}
	}
	// Затем из Mistral
	if r.mistral != nil {
		ch, err := r.mistral.GetCh(respId)
		if err == nil {
			return ch, nil
		}
	}
	// Затем Google
	if r.google != nil {
		ch, err := r.google.GetCh(respId)
		if err == nil {
			return ch, nil
		}
	}

	return nil, fmt.Errorf("канал не найден для respId %d", respId)
}

// GetRespIdBydialogID делегирует к обеим моделям
func (r *ModelRouter) GetRespIdBydialogID(dialogID uint64) (uint64, error) {
	// Пробуем OpenAI
	if r.openai != nil {
		id, err := r.openai.GetRespIdBydialogID(dialogID)
		if err == nil {
			return id, nil
		}
	}
	// Затем Mistral
	if r.mistral != nil {
		return r.mistral.GetRespIdBydialogID(dialogID)
	}
	// Затем Google
	if r.google != nil {
		return r.google.GetRespIdBydialogID(dialogID)
	}
	return 0, fmt.Errorf("RespId не найден для DialogID %d", dialogID)
}

// SaveAllContextDuringExit сохраняет контексты всех моделей
func (r *ModelRouter) SaveAllContextDuringExit() {
	if r.openai != nil {
		r.openai.SaveAllContextDuringExit()
	}
	if r.mistral != nil {
		r.mistral.SaveAllContextDuringExit()
	}
	if r.google != nil {
		r.google.SaveAllContextDuringExit()
	}
}

// Request направляет запрос к нужной модели на основе DialogID
func (r *ModelRouter) Request(userId uint32, dialogID uint64, text string, files ...FileUpload) (AssistResponse, error) {
	// Определяем провайдера по наличию респондента (БЕЗ запроса к БД!)
	if r.openai != nil {
		_, err := r.openai.GetRespIdBydialogID(dialogID)
		if err == nil {
			return r.openai.Request(userId, dialogID, text, files...)
		}
	}

	if r.mistral != nil {
		_, err := r.mistral.GetRespIdBydialogID(dialogID)
		if err == nil {
			return r.mistral.Request(userId, dialogID, text, files...)
		}
	}

	if r.google != nil {
		_, err := r.google.GetRespIdBydialogID(dialogID)
		if err == nil {
			return r.google.Request(userId, dialogID, text, files...)
		}
	}

	return AssistResponse{}, fmt.Errorf("модель не найдена для DialogID %d", dialogID)
}

// RequestStreaming направляет streaming запрос к нужной модели на основе DialogID
func (r *ModelRouter) RequestStreaming(userId uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...FileUpload) error {
	// Определяем провайдера по наличию респондента (БЕЗ запроса к БД!)
	if r.openai != nil {
		_, err := r.openai.GetRespIdBydialogID(dialogID)
		if err == nil {
			// Проверяем поддержку RequestStreaming через type assertion
			if streamer, ok := r.openai.(interface {
				RequestStreaming(userId uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...FileUpload) error
			}); ok {
				return streamer.RequestStreaming(userId, dialogID, text, onDelta, files...)
			}
			// Fallback на обычный Request с буферизацией
			response, err := r.openai.Request(userId, dialogID, text, files...)
			if err != nil {
				return err
			}
			// Сериализуем ответ и отправляем как один delta
			jsonData, _ := json.Marshal(response)
			if onDelta != nil {
				onDelta(string(jsonData), true)
			}
			return nil
		}
	}

	if r.mistral != nil {
		_, err := r.mistral.GetRespIdBydialogID(dialogID)
		if err == nil {
			// Проверяем поддержку RequestStreaming
			if streamer, ok := r.mistral.(interface {
				RequestStreaming(userId uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...FileUpload) error
			}); ok {
				return streamer.RequestStreaming(userId, dialogID, text, onDelta, files...)
			}
			// Fallback
			response, err := r.mistral.Request(userId, dialogID, text, files...)
			if err != nil {
				return err
			}
			jsonData, _ := json.Marshal(response)
			if onDelta != nil {
				onDelta(string(jsonData), true)
			}
			return nil
		}
	}

	if r.google != nil {
		_, err := r.google.GetRespIdBydialogID(dialogID)
		if err == nil {
			// Проверяем поддержку RequestStreaming
			if streamer, ok := r.google.(interface {
				RequestStreaming(userId uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...FileUpload) error
			}); ok {
				return streamer.RequestStreaming(userId, dialogID, text, onDelta, files...)
			}
			// Fallback
			response, err := r.google.Request(userId, dialogID, text, files...)
			if err != nil {
				return err
			}
			jsonData, _ := json.Marshal(response)
			if onDelta != nil {
				onDelta(string(jsonData), true)
			}
			return nil
		}
	}

	return fmt.Errorf("модель не найдена для DialogID %d", dialogID)
}

// CleanDialogData очищает данные диалога из нужной модели
func (r *ModelRouter) CleanDialogData(dialogID uint64) {
	if r.openai != nil {
		r.openai.CleanDialogData(dialogID)
	}
	if r.mistral != nil {
		r.mistral.CleanDialogData(dialogID)
	}
	if r.google != nil {
		r.google.CleanDialogData(dialogID)
	}
}

// GetActiveUserModel получает активную модель пользователя
func (r *ModelRouter) GetActiveUserModel(userId uint32) (*create.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetActiveUserModel(userId)
}

func (r *ModelRouter) GetActiveUserManager(userId uint32) (Inter, error) {
	provider, err := r.db.GetActiveProvider(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения активного провайдера для userId %d: %w", userId, err)
	}

	switch provider {
	case create.ProviderOpenAI:
		if r.openai == nil {
			return nil, fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		return r.openai.(OpenAIManager), nil
	case create.ProviderMistral:
		if r.mistral == nil {
			return nil, fmt.Errorf("Mistral провайдер не инициализирован")
		}
		return r.mistral.(MistralManager), nil
	case create.ProviderGoogle:
		if r.google == nil {
			return nil, fmt.Errorf("Google провайдер не инициализирован")
		}
		return r.google.(GoogleManager), nil
	default:
		return nil, fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// TranscribeAudio транскрибирует аудио
func (r *ModelRouter) TranscribeAudio(userId uint32, audioData []byte, fileName string) (string, error) {
	manager, err := r.GetActiveUserManager(userId)
	if err != nil {
		return "", fmt.Errorf("ошибка получения активного менеджера для userId %d: %w", userId, err)
	}

	return manager.TranscribeAudio(userId, audioData, fileName)
}

// GetRealtimeProvider возвращает RealtimeProvider если активная модель пользователя — OpenAI
// с включённым флагом Realtime. Второй bool = false если провайдер недоступен.
func (r *ModelRouter) GetRealtimeProvider(userId uint32) (RealtimeProvider, bool) {
	if r.openai == nil {
		return nil, false
	}
	activeManager, err := r.GetActiveUserManager(userId)
	if err != nil {
		return nil, false
	}
	rp, ok := activeManager.(RealtimeProvider)
	return rp, ok
}

// GetRealtimeGenerating реализует RealtimeProvider — делегирует в openai провайдер напрямую.
func (r *ModelRouter) GetRealtimeGenerating(respId uint64) *atomic.Bool {
	if r.openai == nil {
		return nil
	}
	rp, ok := r.openai.(RealtimeProvider)
	if !ok {
		return nil
	}
	return rp.GetRealtimeGenerating(respId)
}

// DisconnectRealtimeSession завершает голосовую сессию respId с вызовом зарегистрированного callback.
// Используется для универсального завершения сессии (API WebSocket + Telegram звонок).
// Вызывает OnDisconnect callback если он установлен в RealtimeSession.
func (r *ModelRouter) DisconnectRealtimeSession(respId uint64) {
	if r.openai == nil {
		return
	}
	rp, ok := r.openai.(RealtimeProvider)
	if !ok {
		return
	}
	rp.CloseRealtimeSession(respId)
}

// SetRealtimeDisconnectCallback устанавливает callback в RealtimeSession для уведомления о критическом таймауте.
func (r *ModelRouter) SetRealtimeDisconnectCallback(respId uint64, callback func(respId uint64)) error {
	if r.openai == nil {
		return fmt.Errorf("SetRealtimeDisconnectCallback: OpenAI провайдер не инициализирован")
	}
	rp, ok := r.openai.(RealtimeProvider)
	if !ok {
		return fmt.Errorf("SetRealtimeDisconnectCallback: OpenAI провайдер не реализует RealtimeProvider")
	}
	return rp.SetRealtimeDisconnectCallback(respId, callback)
}

// Shutdown завершает работу всех моделей
func (r *ModelRouter) Shutdown(shutCh chan<- com.LogMsg) {
	if r.openai != nil {
		r.openai.Shutdown(shutCh)
	}
	if r.mistral != nil {
		r.mistral.Shutdown(shutCh)
	}
	if r.google != nil {
		r.google.Shutdown(shutCh)
	}
}

// CleanUp запускает фоновую очистку устаревших записей для всех моделей
// Каждая модель запускает свой тикер для периодической очистки
func (r *ModelRouter) CleanUp() {
	if r.openai != nil {
		go r.openai.CleanUp()
	}
	if r.mistral != nil {
		go r.mistral.CleanUp()
	}
	if r.google != nil {
		go r.google.CleanUp()
	}
}

// CreateModel создаёт новую модель у указанного провайдера
// Использует modelsManager для создания модели
func (r *ModelRouter) CreateModel(userId uint32, provider create.ProviderType, modelData *create.UniversalModelData, fileIDs []create.Ids) (create.UMCR, error) {
	// Проверяем, что провайдер поддерживается
	_, err := r.getModel(provider)
	if err != nil {
		return create.UMCR{}, err
	}

	// Используем modelsManager для создания модели
	if r.modelsManager == nil {
		return create.UMCR{}, fmt.Errorf("модельный менеджер не инициализирован")
	}

	return r.modelsManager.CreateModel(userId, provider, modelData, fileIDs)
}

// UploadFileToProvider загружает файл в указанный провайдер
func (r *ModelRouter) UploadFileToProvider(userId uint32, provider create.ProviderType, fileName string, fileData []byte) (string, error) {
	switch provider {
	case create.ProviderOpenAI:
		return "", fmt.Errorf("OpenAI провайдер не поддерживает загрузку файлов")

	case create.ProviderMistral:
		if r.mistral == nil {
			return "", fmt.Errorf("Mistral провайдер не инициализирован")
		}
		if manager, ok := r.mistral.(MistralManager); ok {
			return manager.UploadFileToProvider(userId, fileName, fileData)
		}
		return "", fmt.Errorf("Mistral провайдер не поддерживает загрузку файлов")

	case create.ProviderGoogle:
		return "", fmt.Errorf("Google провайдер не поддерживает загрузку файлов")

	default:
		return "", fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// DeleteTempFile удаляет загруженный/созданный моделью временный файл
func (r *ModelRouter) DeleteTempFile(fileID string) error {
	// Временные файлы нужно удалять только из Mistarl
	if r.mistral == nil {
		return fmt.Errorf("OpenAI провайдер не инициализирован")
	}
	if manager, ok := r.mistral.(MistralManager); ok {
		return manager.DeleteTempFile(fileID)
	}
	return fmt.Errorf("OpenAI провайдер не поддерживает удаление загруженных файлов")

}

// DeleteFileFromProvider удаляет файл из указанного провайдера
func (r *ModelRouter) DeleteFileFromProvider(userId uint32, provider create.ProviderType, fileID string) error {
	switch provider {
	case create.ProviderOpenAI:
		return fmt.Errorf("OpenAI провайдер не поддерживает удаление файлов")

	case create.ProviderMistral:
		if r.mistral == nil {
			return fmt.Errorf("Mistral провайдер не инициализирован")
		}
		if manager, ok := r.mistral.(MistralManager); ok {
			return manager.DeleteDocumentFromLibrary(userId, fileID)
		}
		return fmt.Errorf("Mistral провайдер не поддерживает удаление файлов")

	case create.ProviderGoogle:
		return fmt.Errorf("Google провайдер не поддерживает удаление файлов")

	default:
		return fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// ===========================================================
// Специфичные методы для работы с файлами в векторных хранилищах
// ===========================================================

// AddFileFromFromProvider добавляет файл в хранилище указанного провайдера
func (r *ModelRouter) AddFileFromFromProvider(provider create.ProviderType, userId uint32, fileID, fileName string) error {
	switch provider {
	case create.ProviderOpenAI:
		return fmt.Errorf("OpenAI провайдер не поддерживает добавление файлов")

	case create.ProviderMistral:
		if r.mistral == nil {
			return fmt.Errorf("Mistral провайдер не инициализирован")
		}
		if manager, ok := r.mistral.(MistralManager); ok {
			return manager.AddFileToLibrary(userId, fileID, fileName)
		}
		return fmt.Errorf("Mistral провайдер не поддерживает добавление файлов")

	case create.ProviderGoogle:
		return fmt.Errorf("Google провайдер не поддерживает добавление файлов")

	default:
		return fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// ===========================================================
// Vector Embedding методы (Google + OpenAI)
// ===========================================================

// UploadDocumentWithEmbedding загружает документ с генерацией эмбеддинга
// Поддерживает Google и OpenAI провайдеры
func (r *ModelRouter) UploadDocumentWithEmbedding(userId uint32, provider, docName, content string, metadata create.DocumentMetadata) (string, error) {
	providerType, err := create.FromString(provider)
	if err != nil {
		return "", fmt.Errorf("неверный provider: %w", err)
	}

	switch providerType {
	case create.ProviderGoogle:
		if r.google == nil {
			return "", fmt.Errorf("Google провайдер не инициализирован")
		}
		if manager, ok := r.google.(GoogleManager); ok {
			return manager.UploadDocumentWithEmbedding(userId, docName, content, metadata)
		}
		return "", fmt.Errorf("Google провайдер не поддерживает загрузку документов с эмбеддингами")

	case create.ProviderOpenAI:
		if r.openai == nil {
			return "", fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.UploadDocumentWithEmbedding(userId, docName, content, metadata)
		}
		return "", fmt.Errorf("OpenAI провайдер не поддерживает загрузку документов с эмбеддингами")

	default:
		return "", fmt.Errorf("провайдер %s не поддерживает эмбеддинги", provider)
	}
}

// SearchSimilarDocuments ищет похожие документы в Vector Store
// Поддерживает Google и OpenAI провайдеры
func (r *ModelRouter) SearchSimilarDocuments(userId uint32, provider, query string, limit int) ([]create.VectorDocument, error) {
	providerType, err := create.FromString(provider)
	if err != nil {
		return nil, fmt.Errorf("неверный provider: %w", err)
	}

	switch providerType {
	case create.ProviderGoogle:
		if r.google == nil {
			return nil, fmt.Errorf("Google провайдер не инициализирован")
		}
		if manager, ok := r.google.(GoogleManager); ok {
			return manager.SearchSimilarDocuments(userId, query, limit)
		}
		return nil, fmt.Errorf("Google провайдер не поддерживает поиск документов")

	case create.ProviderOpenAI:
		if r.openai == nil {
			return nil, fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.SearchSimilarDocuments(userId, query, limit)
		}
		return nil, fmt.Errorf("OpenAI провайдер не поддерживает поиск документов")

	default:
		return nil, fmt.Errorf("провайдер %s не поддерживает эмбеддинги", provider)
	}
}

// DeleteDocument удаляет документ из Vector Store
// Поддерживает Google и OpenAI провайдеры
func (r *ModelRouter) DeleteDocument(userId uint32, provider, docID string) error {
	providerType, err := create.FromString(provider)
	if err != nil {
		return fmt.Errorf("неверный provider: %w", err)
	}

	switch providerType {
	case create.ProviderGoogle:
		if r.google == nil {
			return fmt.Errorf("Google провайдер не инициализирован")
		}
		if manager, ok := r.google.(GoogleManager); ok {
			return manager.DeleteDocument(userId, docID)
		}
		return fmt.Errorf("Google провайдер не поддерживает удаление документов")

	case create.ProviderOpenAI:
		if r.openai == nil {
			return fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.DeleteDocument(userId, docID)
		}
		return fmt.Errorf("OpenAI провайдер не поддерживает удаление документов")

	default:
		return fmt.Errorf("провайдер %s не поддерживает эмбеддинги", provider)
	}
}

// ListUserDocuments возвращает список документов пользователя
// Поддерживает Google и OpenAI провайдеры
// Если provider пустой, возвращает документы всех провайдеров
func (r *ModelRouter) ListUserDocuments(userId uint32, provider string) ([]create.VectorDocument, error) {
	// Если provider пустой - возвращаем документы всех провайдеров
	if provider == "" {
		var allDocs []create.VectorDocument

		// Пробуем получить документы Google
		if r.google != nil {
			if manager, ok := r.google.(GoogleManager); ok {
				docs, err := manager.ListUserDocuments(userId)
				if err == nil && docs != nil {
					allDocs = append(allDocs, docs...)
				}
			}
		}

		// Пробуем получить документы OpenAI
		if r.openai != nil {
			if manager, ok := r.openai.(OpenAIManager); ok {
				docs, err := manager.ListUserDocuments(userId)
				if err == nil && docs != nil {
					allDocs = append(allDocs, docs...)
				}
			}
		}

		return allDocs, nil
	}

	// Если provider указан - работаем только с ним
	providerType, err := create.FromString(provider)
	if err != nil {
		return nil, fmt.Errorf("неверный provider: %w", err)
	}

	switch providerType {
	case create.ProviderGoogle:
		if r.google == nil {
			return nil, fmt.Errorf("Google провайдер не инициализирован")
		}
		if manager, ok := r.google.(GoogleManager); ok {
			return manager.ListUserDocuments(userId)
		}
		return nil, fmt.Errorf("Google провайдер не поддерживает список документов")

	case create.ProviderOpenAI:
		if r.openai == nil {
			return nil, fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.ListUserDocuments(userId)
		}
		return nil, fmt.Errorf("OpenAI провайдер не поддерживает список документов")

	default:
		return nil, fmt.Errorf("провайдер %s не поддерживает эмбеддинги", provider)
	}
}

// ===========================================================
// Управление моделями
// ===========================================================

// SaveModel сохраняет модель в БД в универсальном формате
func (r *ModelRouter) SaveModel(userId uint32, umcr create.UMCR, data *create.UniversalModelData) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.SaveModel(userId, umcr, data)
}

// ReadModel читает модель пользователя по провайдеру
func (r *ModelRouter) ReadModel(userId uint32, provider *create.ProviderType) (*create.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.ReadModel(userId, provider)
}

// GetAllModelAsJSON получает модель в виде JSON
func (r *ModelRouter) GetAllModelAsJSON(userId uint32) ([]byte, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetModelAsJSON(userId)
}

// DeleteModel удаляет модель пользователя
func (r *ModelRouter) DeleteModel(userId uint32, provider create.ProviderType, deleteFiles bool, progressCallback func(string)) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.DeleteModel(userId, provider, deleteFiles, progressCallback)
}

// UpdateModelToDB обновляет модель в БД
func (r *ModelRouter) UpdateModelToDB(userId uint32, data *create.UniversalModelData) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.UpdateModelToDB(userId, data)
}

// UpdateModelEveryWhere обновляет модель везде (БД + провайдер)
func (r *ModelRouter) UpdateModelEveryWhere(userId uint32, data *create.UniversalModelData) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.UpdateModelEveryWhere(userId, data)
}

// GetUserModels получает все модели пользователя
func (r *ModelRouter) GetUserModels(userId uint32) ([]create.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetUserModels(userId)
}

// GetUserModelsResponse получает все модели пользователя в формате для API
func (r *ModelRouter) GetUserModelsResponse(userId uint32) (*create.UserModelsResponse, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetAllUserModelsResponse(userId)
}

// SetActiveUserModel переключает активную модель пользователя (в транзакции)
func (r *ModelRouter) SetActiveUserModel(userId uint32, provider create.ProviderType) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.SetActiveModelByProvider(userId, provider)
}

// GetUserModelByProvider получает модель пользователя по провайдеру
func (r *ModelRouter) GetUserModelByProvider(userId uint32, provider create.ProviderType) (*create.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetUserModelByProvider(userId, provider)
}

// GetRealUserID получает реальный userId через HTTP запрос к landing серверу
// Работает независимо от modelsManager (использует собственный landingPort)
func (r *ModelRouter) GetRealUserID(userId uint32) (uint64, error) {
	// Если есть modelsManager, используем его (для совместимости)
	if r.modelsManager != nil {
		return r.modelsManager.GetRealUserID(userId)
	}

	// Fallback: делаем HTTP запрос самостоятельно
	var url string
	if mode.ProductionMode {
		url = fmt.Sprintf("http://localhost:%s/uid?uid=%d", r.landingPort, userId)
	} else {
		url = fmt.Sprintf("https://localhost:%s/uid?uid=%d", r.landingPort, userId)
	}

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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("неожиданный статус ответа GetRealUserID: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("ошибка чтения ответа GetRealUserID: %v", err)
	}

	var userID uint64
	if err := json.Unmarshal(body, &userID); err != nil {
		return 0, fmt.Errorf("ошибка парсинга JSON ответа GetRealUserID: %v", err)
	}

	return userID, nil
}

// InvalidateUserAgentConfigCache инвалидирует кэш конфигурации модели для пользователя
// Вызывается при обновлении модели чтобы новые сессии получили актуальные настройки
// Работает со всеми провайдерами (OpenAI, Mistral, Google)
func (mr *ModelRouter) InvalidateUserAgentConfigCache(userId uint32) {
	if mr.openai != nil {
		mr.openai.InvalidateUserAgentConfigCache(userId)
	}
	if mr.mistral != nil {
		mr.mistral.InvalidateUserAgentConfigCache(userId)
	}
	if mr.google != nil {
		mr.google.InvalidateUserAgentConfigCache(userId)
	}
	//logger.Debug("Инвалидирован кэш конфигурации модели для userId=%d во всех провайдерах", userId)
}
