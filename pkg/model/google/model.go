package google

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

type Inter interface {
	UploadDocumentWithEmbedding(userId uint32, docName, content string, metadata create.DocumentMetadata) (string, error)
}

type DB = comdb.Exterior

// GoogleModel управляет Google Gemini моделями и респондентами
type GoogleModel struct {
	ctx           context.Context
	cancel        context.CancelFunc
	client        *create.GoogleAgentClient
	db            DB
	responders    sync.Map // respId -> *GoogleRespModel
	waitChannels  sync.Map
	dialogCache   sync.Map // dialogId -> *DialogCache (локальный кэш истории диалогов)
	UserModelTTl  time.Duration
	actionHandler model.ActionHandler
	shutdownOnce  sync.Once
}

// GoogleRespModel представляет респондента для Google Gemini
// В отличие от OpenAI, не хранит Thread (его нет в Gemini API)
// Вместо этого история диалога читается из БД через ReadDialog
type GoogleRespModel struct {
	Ctx      context.Context
	Cancel   context.CancelFunc
	Chan     *model.Ch // Канал для этого респондента
	TTL      time.Time
	Assist   model.Assistant
	RespName string
	Services Services
	// Кэш конфигурации агента для быстрого доступа
	AgentConfig *GoogleAgentConfig
}

// GoogleAgentConfig хранит конфигурацию агента для Google модели
// Примечание: Google модель хранит эмбеддинги в собственной БД (не в AllIds)
// AllIds для Google всегда nil/пуст, поэтому конфигурация создаётся на основе AssistId
type GoogleAgentConfig struct {
	ModelId           uint64                   `json:"model_id"` // ID модели в БД для связи с vector_embeddings
	ModelName         string                   `json:"model_name"`
	SystemInstruction map[string]interface{}   `json:"system_instruction"`
	GenerationConfig  map[string]interface{}   `json:"generation_config"`
	Tools             []map[string]interface{} `json:"tools"`
	VectorIds         []string                 `json:"vector_id,omitempty"`  // ID векторных хранилищ в Google Vector Store
	FileIds           []interface{}            `json:"file_ids,omitempty"`   // ID файлов в Google Vector Store
	HasVector         bool                     `json:"has_vector,omitempty"` // Флаг наличия Vector Store (управляется отдельно)

	// Дополнительные возможности Google модели
	Image     bool `json:"image"`      // Генерация изображений (Imagen 3)
	WebSearch bool `json:"web_search"` // Веб-поиск (google_search)
	Video     bool `json:"video"`      // Генерация видео (Google Veo)
	Haunter   bool `json:"haunter"`    // Модель используется для поиска лидов
	Search    bool `json:"search"`     // Поиск по векторному хранилищу (эмбеддингам в MariaDB)
}

// DialogCache кэширует историю диалога в памяти для быстрого доступа
type DialogCache struct {
	DialogId uint64
	Contents []GoogleContent // История диалога в формате Google Gemini
	ExpireAt time.Time       // Время истечения кэша (вычисляется как time.Now() + GoogleDialogLiveTimeout)
}

// GoogleContent представляет сообщение в формате Google Gemini
type GoogleContent struct {
	Role  string                   `json:"role"`  // "user" или "model"
	Parts []map[string]interface{} `json:"parts"` // Массив частей сообщения
}

type Services struct {
	Listener   atomic.Bool
	Respondent atomic.Bool
}

// New создаёт новый экземпляр GoogleModel
func New(parent context.Context, conf *conf.Conf, d DB, actionHandler model.ActionHandler) *GoogleModel {
	ctx, cancel := context.WithCancel(parent)

	// Создаём Google клиент с API ключом через конструктор
	googleClient := create.NewGoogleAgentClient(ctx, conf.GPT.GoogleKey)

	m := &GoogleModel{
		ctx:           ctx,
		cancel:        cancel,
		client:        googleClient,
		db:            d,
		UserModelTTl:  time.Duration(conf.GLOB.UserModelTTl) * time.Minute,
		actionHandler: actionHandler,
	}

	// Запускаем periodicFlush в фоновой горутине для очистки истекших диалогов из кэша
	go m.periodicFlush()

	return m
}

// NewAsRouterOption создаёт Google модель и возвращает её как опцию для ModelRouter
func NewAsRouterOption() model.RouterOption {
	return func(r *model.ModelRouter, ctx context.Context, cfg *conf.Conf, db model.DB) error {
		actionHandler := &model.UniversalActionHandler{}

		googleDB, ok := db.(DB)
		if !ok {
			return fmt.Errorf("DB не соответствует интерфейсу google.DB")
		}

		googleModel := New(ctx, cfg, googleDB, actionHandler)

		return model.WithGoogleModel(googleModel)(r, ctx, cfg, db)
	}
}

