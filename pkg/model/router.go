package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync/atomic"
	"time"

	"github.com/ikermy/AiR_Common/pkg/com"
	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// ============================================================================
// MODEL ROUTER
// ============================================================================

// Router маршрутизирует запросы к разным провайдерам моделей
type Router struct {
	openai        Inter
	mistral       Inter
	google        Inter
	modelsManager *create.UniversalModel
	ctx           context.Context
	conf          *conf.Conf
	db            DB
	landingPort   string
}

// RouterOption определяет опцию для настройки Router
type RouterOption func(*Router, context.Context, *conf.Conf, DB) error

// NewModelRouter создаёт новый маршрутизатор с опциями.
//
//	router, err := model.NewModelRouter(ctx, conf, db,
//	    openai.NewAsRouterOption(),
//	    mistral.NewAsRouterOption())
func NewModelRouter(ctx context.Context, conf *conf.Conf, db DB, options ...RouterOption) *Router {
	router := &Router{
		ctx:         ctx,
		conf:        conf,
		db:          db,
		landingPort: conf.WEB.Land,
	}

	if managerDB, ok := db.(create.DB); ok {
		router.modelsManager = create.New(ctx, managerDB, conf)
	} else {
		log.Fatalf("DB не реализует create.DB, невозможна инициализация ModelRouter")
	}

	for _, option := range options {
		if err := option(router, ctx, conf, db); err != nil {
			log.Fatalf("ошибка применения опции: %v", err)
		}
	}

	if router.google != nil {
		if googleModel, ok := router.google.(interface{ SetUniversalModel(*create.UniversalModel) }); ok {
			if router.modelsManager == nil {
				log.Fatal("КРИТИЧЕСКАЯ ОШИБКА: modelsManager == nil, не можем установить UniversalModel!")
			}
			googleModel.SetUniversalModel(router.modelsManager)
		} else {
			log.Fatal("КРИТИЧЕСКАЯ ОШИБКА: Google модель не реализует метод SetUniversalModel!")
		}
	}

	if router.openai == nil && router.mistral == nil && router.google == nil {
		log.Fatal("не инициализирован ни один провайдер моделей " +
			"(используйте openai.NewAsRouterOption(), mistral.NewAsRouterOption() или google.NewAsRouterOption())")
	}

	return router
}

// WithOpenAIModel добавляет реализацию OpenAI модели
func WithOpenAIModel(model Inter) RouterOption {
	return func(r *Router, _ context.Context, _ *conf.Conf, _ DB) error {
		if model == nil {
			return fmt.Errorf("OpenAI модель не может быть nil")
		}
		r.openai = model
		return nil
	}
}

// WithMistralModel добавляет реализацию Mistral модели
func WithMistralModel(model Inter) RouterOption {
	return func(r *Router, _ context.Context, _ *conf.Conf, _ DB) error {
		if model == nil {
			return fmt.Errorf("Mistral модель не может быть nil")
		}
		r.mistral = model
		return nil
	}
}

// WithGoogleModel добавляет реализацию Google модели
func WithGoogleModel(model Inter) RouterOption {
	return func(r *Router, _ context.Context, _ *conf.Conf, _ DB) error {
		if model == nil {
			return fmt.Errorf("Google модель не может быть nil")
		}
		r.google = model
		return nil
	}
}

// HasOpenAI проверяет, инициализирован ли провайдер OpenAI
func (r *Router) HasOpenAI() bool { return r.openai != nil }

// HasMistral проверяет, инициализирован ли провайдер Mistral
func (r *Router) HasMistral() bool { return r.mistral != nil }

// HasGoogle проверяет, инициализирован ли провайдер Google
func (r *Router) HasGoogle() bool { return r.google != nil }

