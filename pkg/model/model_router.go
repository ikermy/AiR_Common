package model

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
	models "github.com/ikermy/AiR_Common/pkg/model/create"
)

// ModelRouter маршрутизирует запросы к разным моделям на основе Provider
type ModelRouter struct {
	openai        Model
	mistral       Model
	modelsManager *models.Models // Менеджер для создания/удаления моделей
	ctx           context.Context
	conf          *conf.Conf
	db            DB
}

// RouterOption определяет опцию для настройки ModelRouter
// Каждая опция создаёт UniversalActionHandler самостоятельно
type RouterOption func(*ModelRouter, context.Context, *conf.Conf, DB) error

// NewModelRouter создаёт новый маршрутизатор моделей с опциями
// Примеры использования:
//
//	// OpenAI + Mistral:
//	router, err := model.NewModelRouter(ctx, conf, db,
//	    openai.NewAsRouterOption(),
//	    mistral.NewAsRouterOption())
//
//	// Только OpenAI:
//	router, err := model.NewModelRouter(ctx, conf, db,
//	    openai.NewAsRouterOption())
//
//	// Только Mistral:
//	router, err := model.NewModelRouter(ctx, conf, db,
//	    mistral.NewAsRouterOption())
func NewModelRouter(ctx context.Context, conf *conf.Conf, db DB, options ...RouterOption) *ModelRouter {
	router := &ModelRouter{
		ctx:  ctx,
		conf: conf,
		db:   db,
	}

	// Применяем опции (каждая опция создаёт свой UniversalActionHandler)
	for _, option := range options {
		if err := option(router, ctx, conf, db); err != nil {
			logger.Fatalf("ошибка применения опции: %w", err)
		}
	}

	// Проверяем, что хотя бы один провайдер инициализирован
	if router.openai == nil && router.mistral == nil {
		logger.Fatalf("не инициализирован ни один провайдер моделей (используйте openai.NewAsRouterOption() или mistral.NewAsRouterOption())")
	}

	// Инициализируем менеджер моделей
	if managerDB, ok := db.(models.DB); ok {
		// Получаем ключи из конфигурации
		openaiKey := ""
		mistralKey := ""
		if conf.GPT.OpenAIKey != "" {
			openaiKey = conf.GPT.OpenAIKey
		}
		if conf.GPT.MistralKey != "" {
			mistralKey = conf.GPT.MistralKey
		}

		router.modelsManager = models.New(ctx, managerDB, openaiKey, mistralKey)
	}

	return router
}

