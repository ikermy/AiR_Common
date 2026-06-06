package google

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ikermy/AiR_Common/pkg/com"
	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

type Inter interface {
	UploadDocumentWithEmbedding(userID uint32, docName, content string, metadata create.DocumentMetadata) (string, error)
}

type DB = comdb.Exterior

// Model управляет Google Gemini моделями и респондентами
type Model struct {
	ctx              context.Context
	cancel           context.CancelFunc
	client           *create.GoogleAgentClient
	db               DB
	responders       sync.Map // respId -> *GoogleRespModel
	waitChannels     sync.Map
	dialogCache      sync.Map // dialogID -> *DialogCache (локальный кэш истории диалогов)
	embeddingCache   sync.Map // hash(text) -> *CachedEmbedding (кэш эмбеддингов для RAG)
	realtimeSessions sync.Map // respId -> *GoogleRealtimeSession (параллельные голосовые сессии)
	UserModelTTl     time.Duration
	actionHandler    model.ActionHandler
	universalModel   *create.UniversalModel // Для доступа к GetRealUserID
	shutdownOnce     sync.Once
}

// GoogleRespModel представляет респондента для Google Gemini
// В отличие от OpenAI, не хранит Thread (его нет в Gemini API)
// Вместо этого история диалога читается из БД через ReadDialog
type GoogleRespModel struct {
	Ctx      context.Context
	Cancel   context.CancelFunc
	Chan     *model.Ch            // Канал для этого респондента (основной, deprecated - используйте ChanMap)
	ChanMap  map[uint64]*model.Ch // Map каналов для поддержки множественных dialogID (унификация с OpenAI)
	TTL      time.Time
	Assist   model.Assistant
	RespName string
	Services Services
	// Кэш конфигурации агента для быстрого доступа
	AgentConfig *GoogleAgentConfig
}

// GetChannel реализует интерфейс model.ChannelProvider
func (r *GoogleRespModel) GetChannel() *model.Ch {
	return r.Chan
}

// GetChannelMap реализует интерфейс model.ChannelProvider
func (r *GoogleRespModel) GetChannelMap() map[uint64]*model.Ch {
	return r.ChanMap
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
	Image      bool   `json:"image"`       // Генерация изображений (Imagen 3)
	WebSearch  bool   `json:"web_search"`  // Веб-поиск (google_search)
	Video      bool   `json:"video"`       // Генерация видео (Google Veo)
	Haunter    bool   `json:"haunter"`     // Модель используется для поиска лидов
	VSearch    bool   `json:"search"`      // Поиск по векторному хранилищу (эмбеддингам в MariaDB)
	Operator   bool   `json:"operator"`    // Вызов оператора включён
	MetaAction string `json:"meta_action"` // Целевое действие модели

	// Флаги для Google Services
	S3          bool `json:"s3"`          // S3 хранилище
	Interpreter bool `json:"interpreter"` // Code Interpreter

	// Голосовой режим реального времени (Google Multimodal Live API)
	RealtimeEnabled bool                `json:"realtime_enabled"`       // Голосовой режим включён
	RealtimeModel   string              `json:"realtime_model"`         // Имя realtime-модели (gemini-2.0-flash-lite)
	RealtimeVAD     *create.RealtimeVAD `json:"realtime_vad,omitempty"` // Параметры VAD и голоса
}

// DialogCache кэширует историю диалога в памяти для быстрого доступа
type DialogCache struct {
	dialogID uint64
	Contents []GoogleContent // История диалога в формате Google Gemini
	ExpireAt time.Time       // Время истечения кэша (вычисляется как time.Now() + DialogLiveTimeout)
}