// SetClient устанавливает GoogleAgentClient (вызывается из universalModel)
func (m *GoogleModel) SetClient(client *create.GoogleAgentClient) {
	m.client = client
}

// NewMessage реализует интерфейс model.Inter
func (m *GoogleModel) NewMessage(operator model.Operator, msgType string, content *model.AssistResponse, name *string, files ...model.FileUpload) model.Message {
	msg := model.Message{
		Operator:  operator,
		Type:      msgType,
		Files:     files,
		Timestamp: time.Now(),
	}

	if content != nil {
		msg.Content = *content
	}

	if name != nil {
		msg.Name = *name
	}

	return msg
}

// GetOrCreateResponder получает или создаёт респондента для dialogId
func (m *GoogleModel) GetOrCreateResponder(dialogId uint64, userId uint32) (*GoogleRespModel, error) {
	// Создаём нового респондента
	// Примечание: сохранение в responders происходит в GetOrSetRespGPT с правильным ключом (respId)
	ctx, cancel := context.WithCancel(m.ctx)

	respModel := &GoogleRespModel{
		Ctx:      ctx,
		Cancel:   cancel,
		Chan:     nil, // Будет инициализирован в GetOrSetRespGPT
		TTL:      time.Now().Add(m.UserModelTTl),
		RespName: fmt.Sprintf("google-resp-%d", dialogId),
	}

	// Загружаем конфигурацию агента из БД
	if err := m.loadAgentConfig(userId, respModel); err != nil {
		return nil, fmt.Errorf("ошибка загрузки конфигурации агента: %w", err)
	}

	logger.Info("Создан новый Google респондент для dialogId %d", dialogId)

	return respModel, nil
}

// loadAgentConfig загружает конфигурацию агента для Google модели
// Пытается загрузить из AllIds, если пусто - создает конфигурацию по умолчанию
// Также проверяет наличие эмбеддингов в таблице vector_embeddings
func (m *GoogleModel) loadAgentConfig(userId uint32, respModel *GoogleRespModel) error {
	// Получаем все модели пользователя
	userModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	// Ищем активную модель Google
	var activeModel *create.UserModelRecord
	for i := range userModels {
		if userModels[i].IsActive && userModels[i].Provider == create.ProviderGoogle {
			activeModel = &userModels[i]
			break
		}
	}

	if activeModel == nil {
		return fmt.Errorf("активная Google модель не найдена для userId %d", userId)
	}

	// Инициализируем базовую конфигурацию
	agentConfig := GoogleAgentConfig{
		ModelId:   activeModel.ModelId,
		ModelName: activeModel.AssistId,
		HasVector: false,
	}

	// Загружаем полные данные модели из БД для получения всех параметров
	compressedData, _, err := m.db.ReadUserModelByProvider(userId, create.ProviderGoogle)
	if err != nil {
		logger.Warn("Ошибка чтения данных модели из БД: %v, используем конфигурацию по умолчанию", err, userId)
	} else if compressedData != nil {
		// Используем функцию из пакета db для распаковки и извлечения всех параметров
		_, _, _, image, webSearch, video, haunter, search, err := comdb.DecompressAndExtractMetadata(compressedData)
		if err != nil {
			logger.Warn("Ошибка распаковки параметров модели: %v", err, userId)
		} else {
			agentConfig.Image = image
			agentConfig.WebSearch = webSearch
			agentConfig.Video = video
			agentConfig.Haunter = haunter
			agentConfig.Search = search
		}
	}

	// Формируем массив Tools на основе загруженных параметров
	// ВАЖНО: WebSearch (google_search) добавляем только если он включен
	if agentConfig.WebSearch {
		agentConfig.Tools = append(agentConfig.Tools, map[string]interface{}{
			"google_search": map[string]interface{}{},
		})
	}

	// Пытаемся распарсить AllIds если он не пуст (для обратной совместимости)
	if len(activeModel.AllIds) > 0 {
		var tempConfig GoogleAgentConfig
		if err := json.Unmarshal(activeModel.AllIds, &tempConfig); err != nil {
			logger.Warn("Ошибка парсинга AllIds: %v", err, userId)
		} else {
			// Объединяем конфигурацию из AllIds с загруженной из БД
			if tempConfig.SystemInstruction != nil {
				agentConfig.SystemInstruction = tempConfig.SystemInstruction
			}
			if tempConfig.GenerationConfig != nil {
				agentConfig.GenerationConfig = tempConfig.GenerationConfig
			}
			// ВАЖНО: Tools из AllIds добавляем к существующим, а не заменяем
			if len(tempConfig.Tools) > 0 {
				agentConfig.Tools = append(agentConfig.Tools, tempConfig.Tools...)
			}
		}
	}

	// Проверяем наличие эмбеддингов в таблице vector_embeddings
	// Это важно для Google моделей, т.к. эмбеддинги хранятся в отдельной таблице
	// ВАЖНО: Загружаем эмбеддинги ТОЛЬКО если флаг Search включен
	if agentConfig.Search {
		embeddings, err := m.db.ListModelEmbeddings(activeModel.ModelId)
		if err != nil {
			logger.Warn("Ошибка получения эмбеддингов для modelId=%d: %v", activeModel.ModelId, err, userId)
		} else if len(embeddings) > 0 {
			agentConfig.HasVector = true
			logger.Info("Найдено %d эмбеддингов в vector_embeddings для modelId=%d", len(embeddings), activeModel.ModelId, userId)

			// Извлекаем уникальные doc_id как VectorIds
			vectorIdsMap := make(map[string]bool)
			for _, emb := range embeddings {
				vectorIdsMap[emb.ID] = true
			}
			agentConfig.VectorIds = make([]string, 0, len(vectorIdsMap))
			for id := range vectorIdsMap {
				agentConfig.VectorIds = append(agentConfig.VectorIds, id)
			}
		} else {
			logger.Info("Search включен для modelId=%d, но эмбеддинги отсутствуют", activeModel.ModelId, userId)
		}
	} else {
		logger.Info("Search отключен для modelId=%d, пропускаем загрузку эмбеддингов", activeModel.ModelId, userId)
	}

	respModel.AgentConfig = &agentConfig
	respModel.Assist.AssistId = activeModel.AssistId

	//logger.Debug("Загружена конфигурация Google агента: model=%s, tools=%d, hasVector=%v, vectorIds=%d, Image=%v, WebSearch=%v, Video=%v, Haunter=%v",
	//	agentConfig.ModelName, len(agentConfig.Tools), agentConfig.HasVector, len(agentConfig.VectorIds),
	//	agentConfig.Image, agentConfig.WebSearch, agentConfig.Video, agentConfig.Haunter)

	return nil
}

