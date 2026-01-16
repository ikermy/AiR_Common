package model

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model/create"
	"github.com/sashabaranov/go-openai"
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
	GetOrSetRespGPT(assist Assistant, dialogId, respId uint64, respName string) (*RespModel, error)
	GetCh(respId uint64) (*Ch, error)
	GetRespIdByDialogId(dialogId uint64) (uint64, error)
	SaveAllContextDuringExit()
	Request(userId uint32, modelId string, dialogId uint64, text string, files ...FileUpload) (AssistResponse, error)
	CleanDialogData(dialogId uint64)
	DeleteTempFile(fileID string) error
	TranscribeAudio(userId uint32, audioData []byte, fileName string) (string, error)
	CleanUp() // Фоновая очистка устаревших записей
	Shutdown()
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

	// Методы для работы с файлами OpenAI (специфичные для OpenAI)
	UploadFileFromVectorStorage(fileName string, fileData []byte) (string, error)
	DeleteFileFromVectorStorage(fileID string) error
	AddFileFromVectorStorage(userId uint32, fileID, fileName string) error
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
	RunAction(ctx context.Context, functionName, arguments string) string
	GetTools(provider create.ProviderType) interface{} // Возвращает инструменты для конкретного провайдера
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

// RespModel модель респондента
type RespModel struct {
	Ctx       context.Context
	Cancel    context.CancelFunc
	TreadsGPT map[uint64]*openai.Thread
	Chan      map[uint64]*Ch
	TTL       time.Time
	Assist    Assistant
	RespName  string
	Services  Services
	mu        sync.RWMutex
}

// Services структура для отслеживания активных сервисов
type Services struct {
	Listener   atomic.Bool
	Respondent atomic.Bool
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
	UserId   uint32
	DialogId uint64
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
		return fmt.Errorf("канал TxCh закрыт для dialogId %d", ch.DialogId)
	}

	defer func() {
		if r := recover(); r != nil {
			logger.Error("Паника при отправке в TxCh для dialogId %d: %v", ch.DialogId, r)
		}
	}()

	select {
	case ch.TxCh <- msg:
		return nil
	case <-time.After(1 * time.Second):
		return fmt.Errorf("таймаут отправки в TxCh для dialogId %d", ch.DialogId)
	}
}