// CachedEmbedding кэширует результаты GenerateEmbedding для избежания повторных API вызовов
type CachedEmbedding struct {
	Embedding []float32 // Векторное представление текста (768 dimensions для gemini-embedding-001)
	ExpireAt  time.Time // Время истечения кэша (TTL 5 минут)
	Hash      string    // SHA256 hash текста (первые 16 символов для ключа)
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
func New(parent context.Context, d DB, actionHandler model.ActionHandler) *Model {
	ctx, cancel := context.WithCancel(parent)

	// Клиент не принимает глобальный ключ — персональный ключ читается из БД через keyResolver.
	googleClient := create.NewGoogleAgentClient(ctx)

	googleClient.SetKeyResolver(func(userID uint32) string {
		if key, err := d.GetUserAPIKey(userID, create.ProviderGoogle); err == nil {
			return key
		}
		return ""
	})
	if mcpProvider, ok := actionHandler.(model.MCPConfigProvider); ok {
		googleClient.SetMCPConfigFetchers(
			func(fetchCtx context.Context, userID uint32, provider create.ProviderType) (string, error) {
				return mcpProvider.FetchSystemPrompt(fetchCtx, userID, provider)
			},
			func(fetchCtx context.Context, userID uint32, provider create.ProviderType) ([]create.FunctionDeclaration, error) {
				mcpTools, err := mcpProvider.FetchToolsList(fetchCtx, userID, provider)
				if err != nil {
					return nil, err
				}
				functions := make([]create.FunctionDeclaration, 0, len(mcpTools))
				for _, t := range mcpTools {
					functions = append(functions, create.FunctionDeclaration{
						Name:        t.Name,
						Description: t.Description,
						Parameters:  t.InputSchema,
					})
				}
				return functions, nil
			},
		)
	}

	m := &Model{
		ctx:           ctx,
		cancel:        cancel,
		client:        googleClient,
		db:            d,
		UserModelTTl:  mode.UserModelTTl,
		actionHandler: actionHandler,
	}

	// Запускаем periodicFlush в фоновой горутине для очистки истекших диалогов из кэша
	go m.periodicFlush()

	return m
}

// NewAsRouterOption создаёт Google модель и возвращает её как опцию для ModelRouter
func NewAsRouterOption() model.RouterOption {
	return func(r *model.Router, ctx context.Context, db model.DB) error {
		googleDB, ok := db.(DB)
		if !ok {
			return fmt.Errorf("DB не соответствует интерфейсу google.DB")
		}

		actionHandler := model.NewUniversalActionHandler(ctx)

		googleModel := New(ctx, googleDB, actionHandler)

		return model.WithGoogleModel(googleModel)(r, ctx, db)
	}
}

// SetClient устанавливает GoogleAgentClient (вызывается из universalModel)
func (m *Model) SetClient(client *create.GoogleAgentClient) {
	m.client = client
}

// SetUniversalModel устанавливает UniversalModel для доступа к GetRealUserID
func (m *Model) SetUniversalModel(um *create.UniversalModel) {
	m.universalModel = um
	if m.client != nil {
		m.client.SetUniversalModel(um)
	}
}

// NewMessage реализует интерфейс model.Inter
func (m *Model) NewMessage(operator model.Operator, msgType string, content *model.AssistResponse, name *string, files ...model.FileUpload) model.Message {
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

// loadAgentConfig загружает конфигурацию агента для Google модели
// Пытается загрузить из AllIds, если пусто - создает конфигурацию по умолчанию
// Также проверяет наличие эмбеддингов в таблице vector_embeddings
func (m *Model) loadAgentConfig(userID uint32, respModel *GoogleRespModel) error {
	// Получаем API-ключ напрямую через DB: это обеспечивает правильную обработку $mk$-ключей —
	// если MasterKey недоступен (Landing не ответил / пользователь не входил), ошибка и уведомление
	// пропагируются явно, а не теряются внутри HasAPIKey.
	apiKey, err := m.db.GetUserAPIKey(userID, create.ProviderGoogle)
	if err != nil {
		return fmt.Errorf("ошибка получения Google API-ключа для пользователя %d: %w", userID, err)
	}
	if m.client == nil || apiKey == "" {
		return fmt.Errorf("Google API ключ не настроен для пользователя %d: добавьте персональный ключ через настройки", userID)
	}

	// Получаем все модели пользователя
	userModels, err := m.db.GetAllUserModels(userID)
	if err != nil {
		return fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	// Ищем активную модель Google
	var found *create.UserModelRecord
	for i := range userModels {
		if userModels[i].Provider == create.ProviderGoogle {
			found = &userModels[i]
			break
		}
	}

	if found == nil {
		return fmt.Errorf("модель Google не найдена для userID %d", userID)
	}

	// Инициализируем базовую конфигурацию.
	// RealtimeModel выставляется сразу в константу — НЕ в found.AssistId (это текстовая модель).
	// Это гарантирует что realtime-сессия не получит обычную модель (gemini-2.5-flash и т.п.),
	// которая не поддерживает v1alpha BidiGenerateContent.
	agentConfig := GoogleAgentConfig{
		ModelId:       found.ModelId,
		ModelName:     found.AssistId, // текстовая модель (напр. gemini-2.5-flash)
		HasVector:     false,
		RealtimeModel: create.RealtimeGoogleModel, // всегда realtime-модель (gemini-2.0-flash-lite)
	}

	// Загружаем полные данные модели из БД для получения всех параметров
	compressedData, _, err := m.db.ReadUserModelByProvider(userID, create.ProviderGoogle)
	if err != nil {
		//logger.Warn("Ошибка чтения данных модели из БД: %v, используем конфигурацию по умолчанию", err, userID)
	} else if compressedData != nil {
		// Распаковываем данные модели чтобы получить Prompt (SystemInstruction)
		if m.universalModel != nil {
			modelData, decompressErr := m.universalModel.DecompressModelData(compressedData, nil)
			if decompressErr != nil {
				//logger.Warn("Ошибка распаковки данных модели: %v", decompressErr, userID)
			} else {
				// SystemInstruction: базовый prompt + hint от MCP, если он доступен.
				promptText := modelData.Prompt
				if mcpProvider, ok := m.actionHandler.(model.MCPConfigProvider); ok {
					if hint, fetchErr := mcpProvider.FetchSystemPrompt(m.ctx, userID, create.ProviderGoogle); fetchErr == nil && hint != "" {
						promptText = modelData.Prompt + "\n\n" + hint
					}
				}
				if promptText != "" {
					agentConfig.SystemInstruction = map[string]interface{}{
						"parts": []map[string]interface{}{
							{
								"text": promptText,
							},
						},
					}
					//} else {
					//	logger.Warn("Prompt пустой в БД!", userID)
				}

				// Загружаем остальные параметры
				agentConfig.Image = modelData.Image
				agentConfig.WebSearch = modelData.WebSearch
				agentConfig.Video = modelData.Video
				agentConfig.Haunter = modelData.Haunter
				agentConfig.VSearch = modelData.Search
				agentConfig.Operator = modelData.Operator
				agentConfig.MetaAction = modelData.MetaAction
				agentConfig.S3 = modelData.S3
				agentConfig.Interpreter = modelData.Interpreter
				agentConfig.RealtimeEnabled = modelData.Realtime
				agentConfig.RealtimeVAD = modelData.RealtimeVAD
				// RealtimeModel: берём из RealtimeVAD.Google.VoiceName нет — это фиксированная модель.
				// Используем константу RealtimeGoogleModel, если в данных не переопределена.
				agentConfig.RealtimeModel = create.RealtimeGoogleModel
			}
		} else {
			return fmt.Errorf("UniversalModel не установлен, невозможно загрузить данные модели для пользователя %d", userID)
		}
	}

	// Формируем массив Tools на основе загруженных параметров
	// ВАЖНО: WebSearch (google_search) добавляем только если он включен
	if agentConfig.WebSearch {
		agentConfig.Tools = append(agentConfig.Tools, map[string]interface{}{
			"google_search": map[string]interface{}{},
		})
	}

	// Формируем function_declarations от MCP сервера.
	// Если MCP недоступен — function tools не добавляются (модель работает только с modelData.Prompt).
	var functionDeclarations []map[string]interface{}
	if mcpProvider, ok := m.actionHandler.(model.MCPConfigProvider); ok {
		if mcpTools, fetchErr := mcpProvider.FetchToolsList(m.ctx, userID, create.ProviderGoogle); fetchErr == nil {
			for _, t := range mcpTools {
				functionDeclarations = append(functionDeclarations, map[string]interface{}{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.InputSchema,
				})
			}
		}
	}

	// 4. Если есть function_declarations, добавляем их в Tools
	if len(functionDeclarations) > 0 {
		agentConfig.Tools = append(agentConfig.Tools, map[string]interface{}{
			"function_declarations": functionDeclarations,
		})
	}

	// 5. Code Interpreter (только если нет других function_declarations)
	// ВАЖНО: Google Gemini НЕ поддерживает одновременное использование
	// function_declarations и code_execution в одном запросе
	if agentConfig.Interpreter && len(functionDeclarations) == 0 {
		agentConfig.Tools = append(agentConfig.Tools, map[string]interface{}{
			"code_execution": map[string]interface{}{},
		})
	}

	// ПРИМЕЧАНИЕ: AllIds для Google модели всегда пустой (не используется)
	// Конфигурация Tools формируется динамически выше на основе флагов из БД

	// Проверяем наличие эмбеддингов в таблице vector_embeddings
	// Это важно для Google моделей, т.к. эмбеддинги хранятся в отдельной таблице
	// ВАЖНО: Загружаем эмбеддинги ТОЛЬКО если флаг VSearch включен
	if agentConfig.VSearch {
		embeddings, err := m.db.ListModelEmbeddings(found.ModelId, create.ProviderGoogle)
		if err != nil {
			//logger.Warn("Ошибка получения эмбеддингов для modelId=%d: %v", found.ModelId, err, userID)
		} else if len(embeddings) > 0 {
			agentConfig.HasVector = true
			//logger.Debug("Найдено %d эмбеддингов в vector_embeddings для modelId=%d", len(embeddings), found.ModelId, userID)

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
			//logger.Debug("VSearch включен для modelId=%d, но эмбеддинги отсутствуют", found.ModelId, userID)
		}
		//} else {
		//	logger.Debug("VSearch отключен для modelId=%d, пропускаем загрузку эмбеддингов", found.ModelId, userID)
	}

	respModel.AgentConfig = &agentConfig
	respModel.Assist.AssistId = found.AssistId

	// Логируем загруженную конфигурацию для отладки
	//logger.Debug("Загружена конфигурация Google агента: model=%s, tools=%d, WebSearch=%v, S3=%v, Calendar=%v, Sheets=%v, Interpreter=%v, VSearch=%v, hasVector=%v",
	//	agentConfig.ModelName, len(agentConfig.Tools), agentConfig.WebSearch, agentConfig.S3,
	//	agentConfig.Interpreter, agentConfig.VSearch, agentConfig.HasVector, userID)

	return nil
}

// CleanupExpiredResponders удаляет неактивных респондентов
func (m *Model) CleanupExpiredResponders() {
	m.responders.Range(func(key, value interface{}) bool {
		respId := key.(uint64)
		respModel := value.(*GoogleRespModel)

		if time.Now().After(respModel.TTL) {
			// Останавливаем контекст
			if respModel.Cancel != nil {
				respModel.Cancel()
			}

			m.responders.Delete(respId)
			//logger.Debug("Удален неактивный Google респондент для respId %d", respId)
		}

		return true
	})
}

// Shutdown корректно завершает работу модели
func (m *Model) Shutdown(shutCh chan<- com.LogMsg) {
	m.shutdownOnce.Do(func() {
		shutCh <- com.LogMsg{
			Msg: "начало shutdown",
			Mod: "GoogleModel",
			Log: 0, // 0 - Info
			UID: 0,
		}
	})

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

	shutCh <- com.LogMsg{
		Msg: "shutdown завершен",
		Mod: "GoogleModel",
		Log: 0, // 0 - Info
		UID: 0,
	}
}

// TranscribeAudio транскрибирует аудио в текст (обёртка для клиента)
func (m *Model) TranscribeAudio(_ uint32, audioData []byte, mimeType string) (string, error) {
	if m.client == nil {
		return "", fmt.Errorf("google клиент не инициализирован")
	}

	return m.client.TranscribeAudio(audioData, mimeType)
}

// GenerateVideo генерирует видео по описанию (обёртка для клиента)
func (m *Model) GenerateVideo(prompt string, aspectRatio string, duration int) ([]byte, string, error) {
	if m.client == nil {
		return nil, "", fmt.Errorf("google клиент не инициализирован")
	}

	return m.client.GenerateVideo(prompt, aspectRatio, duration)
}

// GetOrSetRespGPT получает или создаёт респондента (адаптер для совместимости с Inter)
func (m *Model) GetOrSetRespGPT(assist model.Assistant, dialogID, respId uint64, respName string) (*model.RespModel, error) {
	// Проверяем кэш по respId (как в OpenAI версии)
	if val, ok := m.responders.Load(respId); ok {
		respModel := val.(*GoogleRespModel)
		respModel.TTL = time.Now().Add(m.UserModelTTl) // Обновляем TTL
		respModel.Assist = assist
		respModel.RespName = respName

		// ВАЖНО: Проверяем наличие канала для данного dialogID
		if respModel.ChanMap == nil {
			respModel.ChanMap = make(map[uint64]*model.Ch)
		}

		// Если канал для этого dialogID не существует - создаем новый
		if _, exists := respModel.ChanMap[dialogID]; !exists {
			// Создаем новый канал для нового диалога
			newCh := &model.Ch{
				DialogID: dialogID,
				TxCh:     make(chan model.Message, create.TxChanBuffer), // Буфер как в CreateBaseResponder
				RxCh:     make(chan model.Message, create.RxChanBuffer),
			}
			respModel.ChanMap[dialogID] = newCh

			// Обновляем основной Chan для совместимости (deprecated)
			respModel.Chan = newCh

			//logger.Debug("Создан новый канал для существующего респондента: dialogID=%d, respId=%d, буфер TxCh=%d",
			//	dialogID, respId, cap(newCh.TxCh), assist.userID)
		}

		// Конвертируем в model.RespModel
		return m.convertToModelRespModel(respModel), nil
	}

	// Используем helper-функцию для создания базовых компонентов
	ctx, cancel, ch, ttl := model.CreateBaseResponder(m.ctx, m.UserModelTTl, assist, dialogID, respName)

	googleResp := &GoogleRespModel{
		Ctx:      ctx,
		Cancel:   cancel,
		Chan:     ch,                                 // Deprecated: для обратной совместимости
		ChanMap:  map[uint64]*model.Ch{dialogID: ch}, // Унифицированный map каналов
		TTL:      ttl,
		Assist:   assist,
		RespName: respName,
		Services: Services{},
	}

	// Загружаем конфигурацию агента из БД
	if err := m.loadAgentConfig(assist.UserID, googleResp); err != nil {
		cancel() // Очищаем ресурсы при ошибке
		return nil, fmt.Errorf("ошибка загрузки конфигурации агента: %w", err)
	}

	// Сохраняем по respId (как в OpenAI версии)
	m.responders.Store(respId, googleResp)

	//logger.Debug("Создан новый Google респондент для dialogID %d, respId=%d с каналом TxCh (буфер=%d)",
	//	dialogID, respId, cap(ch.TxCh), assist.userID)

	// Уведомляем ожидающие горутины о создании респондента
	model.NotifyWaitChannels(&m.waitChannels, respId)

	// Конвертируем в model.RespModel
	return m.convertToModelRespModel(googleResp), nil
}

// GetCh получает канал по respId, ждёт его создания если необходимо
func (m *Model) GetCh(respId uint64) (*model.Ch, error) {
	return model.GetChannel(
		respId,
		m.ctx,
		&m.waitChannels,
		&m.responders,
		func(val interface{}) (*model.Ch, error) {
			respModel := val.(*GoogleRespModel)
			return model.ExtractChannelWithPriority(respModel)
		},
	)
}

// GetRespIdBydialogID получает respId по dialogID
func (m *Model) GetRespIdBydialogID(dialogID uint64) (uint64, error) {
	// Ищем responder по dialogID в Chan
	var foundRespId uint64
	found := false

	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*GoogleRespModel)

		if respModel.Chan != nil && respModel.Chan.DialogID == dialogID {
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
		return 0, fmt.Errorf("RespModel не найден для dialogID %d", dialogID)
	}

	return foundRespId, nil
}

// SaveAllContextDuringExit сохраняет все контексты при выходе
func (m *Model) SaveAllContextDuringExit() {
	// Google не использует SaveContext (история в БД через ReadDialog)
	// Поэтому этот метод пустой
}

// CleanDialogData очищает данные диалога
func (m *Model) CleanDialogData(dialogID uint64) {
	// Получаем respId по dialogID
	respId, err := m.GetRespIdBydialogID(dialogID)
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
		//logger.Debug("Очищены данные диалога %d (respId: %d)", dialogID, respId)
	}
}

// CleanUp фоновая очистка устаревших респондентов
func (m *Model) CleanUp() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.CleanupExpiredResponders()
			m.cleanupExpiredWaitChannels()
		case <-m.ctx.Done():
			//logger.Info("GoogleModel: CleanUp остановлен")
			return
		}
	}
}