// WithOpenAIModel добавляет готовую реализацию OpenAI модели
// Используется пакетом openai для регистрации провайдера
func WithOpenAIModel(model Model) RouterOption {
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
func WithMistralModel(model Model) RouterOption {
	return func(r *ModelRouter, ctx context.Context, conf *conf.Conf, db DB) error {
		if model == nil {
			return fmt.Errorf("Mistral модель не может быть nil")
		}
		r.mistral = model
		return nil
	}
}

// CanTranscribeAudio проверяет, доступна ли транскрибация аудио
func (r *ModelRouter) CanTranscribeAudio() bool {
	return r.openai != nil
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

// GetAvailableProviders возвращает список доступных провайдеров
func (r *ModelRouter) GetAvailableProviders() []string {
	providers := []string{}
	if r.openai != nil {
		providers = append(providers, "OpenAI")
	}
	if r.mistral != nil {
		providers = append(providers, "Mistral")
	}
	return providers
}

// getModel возвращает модель по типу провайдера
func (r *ModelRouter) getModel(provider models.ProviderType) (Model, error) {
	switch provider {
	case models.ProviderOpenAI:
		if r.openai == nil {
			return nil, fmt.Errorf("модель OpenAI не инициализирована")
		}
		return r.openai, nil
	case models.ProviderMistral:
		if r.mistral == nil {
			return nil, fmt.Errorf("модель Mistral не инициализирована")
		}
		return r.mistral, nil
	default:
		return nil, fmt.Errorf("неизвестный провайдер: %v", provider)
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
func (r *ModelRouter) GetFileAsReader(url string) (io.Reader, error) {
	// Пробуем OpenAI первым
	if r.openai != nil {
		return r.openai.GetFileAsReader(url)
	}
	if r.mistral != nil {
		return r.mistral.GetFileAsReader(url)
	}
	return nil, fmt.Errorf("ни одна модель не инициализирована")
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
		return r.mistral.GetCh(respId)
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
}

// Request направляет запрос к нужной модели на основе dialogId
func (r *ModelRouter) Request(modelId string, dialogId uint64, text *string, files ...FileUpload) (AssistResponse, error) {
	// Нужно определить провайдера по dialogId
	var provider models.ProviderType
	if r.openai != nil {
		_, err := r.openai.GetRespIdByDialogId(dialogId)
		if err == nil {
			provider = models.ProviderOpenAI
		}
	}
	if provider == 0 && r.mistral != nil {
		_, err := r.mistral.GetRespIdByDialogId(dialogId)
		if err == nil {
			provider = models.ProviderMistral
			// Предупреждение: Mistral не поддерживает файлы
			if len(files) > 0 {
				return AssistResponse{}, fmt.Errorf("провайдер Mistral не поддерживает обработку файлов, используйте OpenAI для работы с файлами")
			}
		}
	}
	if provider == 0 {
		return AssistResponse{}, fmt.Errorf("не удалось определить провайдера для dialogId %d", dialogId)
	}
	m, err := r.getModel(provider)
	if err != nil {
		return AssistResponse{}, err
	}
	return m.Request(modelId, dialogId, text, files...)
}

// CleanDialogData очищает данные диалога из нужной модели
func (r *ModelRouter) CleanDialogData(dialogId uint64) {
	if r.openai != nil {
		r.openai.CleanDialogData(dialogId)
	}
	if r.mistral != nil {
		r.mistral.CleanDialogData(dialogId)
	}
}

// TranscribeAudio делегирует к OpenAI (Mistral не поддерживает)
// Возвращает ошибку, если OpenAI провайдер не инициализирован
func (r *ModelRouter) TranscribeAudio(audioData []byte, fileName string) (string, error) {
	if r.openai == nil {
		return "", fmt.Errorf("транскрибация аудио доступна только для OpenAI, но провайдер не инициализирован")
	}
	return r.openai.TranscribeAudio(audioData, fileName)
}

// Shutdown завершает работу всех моделей
func (r *ModelRouter) Shutdown() {
	if r.openai != nil {
		r.openai.Shutdown()
	}
	if r.mistral != nil {
		r.mistral.Shutdown()
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
}

// CreateModel создаёт новую модель у указанного провайдера
// Делегирует вызов к соответствующей модели на основе provider
// fileIDs должен быть типа []models.Ids из пакета pkg/model/create
func (r *ModelRouter) CreateModel(userId uint32, provider models.ProviderType, gptName string, gptId uint8, modelName string, modelJSON []byte, fileIDs interface{}) (string, error) {
	m, err := r.getModel(provider)
	if err != nil {
		return "", err
	}

	// Проверяем, что модель поддерживает создание (реализует ModelManager)
	if manager, ok := m.(ModelManager); ok {
		return manager.CreateModel(userId, provider, gptName, gptId, modelName, modelJSON, fileIDs)
	}

	return "", fmt.Errorf("провайдер %s не поддерживает создание моделей", provider)
}

// UploadFileToOpenAI загружает файл в OpenAI (только для OpenAI провайдера)
func (r *ModelRouter) UploadFileToOpenAI(fileName string, fileData []byte) (string, error) {
	if r.openai == nil {
		return "", fmt.Errorf("OpenAI провайдер не инициализирован")
	}

	if manager, ok := r.openai.(ModelManager); ok {
		return manager.UploadFileToOpenAI(fileName, fileData)
	}

	return "", fmt.Errorf("OpenAI провайдер не поддерживает загрузку файлов")
}

// DeleteFileFromOpenAI удаляет файл из OpenAI (только для OpenAI провайдера)
func (r *ModelRouter) DeleteFileFromOpenAI(fileID string) error {
	if r.openai == nil {
		return fmt.Errorf("OpenAI провайдер не инициализирован")
	}

	if manager, ok := r.openai.(ModelManager); ok {
		return manager.DeleteFileFromOpenAI(fileID)
	}

	return fmt.Errorf("OpenAI провайдер не поддерживает удаление файлов")
}

// AddFileFromOpenAI добавляет файл в векторное хранилище пользователя (только для OpenAI)
func (r *ModelRouter) AddFileFromOpenAI(userId uint32, fileID, fileName string) error {
	if r.openai == nil {
		return fmt.Errorf("OpenAI провайдер не инициализирован")
	}

	if manager, ok := r.openai.(ModelManager); ok {
		return manager.AddFileFromOpenAI(userId, fileID, fileName)
	}

	return fmt.Errorf("OpenAI провайдер не поддерживает добавление файлов")
}

// SaveModel сохраняет модель в БД в универсальном формате
func (r *ModelRouter) SaveModel(userId uint32, data *models.UniversalModelData) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.SaveModel(userId, data)
}

// ReadModel читает модель пользователя по провайдеру
func (r *ModelRouter) ReadModel(userId uint32, provider *models.ProviderType) (*models.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.ReadModel(userId, provider)
}

// GetModelAsJSON получает модель в виде JSON
func (r *ModelRouter) GetModelAsJSON(userId uint32, provider *models.ProviderType) ([]byte, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetModelAsJSON(userId, provider)
}

// DeleteModel удаляет модель пользователя
func (r *ModelRouter) DeleteModel(userId uint32, provider models.ProviderType, deleteFiles bool, progressCallback func(string)) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.DeleteModel(userId, provider, deleteFiles, progressCallback)
}

// UpdateModelToDB обновляет модель в БД
func (r *ModelRouter) UpdateModelToDB(userId uint32, data *models.UniversalModelData) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.UpdateModelToDB(userId, data)
}

// UpdateModelEveryWhere обновляет модель везде (БД + провайдер)
func (r *ModelRouter) UpdateModelEveryWhere(userId uint32, data *models.UniversalModelData, modelJSON []byte) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.UpdateModelEveryWhere(userId, data, modelJSON)
}

// GetUserModels получает все модели пользователя
func (r *ModelRouter) GetUserModels(userId uint32) ([]models.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetUserModels(userId)
}

// GetActiveUserModel получает активную модель пользователя
func (r *ModelRouter) GetActiveUserModel(userId uint32) (*models.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetActiveUserModel(userId)
}

// SetActiveUserModel переключает активную модель пользователя (в транзакции)
func (r *ModelRouter) SetActiveUserModel(userId uint32, modelId uint64) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.SetActiveModel(userId, modelId)
}

// GetUserModelByProvider получает модель пользователя по провайдеру
func (r *ModelRouter) GetUserModelByProvider(userId uint32, provider models.ProviderType) (*models.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetUserModelByProvider(userId, provider)
}