// GetAvailableProviders возвращает список доступных провайдеров
func (r *Router) GetAvailableProviders() []string {
	providers := make([]string, 0, 3)
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

// forEachProvider вызывает fn для каждого инициализированного провайдера.
// Порядок: OpenAI → Mistral → Google.
func (r *Router) forEachProvider(fn func(Inter)) {
	for _, p := range []Inter{r.openai, r.mistral, r.google} {
		if p != nil {
			fn(p)
		}
	}
}

// getModel возвращает модель по типу провайдера
func (r *Router) getModel(provider create.ProviderType) (Inter, error) {
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
func (r *Router) GetProviderModel(provider create.ProviderType) interface{} {
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

// ============================================================================
// ДЕЛЕГИРУЮЩИЕ МЕТОДЫ
// ============================================================================

// NewMessage делегирует к первому доступному провайдеру
func (r *Router) NewMessage(operator Operator, msgType string, content *AssistResponse, name *string, files ...FileUpload) Message {
	if r.openai != nil {
		return r.openai.NewMessage(operator, msgType, content, name, files...)
	}
	if r.mistral != nil {
		return r.mistral.NewMessage(operator, msgType, content, name, files...)
	}
	if r.google != nil {
		return r.google.NewMessage(operator, msgType, content, name, files...)
	}
	// Fallback — только если ни один провайдер не инициализирован
	return Message{
		Operator:  operator,
		Type:      msgType,
		Content:   *content,
		Name:      *name,
		Timestamp: time.Now(),
		Files:     files,
	}
}

// GetFileAsReader делегирует к активному провайдеру пользователя
func (r *Router) GetFileAsReader(userID uint32, url string) (io.Reader, error) {
	manager, err := r.GetActiveUserManager(userID)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения активного менеджера для userID %d: %w", userID, err)
	}
	return manager.GetFileAsReader(userID, url)
}

// GetOrSetRespGPT делегирует к модели на основе Provider из Assistant
func (r *Router) GetOrSetRespGPT(assist Assistant, dialogID, respId uint64, respName string) (*RespModel, error) {
	if assist.Provider == 0 {
		return nil, fmt.Errorf("провайдер не установлен для userID=%d: у пользователя не создана модель ассистента. "+
			"Создайте модель через API или панель управления", assist.UserID)
	}
	m, err := r.getModel(assist.Provider)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить модель для провайдера %s (userID=%d): %w",
			assist.Provider, assist.UserID, err)
	}
	return m.GetOrSetRespGPT(assist, dialogID, respId, respName)
}

// GetCh ищет канал по respId во всех провайдерах
func (r *Router) GetCh(respId uint64) (*Ch, error) {
	for _, p := range []Inter{r.openai, r.mistral, r.google} {
		if p == nil {
			continue
		}
		if ch, err := p.GetCh(respId); err == nil {
			return ch, nil
		}
	}
	return nil, fmt.Errorf("канал не найден для respId %d", respId)
}

// GetRespIdBydialogID ищет respId по dialogID во всех провайдерах
func (r *Router) GetRespIdBydialogID(dialogID uint64) (uint64, error) {
	for _, p := range []Inter{r.openai, r.mistral, r.google} {
		if p == nil {
			continue
		}
		if id, err := p.GetRespIdBydialogID(dialogID); err == nil {
			return id, nil
		}
	}
	return 0, fmt.Errorf("RespId не найден для DialogID %d", dialogID)
}

// SaveAllContextDuringExit сохраняет контексты всех провайдеров
func (r *Router) SaveAllContextDuringExit() {
	r.forEachProvider(func(p Inter) { p.SaveAllContextDuringExit() })
}

// Request направляет запрос к провайдеру, которому принадлежит диалог
func (r *Router) Request(userID uint32, dialogID uint64, text string, files ...FileUpload) (AssistResponse, error) {
	for _, p := range []Inter{r.openai, r.mistral, r.google} {
		if p == nil {
			continue
		}
		if _, err := p.GetRespIdBydialogID(dialogID); err == nil {
			return p.Request(userID, dialogID, text, files...)
		}
	}
	return AssistResponse{}, fmt.Errorf("модель не найдена для DialogID %d", dialogID)
}