// cleanupExpiredWaitChannels удаляет заблокированные waitChannels для несуществующих respId
func (m *Model) cleanupExpiredWaitChannels() {
	m.waitChannels.Range(func(key, value interface{}) bool {
		respId := key.(uint64)
		// Если респондента нет, это значит что waitCh никогда не будет закрыт
		// Удаляем такой waitCh чтобы не было утечек памяти
		if _, ok := m.responders.Load(respId); !ok {
			m.waitChannels.Delete(respId)
			//logger.Debug("Удален заблокированный waitCh для respId %d", respId)
		}
		return true
	})
}

// convertToModelRespModel конвертирует GoogleRespModel в model.RespModel
// Использует ChanMap для унификации с OpenAI
func (m *Model) convertToModelRespModel(internal *GoogleRespModel) *model.RespModel {
	return &model.RespModel{
		Ctx:      internal.Ctx,
		Cancel:   internal.Cancel,
		Chan:     internal.ChanMap, // Используем унифицированный ChanMap
		TTL:      internal.TTL,
		Assist:   internal.Assist,
		RespName: internal.RespName,
		Services: model.Services{
			Listener:   &internal.Services.Listener,
			Respondent: &internal.Services.Respondent,
		},
	}
}

// getOrCreateDialogCache получает или создаёт кэш диалога с обновлением ExpireAt
func (m *Model) getOrCreateDialogCache(dialogID uint64) *DialogCache {
	expireAt := time.Now().Add(create.DialogLiveTimeout)

	// Пытаемся получить существующий кэш
	if cacheIface, ok := m.dialogCache.Load(dialogID); ok {
		cache := cacheIface.(*DialogCache)
		cache.ExpireAt = expireAt // Обновляем время истечения
		return cache
	}

	// Создаём новый кэш
	cache := &DialogCache{
		dialogID: dialogID,
		Contents: []GoogleContent{},
		ExpireAt: expireAt,
	}

	m.dialogCache.Store(dialogID, cache)

	return cache
}