// CleanupExpiredResponders удаляет неактивных респондентов
func (m *GoogleModel) CleanupExpiredResponders() {
	m.responders.Range(func(key, value interface{}) bool {
		respId := key.(uint64)
		respModel := value.(*GoogleRespModel)

		if time.Now().After(respModel.TTL) {
			// Останавливаем контекст
			if respModel.Cancel != nil {
				respModel.Cancel()
			}

			m.responders.Delete(respId)
			logger.Info("Удален неактивный Google респондент для respId %d", respId)
		}

		return true
	})
}

// Shutdown корректно завершает работу модели
func (m *GoogleModel) Shutdown() {
	m.shutdownOnce.Do(func() {
		logger.Info("Начало shutdown для GoogleModel")

		// Останавливаем все респонденты
		m.responders.Range(func(key, value interface{}) bool {
			respModel := value.(*GoogleRespModel)
			if respModel.Cancel != nil {
				respModel.Cancel()
			}
			return true
		})

		// Отменяем главный контекст
		if m.cancel != nil {
			m.cancel()
		}

		logger.Info("GoogleModel shutdown завершен")
	})
}

// TranscribeAudio транскрибирует аудио в текст (обёртка для клиента)
func (m *GoogleModel) TranscribeAudio(userId uint32, audioData []byte, mimeType string) (string, error) {
	if m.client == nil {
		return "", fmt.Errorf("google клиент не инициализирован")
	}

	return m.client.TranscribeAudio(audioData, mimeType)
}

// GenerateVideo генерирует видео по описанию (обёртка для клиента)
func (m *GoogleModel) GenerateVideo(userId uint32, prompt string, aspectRatio string, duration int) ([]byte, string, error) {
	if m.client == nil {
		return nil, "", fmt.Errorf("google клиент не инициализирован")
	}

	return m.client.GenerateVideo(prompt, aspectRatio, duration)
}