// SendToRx безопасно отправляет сообщение в RxCh
func (ch *Ch) SendToRx(msg Message) error {
	if !ch.IsRxOpen() {
		logger.Warn("SendToRx: канал RxCh закрыт для dialogId %d, userId %d", ch.DialogId, ch.UserId)
		return fmt.Errorf("канал RxCh закрыт для dialogId %d", ch.DialogId)
	}

	defer func() {
		if r := recover(); r != nil {
			logger.Error("Паника при отправке в RxCh для dialogId %d: %v", ch.DialogId, r)
		}
	}()

	select {
	case ch.RxCh <- msg:
		return nil
	default:
		logger.Warn("SendToRx: канал RxCh переполнен для dialogId %d, userId %d", ch.DialogId, ch.UserId)
		return fmt.Errorf("канал RxCh переполнен для dialogId %d", ch.DialogId)
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
	ch.txClosed.Store(true)
	time.Sleep(10 * time.Millisecond)
	safeCloseMessage(ch.TxCh)
}

// CloseRx безопасно закрывает RxCh
func (ch *Ch) CloseRx() {
	ch.rxClosed.Store(true)
	time.Sleep(10 * time.Millisecond)
	safeCloseMessage(ch.RxCh)
}

// safeCloseMessage закрывает канал и обрабатывает панику
func safeCloseMessage(ch chan Message) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Паника при закрытии канала: %v", r)
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
		logger.Fatalf("DB не реализует create.DB, невозможна инициализация ModelRouter")
	}

	// Применяем опции (каждая опция создаёт свой UniversalActionHandler)
	for _, option := range options {
		if err := option(router, ctx, conf, db); err != nil {
			logger.Fatalf("ошибка применения опции: %w", err)
		}
	}

	// Проверяем, что хотя бы один провайдер инициализирован
	if router.openai == nil && router.mistral == nil && router.google == nil {
		logger.Fatalf("не инициализирован ни один провайдер моделей (используйте openai.NewAsRouterOption(), mistral.NewAsRouterOption() или google.NewAsRouterOption())")
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

// CanTranscribeAudio проверяет, доступна ли транскрибация аудио
func (r *ModelRouter) CanTranscribeAudio() bool {
	return r.openai != nil || r.google != nil || r.mistral != nil
}

// CanProcessFiles проверяет, доступна ли обработка файлов с векторным хранилищем
func (r *ModelRouter) CanProcessFiles() bool {
	return r.openai != nil
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
func (r *ModelRouter) GetOrSetRespGPT(assist Assistant, dialogId, respId uint64, respName string) (*RespModel, error) {
	// Если провайдер не установлен - это ошибка, у пользователя нет созданной модели
	if assist.Provider == 0 {
		return nil, fmt.Errorf("провайдер не установлен для userId=%d: у пользователя не создана модель ассистента. "+
			"Создайте модель через API или панель управления", assist.UserId)
	}

	m, err := r.getModel(assist.Provider)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить модель для провайдера %s (userId=%d): %w", assist.Provider, assist.UserId, err)
	}
	return m.GetOrSetRespGPT(assist, dialogId, respId, respName)
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

// GetRespIdByDialogId делегирует к обеим моделям
func (r *ModelRouter) GetRespIdByDialogId(dialogId uint64) (uint64, error) {
	// Пробуем OpenAI
	if r.openai != nil {
		id, err := r.openai.GetRespIdByDialogId(dialogId)
		if err == nil {
			return id, nil
		}
	}
	// Затем Mistral
	if r.mistral != nil {
		return r.mistral.GetRespIdByDialogId(dialogId)
	}
	// Затем Google
	if r.google != nil {
		return r.google.GetRespIdByDialogId(dialogId)
	}
	return 0, fmt.Errorf("RespId не найден для dialogId %d", dialogId)
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

// Request направляет запрос к нужной модели на основе dialogId
func (r *ModelRouter) Request(userId uint32, modelId string, dialogId uint64, text string, files ...FileUpload) (AssistResponse, error) {
	// Определяем провайдера по наличию респондента (БЕЗ запроса к БД!)
	if r.openai != nil {
		_, err := r.openai.GetRespIdByDialogId(dialogId)
		if err == nil {
			return r.openai.Request(userId, modelId, dialogId, text, files...)
		}
	}

	if r.mistral != nil {
		_, err := r.mistral.GetRespIdByDialogId(dialogId)
		if err == nil {
			return r.mistral.Request(userId, modelId, dialogId, text, files...)
		}
	}

	if r.google != nil {
		_, err := r.google.GetRespIdByDialogId(dialogId)
		if err == nil {
			return r.google.Request(userId, modelId, dialogId, text, files...)
		}
	}

	return AssistResponse{}, fmt.Errorf("модель не найдена для dialogId %d", dialogId)
}

// CleanDialogData очищает данные диалога из нужной модели
func (r *ModelRouter) CleanDialogData(dialogId uint64) {
	if r.openai != nil {
		r.openai.CleanDialogData(dialogId)
	}
	if r.mistral != nil {
		r.mistral.CleanDialogData(dialogId)
	}
	if r.google != nil {
		r.google.CleanDialogData(dialogId)
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

// Shutdown завершает работу всех моделей
func (r *ModelRouter) Shutdown() {
	if r.openai != nil {
		r.openai.Shutdown()
	}
	if r.mistral != nil {
		r.mistral.Shutdown()
	}
	if r.google != nil {
		r.google.Shutdown()
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
		if r.openai == nil {
			return "", fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.UploadFileFromVectorStorage(fileName, fileData) // userId не нужен для OpenAI привязывается к модели
		}
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
		if r.openai == nil {
			return fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.DeleteFileFromVectorStorage(fileID)
		}
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
		if r.openai == nil {
			return fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.AddFileFromVectorStorage(userId, fileID, fileName)
		}
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
// Google Vector Embedding методы
// ===========================================================

// UploadDocumentWithEmbedding загружает документ с генерацией эмбеддинга (только Google)
func (r *ModelRouter) UploadDocumentWithEmbedding(userId uint32, docName, content string, metadata create.DocumentMetadata) (string, error) {
	if r.google == nil {
		return "", fmt.Errorf("Google провайдер не инициализирован")
	}
	if manager, ok := r.google.(GoogleManager); ok {
		return manager.UploadDocumentWithEmbedding(userId, docName, content, metadata)
	}
	return "", fmt.Errorf("Google провайдер не поддерживает загрузку документов с эмбеддингами")
}

// SearchSimilarDocuments ищет похожие документы в Vector Store (только Google)
func (r *ModelRouter) SearchSimilarDocuments(userId uint32, query string, limit int) ([]create.VectorDocument, error) {
	if r.google == nil {
		return nil, fmt.Errorf("Google провайдер не инициализирован")
	}
	if manager, ok := r.google.(GoogleManager); ok {
		return manager.SearchSimilarDocuments(userId, query, limit)
	}
	return nil, fmt.Errorf("Google провайдер не поддерживает поиск документов")
}

// DeleteDocument удаляет документ из Vector Store (только Google)
func (r *ModelRouter) DeleteDocument(userId uint32, docID string) error {
	if r.google == nil {
		return fmt.Errorf("Google провайдер не инициализирован")
	}
	if manager, ok := r.google.(GoogleManager); ok {
		return manager.DeleteDocument(userId, docID)
	}
	return fmt.Errorf("Google провайдер не поддерживает удаление документов")
}

// ListUserDocuments возвращает список документов пользователя (только Google)
func (r *ModelRouter) ListUserDocuments(userId uint32) ([]create.VectorDocument, error) {
	if r.google == nil {
		return nil, fmt.Errorf("Google провайдер не инициализирован")
	}
	if manager, ok := r.google.(GoogleManager); ok {
		return manager.ListUserDocuments(userId)
	}
	return nil, fmt.Errorf("Google провайдер не поддерживает список документов")
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