// addMessageToCache добавляет сообщение в кэш диалога с ограничением по количеству
// Если превышен лимит DialogHistoryLimit, удаляет старые сообщения
func (m *Model) addMessageToCache(dialogID uint64, content GoogleContent) {
	cache := m.getOrCreateDialogCache(dialogID)
	cache.Contents = append(cache.Contents, content)

	// Ограничиваем количество сообщений до DialogHistoryLimit
	maxMessages := int(create.DialogHistoryLimit)
	if len(cache.Contents) > maxMessages {
		// Удаляем старые сообщения, оставляя только последние maxMessages
		cache.Contents = cache.Contents[len(cache.Contents)-maxMessages:]
		//logger.Debug("Достигнут лимит сообщений в кэше диалога %d (%d), удалены старые сообщения",
		//	dialogID, maxMessages)
	}
}

// getDialogHistoryFromCache получает историю диалога из кэша
func (m *Model) getDialogHistoryFromCache(dialogID uint64) ([]GoogleContent, bool) {
	if cacheIface, ok := m.dialogCache.Load(dialogID); ok {
		cache := cacheIface.(*DialogCache)

		// Копируем содержимое для безопасности (поскольку Contents может быть изменён в другой горутине)
		contents := make([]GoogleContent, len(cache.Contents))
		copy(contents, cache.Contents)

		//logger.Debug("Получена история из кэша диалога %d, сообщений: %d", dialogID, len(contents))
		return contents, true
	}

	//logger.Debug("Кэш не найден для диалога %d", dialogID)
	return nil, false
}