// GetOrSetRespGPT получает или создаёт респондента (адаптер для совместимости с Inter)
func (m *GoogleModel) GetOrSetRespGPT(assist model.Assistant, dialogId, respId uint64, respName string) (*model.RespModel, error) {
	// Проверяем кэш по respId (как в OpenAI версии)
	if val, ok := m.responders.Load(respId); ok {
		respModel := val.(*GoogleRespModel)
		respModel.TTL = time.Now().Add(m.UserModelTTl) // Обновляем TTL
		respModel.Assist = assist
		respModel.RespName = respName
		// Конвертируем в model.RespModel
		return m.convertToModelRespModel(respModel), nil
	}

	// Google использует свою структуру GoogleRespModel
	// Для совместимости создаём адаптер
	googleResp, err := m.GetOrCreateResponder(dialogId, assist.UserId)
	if err != nil {
		return nil, err
	}

	// Инициализируем канал с правильной структурой
	googleResp.Chan = &model.Ch{
		TxCh:     make(chan model.Message, 1),
		RxCh:     make(chan model.Message, 1),
		UserId:   assist.UserId,
		DialogId: dialogId,
		RespName: respName,
	}

	googleResp.Assist = assist
	googleResp.RespName = respName

	// Сохраняем по respId (как в OpenAI версии)
	m.responders.Store(respId, googleResp)

	// Сигнализируем об готовности канала для ожидающих горутин
	if waitChIface, exists := m.waitChannels.Load(respId); exists {
		waitCh := waitChIface.(chan struct{})
		close(waitCh)
		m.waitChannels.Delete(respId)
	}

	// Конвертируем в model.RespModel
	return m.convertToModelRespModel(googleResp), nil
}

// GetCh получает канал по respId, ждёт его создания если необходимо
func (m *GoogleModel) GetCh(respId uint64) (*model.Ch, error) {
	waitChInterface, exists := m.waitChannels.Load(respId)
	var waitCh chan struct{}

	if !exists {
		waitCh = make(chan struct{})
		m.waitChannels.Store(respId, waitCh)
	} else {
		waitCh = waitChInterface.(chan struct{})
	}

	userCh, err := m.getTryCh(respId)
	if err == nil {
		return userCh, nil
	}

	select {
	case <-waitCh:
		return m.getTryCh(respId)
	case <-m.ctx.Done():
		return nil, fmt.Errorf("отменено контекстом ожидание канала для responderId %d", respId)
	case <-time.After(1 * time.Second):
		return nil, fmt.Errorf("тайм-аут при ожидании канала для responderId %d", respId)
	}
}

func (m *GoogleModel) getTryCh(respId uint64) (*model.Ch, error) {
	val, ok := m.responders.Load(respId)
	if !ok {
		return nil, fmt.Errorf("RespModel не найден для respId %d", respId)
	}

	respModel := val.(*GoogleRespModel)
	if respModel.Chan == nil {
		return nil, fmt.Errorf("канал не найден для respId %d", respId)
	}

	return respModel.Chan, nil
}

// GetRespIdByDialogId получает respId по dialogId
func (m *GoogleModel) GetRespIdByDialogId(dialogId uint64) (uint64, error) {
	// Ищем responder по dialogId в Chan
	var foundRespId uint64
	found := false

	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*GoogleRespModel)

		if respModel.Chan != nil && respModel.Chan.DialogId == dialogId {
			respId, ok := key.(uint64)
			if ok {
				foundRespId = respId
				found = true
				return false
			}
		}
		return true
	})

	if !found {
		return 0, fmt.Errorf("RespModel не найден для dialogId %d", dialogId)
	}

	return foundRespId, nil
}

// SaveAllContextDuringExit сохраняет все контексты при выходе
func (m *GoogleModel) SaveAllContextDuringExit() {
	// Google не использует SaveContext (история в БД через ReadDialog)
	// Поэтому этот метод пустой
}

// CleanDialogData очищает данные диалога
func (m *GoogleModel) CleanDialogData(dialogId uint64) {
	// Получаем respId по dialogId
	respId, err := m.GetRespIdByDialogId(dialogId)
	if err != nil {
		return
	}

	// Удаляем по respId
	if value, ok := m.responders.Load(respId); ok {
		respModel := value.(*GoogleRespModel)
		if respModel.Cancel != nil {
			respModel.Cancel()
		}
		m.responders.Delete(respId)
		logger.Info("Очищены данные диалога %d (respId: %d)", dialogId, respId)
	}
}

// CleanUp фоновая очистка устаревших респондентов
func (m *GoogleModel) CleanUp() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.CleanupExpiredResponders()
			m.cleanupExpiredWaitChannels()
		case <-m.ctx.Done():
			logger.Info("GoogleModel: CleanUp остановлен")
			return
		}
	}
}