// tryProviderStreaming пытается выполнить streaming запрос к провайдеру.
// Возвращает (true, err) если провайдер найден, (false, nil) если нет.
func (r *Router) tryProviderStreaming(provider Inter, userID uint32, dialogID uint64, text string,
	onDelta func(delta string, done bool) error, files ...FileUpload) (bool, error) {
	if provider == nil {
		return false, nil
	}
	if _, err := provider.GetRespIdBydialogID(dialogID); err != nil {
		return false, nil
	}
	if streamer, ok := provider.(interface {
		RequestStreaming(userID uint32, dialogID uint64, text string,
			onDelta func(delta string, done bool) error, files ...FileUpload) error
	}); ok {
		return true, streamer.RequestStreaming(userID, dialogID, text, onDelta, files...)
	}
	// Fallback: буферизуем через Request
	response, err := provider.Request(userID, dialogID, text, files...)
	if err != nil {
		return true, err
	}
	jsonData, _ := json.Marshal(response)
	if onDelta != nil {
		if err := onDelta(string(jsonData), true); err != nil {
			return true, err
		}
	}
	return true, nil
}

// RequestStreaming направляет streaming запрос к провайдеру диалога
func (r *Router) RequestStreaming(userID uint32, dialogID uint64, text string,
	onDelta func(delta string, done bool) error, files ...FileUpload) error {
	for _, p := range []Inter{r.openai, r.mistral, r.google} {
		if found, err := r.tryProviderStreaming(p, userID, dialogID, text, onDelta, files...); found {
			return err
		}
	}
	return fmt.Errorf("модель не найдена для DialogID %d", dialogID)
}

// CleanDialogData очищает данные диалога у всех провайдеров
func (r *Router) CleanDialogData(dialogID uint64) {
	r.forEachProvider(func(p Inter) { p.CleanDialogData(dialogID) })
}

// GetActiveUserModel получает активную модель пользователя
func (r *Router) GetActiveUserModel(userID uint32) (*create.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetActiveUserModel(userID)
}