// getCachedEmbedding проверяет кэш эмбеддингов и возвращает закэшированный результат
func (m *Model) getCachedEmbedding(text string) ([]float32, bool) {
	hash := m.hashText(text)

	if cacheIface, ok := m.embeddingCache.Load(hash); ok {
		cached := cacheIface.(*CachedEmbedding)

		// Проверяем не истёк ли кэш
		if time.Now().Before(cached.ExpireAt) {
			return cached.Embedding, true
		}

		// Кэш истёк - удаляем
		m.embeddingCache.Delete(hash)
	}

	return nil, false
}

// setCachedEmbedding сохраняет эмбеддинг в кэш с TTL 5 минут
func (m *Model) setCachedEmbedding(text string, embedding []float32) {
	hash := m.hashText(text)

	cached := &CachedEmbedding{
		Embedding: embedding,
		ExpireAt:  time.Now().Add(5 * time.Minute),
		Hash:      hash,
	}

	m.embeddingCache.Store(hash, cached)
}

// hashText создаёт короткий hash текста (первые 16 символов SHA256)
func (m *Model) hashText(text string) string {
	h := sha256.New()
	h.Write([]byte(text))
	fullHash := fmt.Sprintf("%x", h.Sum(nil))

	// Возвращаем первые 16 символов (риск коллизий исчезающе мал)
	if len(fullHash) > 16 {
		return fullHash[:16]
	}
	return fullHash
}