// cleanupExpiredWaitChannels удаляет заблокированные waitChannels для несуществующих respId
func (m *GoogleModel) cleanupExpiredWaitChannels() {
	m.waitChannels.Range(func(key, value interface{}) bool {
		respId := key.(uint64)
		// Если респондента нет, это значит что waitCh никогда не будет закрыт
		// Удаляем такой waitCh чтобы не было утечек памяти
		if _, ok := m.responders.Load(respId); !ok {
			m.waitChannels.Delete(respId)
			logger.Debug("Удален заблокированный waitCh для respId %d", respId)
		}
		return true
	})
}

// convertToModelRespModel конвертирует GoogleRespModel в model.RespModel
// Создает map с одним каналом для совместимости с интерфейсом model.RespModel
func (m *GoogleModel) convertToModelRespModel(internal *GoogleRespModel) *model.RespModel {
	// Создаем map с одним каналом для совместимости
	chanMap := make(map[uint64]*model.Ch)
	if internal.Chan != nil {
		chanMap[internal.Chan.DialogId] = internal.Chan
	}

	return &model.RespModel{
		Ctx:      internal.Ctx,
		Cancel:   internal.Cancel,
		Chan:     chanMap,
		TTL:      internal.TTL,
		Assist:   internal.Assist,
		RespName: internal.RespName,
	}
}

// getOrCreateDialogCache получает или создаёт кэш диалога с обновлением ExpireAt
func (m *GoogleModel) getOrCreateDialogCache(dialogId uint64) *DialogCache {
	expireAt := time.Now().Add(create.GoogleDialogLiveTimeout)

	// Пытаемся получить существующий кэш
	if cacheIface, ok := m.dialogCache.Load(dialogId); ok {
		cache := cacheIface.(*DialogCache)
		cache.ExpireAt = expireAt // Обновляем время истечения
		return cache
	}

	// Создаём новый кэш
	cache := &DialogCache{
		DialogId: dialogId,
		Contents: []GoogleContent{},
		ExpireAt: expireAt,
	}

	m.dialogCache.Store(dialogId, cache)

	return cache
}

// addMessageToCache добавляет сообщение в кэш диалога с ограничением по количеству
// Если превышен лимит GoogleDialogHistoryLimit, удаляет старые сообщения
func (m *GoogleModel) addMessageToCache(dialogId uint64, content GoogleContent) {
	cache := m.getOrCreateDialogCache(dialogId)
	cache.Contents = append(cache.Contents, content)

	// Ограничиваем количество сообщений до GoogleDialogHistoryLimit
	maxMessages := int(create.GoogleDialogHistoryLimit)
	if len(cache.Contents) > maxMessages {
		// Удаляем старые сообщения, оставляя только последние maxMessages
		cache.Contents = cache.Contents[len(cache.Contents)-maxMessages:]
		//logger.Debug("Достигнут лимит сообщений в кэше диалога %d (%d), удалены старые сообщения",
		//	dialogId, maxMessages)
	}
}

// getDialogHistoryFromCache получает историю диалога из кэша
func (m *GoogleModel) getDialogHistoryFromCache(dialogId uint64) ([]GoogleContent, bool) {
	if cacheIface, ok := m.dialogCache.Load(dialogId); ok {
		cache := cacheIface.(*DialogCache)

		// Копируем содержимое для безопасности (поскольку Contents может быть изменён в другой горутине)
		contents := make([]GoogleContent, len(cache.Contents))
		copy(contents, cache.Contents)

		//logger.Debug("Получена история из кэша диалога %d, сообщений: %d", dialogId, len(contents))
		return contents, true
	}

	//logger.Debug("Кэш не найден для диалога %d", dialogId)
	return nil, false
}

// periodicFlush удаляет из кэша диалоги с истёкшим ExpireAt
func (m *GoogleModel) periodicFlush() {
	ticker := time.NewTicker(30 * time.Second) // Проверяем каждые 30 секунд
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			expiredCount := 0

			m.dialogCache.Range(func(key, value interface{}) bool {
				dialogId := key.(uint64)
				cache := value.(*DialogCache)

				if now.After(cache.ExpireAt) {
					m.dialogCache.Delete(dialogId)
					//logger.Debug("Удален кэш диалога %d из-за истечения ExpireAt", dialogId)
					expiredCount++
				}

				return true
			})

			if expiredCount > 0 {
				//logger.Debug("periodicFlush: удалено %d кэшей диалогов", expiredCount)
			}

		case <-m.ctx.Done():
			//logger.Debug("periodicFlush остановлен")
			return
		}
	}
}

// clearDialogCache очищает кэш конкретного диалога
func (m *GoogleModel) clearDialogCache(dialogId uint64) {
	m.dialogCache.Delete(dialogId)
	//logger.Debug("Очищен кэш диалога %d", dialogId)
}