// GetActiveUserManager возвращает менеджера активного провайдера пользователя.
// Использует comma-ok form для безопасного type assertion.
func (r *Router) GetActiveUserManager(userID uint32) (Inter, error) {
	provider, err := r.db.GetActiveProvider(userID)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения активного провайдера для userID %d: %w", userID, err)
	}

	switch provider {
	case create.ProviderOpenAI:
		if r.openai == nil {
			return nil, fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		manager, ok := r.openai.(OpenAIManager)
		if !ok {
			return nil, fmt.Errorf("OpenAI провайдер не реализует OpenAIManager")
		}
		return manager, nil

	case create.ProviderMistral:
		if r.mistral == nil {
			return nil, fmt.Errorf("Mistral провайдер не инициализирован")
		}
		manager, ok := r.mistral.(MistralManager)
		if !ok {
			return nil, fmt.Errorf("Mistral провайдер не реализует MistralManager")
		}
		return manager, nil

	case create.ProviderGoogle:
		if r.google == nil {
			return nil, fmt.Errorf("Google провайдер не инициализирован")
		}
		manager, ok := r.google.(GoogleManager)
		if !ok {
			return nil, fmt.Errorf("Google провайдер не реализует GoogleManager")
		}
		return manager, nil

	default:
		return nil, fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// TranscribeAudio транскрибирует аудио через активный провайдер пользователя
func (r *Router) TranscribeAudio(userID uint32, audioData []byte, fileName string) (string, error) {
	manager, err := r.GetActiveUserManager(userID)
	if err != nil {
		return "", fmt.Errorf("ошибка получения активного менеджера для userID %d: %w", userID, err)
	}
	return manager.TranscribeAudio(userID, audioData, fileName)
}

// GetRealtimeProvider возвращает RealtimeProvider если активная модель поддерживает Realtime API
func (r *Router) GetRealtimeProvider(userID uint32) (RealtimeProvider, bool) {
	if r.openai == nil {
		return nil, false
	}
	activeManager, err := r.GetActiveUserManager(userID)
	if err != nil {
		return nil, false
	}
	rp, ok := activeManager.(RealtimeProvider)
	return rp, ok
}

// GetRealtimeGenerating возвращает указатель на флаг генерации Realtime-сессии
func (r *Router) GetRealtimeGenerating(respId uint64) *atomic.Bool {
	if r.openai == nil {
		return nil
	}
	rp, ok := r.openai.(RealtimeProvider)
	if !ok {
		return nil
	}
	return rp.GetRealtimeGenerating(respId)
}

// DisconnectRealtimeSession завершает голосовую сессию
func (r *Router) DisconnectRealtimeSession(respId uint64) {
	if r.openai == nil {
		return
	}
	rp, ok := r.openai.(RealtimeProvider)
	if !ok {
		return
	}
	rp.CloseRealtimeSession(respId)
}

// SetRealtimeDisconnectCallback устанавливает callback критического таймаута watchdog
func (r *Router) SetRealtimeDisconnectCallback(respId uint64, callback func(respId uint64)) error {
	if r.openai == nil {
		return fmt.Errorf("SetRealtimeDisconnectCallback: OpenAI провайдер не инициализирован")
	}
	rp, ok := r.openai.(RealtimeProvider)
	if !ok {
		return fmt.Errorf("SetRealtimeDisconnectCallback: OpenAI провайдер не реализует RealtimeProvider")
	}
	return rp.SetRealtimeDisconnectCallback(respId, callback)
}

// Shutdown завершает работу всех провайдеров
func (r *Router) Shutdown(shutCh chan<- com.LogMsg) {
	r.forEachProvider(func(p Inter) { p.Shutdown(shutCh) })
}

// CleanUp запускает фоновую очистку у всех провайдеров
func (r *Router) CleanUp() {
	r.forEachProvider(func(p Inter) { go p.CleanUp() })
}

// ============================================================================
// УПРАВЛЕНИЕ МОДЕЛЯМИ
// ============================================================================

// CreateModel создаёт новую модель у указанного провайдера
func (r *Router) CreateModel(userID uint32, provider create.ProviderType, modelData *create.UniversalModelData, fileIDs []create.Ids) (create.UMCR, error) {
	if _, err := r.getModel(provider); err != nil {
		return create.UMCR{}, err
	}
	if r.modelsManager == nil {
		return create.UMCR{}, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.CreateModel(userID, provider, modelData, fileIDs)
}

// UploadFileToProvider загружает файл в указанный провайдер (только Mistral)
func (r *Router) UploadFileToProvider(userID uint32, provider create.ProviderType, fileName string, fileData []byte) (string, error) {
	switch provider {
	case create.ProviderOpenAI:
		return "", fmt.Errorf("OpenAI провайдер не поддерживает загрузку файлов")
	case create.ProviderMistral:
		if r.mistral == nil {
			return "", fmt.Errorf("Mistral провайдер не инициализирован")
		}
		if manager, ok := r.mistral.(MistralManager); ok {
			return manager.UploadFileToProvider(userID, fileName, fileData)
		}
		return "", fmt.Errorf("Mistral провайдер не поддерживает загрузку файлов")
	case create.ProviderGoogle:
		return "", fmt.Errorf("Google провайдер не поддерживает загрузку файлов")
	default:
		return "", fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// DeleteTempFile удаляет загруженный временный файл через Mistral провайдер
func (r *Router) DeleteTempFile(fileID string) error {
	if r.mistral == nil {
		return fmt.Errorf("Mistral провайдер не инициализирован")
	}
	manager, ok := r.mistral.(MistralManager)
	if !ok {
		return fmt.Errorf("Mistral провайдер не поддерживает удаление временных файлов")
	}
	return manager.DeleteTempFile(fileID)
}

// DeleteFileFromProvider удаляет файл из указанного провайдера (только Mistral)
func (r *Router) DeleteFileFromProvider(userID uint32, provider create.ProviderType, fileID string) error {
	switch provider {
	case create.ProviderOpenAI:
		return fmt.Errorf("OpenAI провайдер не поддерживает удаление файлов")
	case create.ProviderMistral:
		if r.mistral == nil {
			return fmt.Errorf("Mistral провайдер не инициализирован")
		}
		if manager, ok := r.mistral.(MistralManager); ok {
			return manager.DeleteDocumentFromLibrary(userID, fileID)
		}
		return fmt.Errorf("Mistral провайдер не поддерживает удаление файлов")
	case create.ProviderGoogle:
		return fmt.Errorf("Google провайдер не поддерживает удаление файлов")
	default:
		return fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// AddFileFromFromProvider добавляет файл в хранилище провайдера (только Mistral)
func (r *Router) AddFileFromFromProvider(provider create.ProviderType, userID uint32, fileID, fileName string) error {
	switch provider {
	case create.ProviderOpenAI:
		return fmt.Errorf("OpenAI провайдер не поддерживает добавление файлов")
	case create.ProviderMistral:
		if r.mistral == nil {
			return fmt.Errorf("Mistral провайдер не инициализирован")
		}
		if manager, ok := r.mistral.(MistralManager); ok {
			return manager.AddFileToLibrary(userID, fileID, fileName)
		}
		return fmt.Errorf("Mistral провайдер не поддерживает добавление файлов")
	case create.ProviderGoogle:
		return fmt.Errorf("Google провайдер не поддерживает добавление файлов")
	default:
		return fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// ============================================================================
// VECTOR EMBEDDING МЕТОДЫ (OpenAI + Google)
// ============================================================================

// UploadDocumentWithEmbedding загружает документ с генерацией эмбеддинга
func (r *Router) UploadDocumentWithEmbedding(userID uint32, provider, docName, content string, metadata create.DocumentMetadata) (string, error) {
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
			return manager.UploadDocumentWithEmbedding(userID, docName, content, metadata)
		}
		return "", fmt.Errorf("Google провайдер не поддерживает загрузку документов с эмбеддингами")
	case create.ProviderOpenAI:
		if r.openai == nil {
			return "", fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.UploadDocumentWithEmbedding(userID, docName, content, metadata)
		}
		return "", fmt.Errorf("OpenAI провайдер не поддерживает загрузку документов с эмбеддингами")
	default:
		return "", fmt.Errorf("провайдер %s не поддерживает эмбеддинги", provider)
	}
}

// SearchSimilarDocuments ищет похожие документы в Vector Store
func (r *Router) SearchSimilarDocuments(userID uint32, provider, query string, limit int) ([]create.VectorDocument, error) {
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
			return manager.SearchSimilarDocuments(userID, query, limit)
		}
		return nil, fmt.Errorf("Google провайдер не поддерживает поиск документов")
	case create.ProviderOpenAI:
		if r.openai == nil {
			return nil, fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.SearchSimilarDocuments(userID, query, limit)
		}
		return nil, fmt.Errorf("OpenAI провайдер не поддерживает поиск документов")
	default:
		return nil, fmt.Errorf("провайдер %s не поддерживает эмбеддинги", provider)
	}
}

// DeleteDocument удаляет документ из Vector Store
func (r *Router) DeleteDocument(userID uint32, provider, docID string) error {
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
			return manager.DeleteDocument(userID, docID)
		}
		return fmt.Errorf("Google провайдер не поддерживает удаление документов")
	case create.ProviderOpenAI:
		if r.openai == nil {
			return fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.DeleteDocument(userID, docID)
		}
		return fmt.Errorf("OpenAI провайдер не поддерживает удаление документов")
	default:
		return fmt.Errorf("провайдер %s не поддерживает эмбеддинги", provider)
	}
}

// ListUserDocuments возвращает список документов пользователя.
// Если provider пустой — агрегирует документы всех провайдеров.
func (r *Router) ListUserDocuments(userID uint32, provider string) ([]create.VectorDocument, error) {
	if provider == "" {
		var allDocs []create.VectorDocument
		if r.google != nil {
			if manager, ok := r.google.(GoogleManager); ok {
				if docs, err := manager.ListUserDocuments(userID); err == nil && docs != nil {
					allDocs = append(allDocs, docs...)
				}
			}
		}
		if r.openai != nil {
			if manager, ok := r.openai.(OpenAIManager); ok {
				if docs, err := manager.ListUserDocuments(userID); err == nil && docs != nil {
					allDocs = append(allDocs, docs...)
				}
			}
		}
		return allDocs, nil
	}

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
			return manager.ListUserDocuments(userID)
		}
		return nil, fmt.Errorf("Google провайдер не поддерживает список документов")
	case create.ProviderOpenAI:
		if r.openai == nil {
			return nil, fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.ListUserDocuments(userID)
		}
		return nil, fmt.Errorf("OpenAI провайдер не поддерживает список документов")
	default:
		return nil, fmt.Errorf("провайдер %s не поддерживает эмбеддинги", provider)
	}
}

// ============================================================================
// ДЕЛЕГАТЫ К modelsManager
// ============================================================================

// SaveModel сохраняет модель в БД
func (r *Router) SaveModel(userID uint32, umcr create.UMCR, data *create.UniversalModelData) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.SaveModel(userID, umcr, data)
}