// periodicFlush удаляет из кэша диалоги с истёкшим ExpireAt и истекшие респонденты
func (m *Model) periodicFlush() {
	ticker := time.NewTicker(30 * time.Second) // Проверяем каждые 30 секунд
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			expiredDialogCount := 0
			expiredRespCount := 0
			expiredEmbeddingCount := 0

			// Очистка кэша диалогов
			m.dialogCache.Range(func(key, value interface{}) bool {
				dialogID := key.(uint64)
				cache := value.(*DialogCache)

				if now.After(cache.ExpireAt) {
					m.dialogCache.Delete(dialogID)
					//logger.Debug("Удален кэш диалога %d из-за истечения ExpireAt", dialogID)
					expiredDialogCount++
				}

				return true
			})

			// Очистка кэша эмбеддингов
			m.embeddingCache.Range(func(key, value interface{}) bool {
				cached := value.(*CachedEmbedding)

				if now.After(cached.ExpireAt) {
					m.embeddingCache.Delete(key)
					expiredEmbeddingCount++
				}

				return true
			})

			// Очистка истекших респондентов (аналогично OpenAI)
			m.responders.Range(func(key, value interface{}) bool {
				responder := value.(*GoogleRespModel)
				ttlExpired := responder.TTL.Before(now)

				respId, ok := key.(uint64)
				if !ok {
					//logger.Error("Некорректный тип ключа responders: %T, ожидался uint64", key)
					return true
				}

				if ttlExpired {
					// Отменяем контекст респондента
					if responder.Cancel != nil {
						responder.Cancel()
					}

					// Закрываем канал респондента
					// ВАЖНО: В Google Chan - это *model.Ch (одиночный канал), а не map[uint64]*model.Ch
					if responder.Chan != nil {
						// Канал закроется автоматически при отмене контекста через Cancel()
						// Дополнительное закрытие не требуется (может вызвать панику)
					}

					// Удаляем респондента
					m.responders.Delete(respId)
					expiredRespCount++
					//logger.Info("Удален просроченный GoogleRespModel для respId=%d (TTL истёк)", respId)
				}

				return true
			})

			if expiredDialogCount > 0 || expiredRespCount > 0 || expiredEmbeddingCount > 0 {
				//logger.Debug("periodicFlush: удалено %d кэшей диалогов, %d респондентов, %d эмбеддингов",
				//	expiredDialogCount, expiredRespCount, expiredEmbeddingCount)
			}

		case <-m.ctx.Done():
			//logger.Debug("periodicFlush остановлен")
			return
		}
	}
}

// InvalidateUserAgentConfigCache инвалидирует кэш конфигурации модели для пользователя
func (m *Model) InvalidateUserAgentConfigCache(userID uint32) {
	var invalidatedCount int
	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*GoogleRespModel)
		if respModel.Assist.UserID == userID {
			m.responders.Delete(key)
			invalidatedCount++
		}
		return true
	})
	if invalidatedCount > 0 {
		//logger.Debug("Инвалидирован кэш конфигурации модели для userID=%d (удалено %d респондентов)", userID, invalidatedCount)
	}
}

// DisconnectUser выполняет graceful завершение всех активных сессий пользователя:
// 1. Закрывает все realtime-сессии (WebSocket + каналы)
// 2. Отменяет контексты всех респондентов
// 3. Удаляет респондентов из кэша
func (m *Model) DisconnectUser(userID uint32) {
	// Шаг 1: закрываем realtime-сессии пользователя
	m.realtimeSessions.Range(func(key, value interface{}) bool {
		rs := value.(*GoogleRealtimeSession)
		if rs.userID == userID {
			respId := key.(uint64)
			m.CloseRealtimeSession(respId)
		}
		return true
	})

	// Шаг 2: отменяем контексты и удаляем респондентов
	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*GoogleRespModel)
		if respModel.Assist.UserID == userID {
			if respModel.Cancel != nil {
				respModel.Cancel()
			}
			m.responders.Delete(key)
		}
		return true
	})
}