// ReadModel читает модель пользователя по провайдеру
func (r *Router) ReadModel(userID uint32, provider *create.ProviderType) (*create.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.ReadModel(userID, provider)
}

// GetAllModelAsJSON получает все модели пользователя в виде JSON
func (r *Router) GetAllModelAsJSON(userID uint32) ([]byte, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetModelAsJSON(userID)
}

// DeleteModel удаляет модель пользователя
func (r *Router) DeleteModel(userID uint32, provider create.ProviderType, deleteFiles bool, progressCallback func(string)) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.DeleteModel(userID, provider, deleteFiles, progressCallback)
}

// UpdateModelToDB обновляет модель в БД (без обновления у провайдера)
func (r *Router) UpdateModelToDB(userID uint32, data *create.UniversalModelData) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.UpdateModelToDB(userID, data)
}

// UpdateModelEveryWhere обновляет модель в БД и у провайдера
func (r *Router) UpdateModelEveryWhere(userID uint32, data *create.UniversalModelData) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.UpdateModelEveryWhere(userID, data)
}

// GetUserModels получает все модели пользователя
func (r *Router) GetUserModels(userID uint32) ([]create.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetUserModels(userID)
}

// GetUserModelsResponse получает все модели пользователя для API
func (r *Router) GetUserModelsResponse(userID uint32) (*create.UserModelsResponse, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetAllUserModelsResponse(userID)
}

// SetActiveUserModel переключает активную модель пользователя
func (r *Router) SetActiveUserModel(userID uint32, provider create.ProviderType) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.SetActiveModelByProvider(userID, provider)
}

// GetUserModelByProvider получает модель пользователя по провайдеру
func (r *Router) GetUserModelByProvider(userID uint32, provider create.ProviderType) (*create.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetUserModelByProvider(userID, provider)
}

// GetRealuserID получает реальный userID через modelsManager.
// Дублирующий fallback с прямым HTTP-запросом удалён — modelsManager всегда инициализирован.
func (r *Router) GetRealuserID(userID uint32) (uint64, error) {
	if r.modelsManager == nil {
		return 0, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetRealuserID(userID)
}

// InvalidateUserAgentConfigCache инвалидирует кэш конфигурации модели для пользователя
func (r *Router) InvalidateUserAgentConfigCache(userID uint32) {
	r.forEachProvider(func(p Inter) { p.InvalidateUserAgentConfigCache(userID) })
}
