package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ikermy/AiR_Common/pkg/com"
	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
	"github.com/ikermy/AiR_Common/pkg/model/provider_catalog"
)

type DB = comdb.Exterior

// Model управляет OpenAI моделями и респондентами
type Model struct {
	ctx              context.Context
	cancel           context.CancelFunc
	client           *create.OpenAIAgentClient // HTTP клиент для работы с OpenAI API
	db               DB
	responders       sync.Map // respId -> *RespModel
	waitChannels     sync.Map
	dialogCache      sync.Map // dialogID -> *DialogCache (локальный кэш истории диалогов)
	realtimeSessions sync.Map // respId -> *RealtimeSession (параллельные голосовые сессии)
	UserModelTTl     time.Duration
	actionHandler    model.ActionHandler
	universalModel   *create.UniversalModel
	shutdownOnce     sync.Once
}

// RespModel представляет респондента для OpenAI
type RespModel struct {
	Ctx      context.Context
	Cancel   context.CancelFunc
	Chan     *model.Ch            // Канал для этого респондента (основной)
	ChanMap  map[uint64]*model.Ch // Map каналов для поддержки множественных dialogID
	TTL      time.Time
	Assist   model.Assistant
	RespName string
	Services Services
	Haunter  bool // Модель используется для поиска лидов
	// Кэш конфигурации агента для быстрого доступа
	AgentConfig *AgentConfig
}

// GetChannel реализует интерфейс model.ChannelProvider
func (r *RespModel) GetChannel() *model.Ch {
	return r.Chan
}

// GetChannelMap реализует интерфейс model.ChannelProvider
func (r *RespModel) GetChannelMap() map[uint64]*model.Ch {
	return r.ChanMap
}

// AgentConfig хранит конфигурацию агента для OpenAI модели
// В отличие от Assistants API, конфигурация хранится в БД и передается с каждым запросом
type AgentConfig struct {
	ModelId        uint64         `json:"model_id"`   // ID модели в БД
	ModelName      string         `json:"model_name"` // Имя модели из user_gpt.AssistantId (gpt-5-mini и т.д.)
	SystemPrompt   string         `json:"system_prompt"`
	Tools          []any          `json:"tools"`
	ResponseFormat map[string]any `json:"response_format"`
	VectorStoreIds []string       `json:"vector_store_ids,omitempty"`
	FileIds        []any          `json:"file_ids,omitempty"`

	// Дополнительные возможности
	Search      bool   `json:"search"`      // Поиск по векторному хранилищу
	Interpreter bool   `json:"interpreter"` // Code Interpreter
	Haunter     bool   `json:"haunter"`     // Модель для поиска лидов
	Operator    bool   `json:"operator"`    // Вызов оператора
	MetaAction  string `json:"meta_action"` // Целевое действие
	WebSearch   bool   `json:"web_search"`  // Веб-поиск
	Image       bool   `json:"image"`       // Генерация изображений

	// Голосовой режим реального времени (OpenAI Realtime API)
	RealtimeEnabled bool                `json:"realtime_enabled"`       // Голосовой режим включён для этой модели
	RealtimeModel   string              `json:"realtime_model"`         // Имя realtime-модели
	RealtimeVAD     *create.RealtimeVAD `json:"realtime_vad,omitempty"` // Параметры VAD и генерации
}

// openaiRagResp — результат работы applyRAG для OpenAI провайдера
type openaiRagResp struct {
	contextText string        // Обогащённый контекст из Vector Store (пустой если RAG не нужен или не дал результата)
	history     []ChatMessage // История диалога (из кэша или БД)
	respModel   *RespModel    // Загруженный респондент
	err         error
	// Метрики производительности
	embeddingDuration     time.Duration
	searchDuration        time.Duration
	historyLoadDuration   time.Duration
	responderLoadDuration time.Duration
}

// DialogCache кэширует историю диалога в памяти для быстрого доступа
type DialogCache struct {
	dialogID uint64
	Messages []ChatMessage // История диалога в формате OpenAI
	ExpireAt time.Time     // Время истечения кэша
}

// ChatMessage представляет сообщение в формате OpenAI Chat Completions
type ChatMessage struct {
	Role       string `json:"role"`    // "system", "user", "assistant"
	Content    any    `json:"content"` // string или массив content parts
	Name       string `json:"name,omitempty"`
	ToolCalls  []any  `json:"tool_calls,omitempty"`
	ToolCallId string `json:"tool_call_id,omitempty"`
}

type Services struct {
	Listener   atomic.Bool
	Respondent atomic.Bool
}

// New создаёт новый экземпляр OpenAIModel
func New(parent context.Context, d DB, actionHandler model.ActionHandler) *Model {
	ctx, cancel := context.WithCancel(parent)

	// Клиент не принимает глобальный ключ — персональный ключ читается из БД через keyResolver.
	openaiClient := create.NewOpenAIAgentClient(ctx)

	openaiClient.SetKeyResolver(func(userID uint32) string {
		if key, err := d.GetUserAPIKey(userID, create.ProviderOpenAI); err == nil {
			return key
		}
		return ""
	})

	m := &Model{
		ctx:           ctx,
		cancel:        cancel,
		client:        openaiClient,
		db:            d,
		responders:    sync.Map{},
		waitChannels:  sync.Map{},
		dialogCache:   sync.Map{},
		UserModelTTl:  mode.UserModelTTl,
		actionHandler: actionHandler,
	}

	// Запускаем periodicFlush в фоновой горутине для очистки истекших диалогов из кэша
	go m.periodicFlush()

	return m
}

// NewAsRouterOption создаёт OpenAI модель и возвращает её как опцию для ModelRouter
// Использование: router := model.NewModelRouter(ctx, db, openai.NewAsRouterOption())
func NewAsRouterOption() model.RouterOption {
	return func(r *model.Router, ctx context.Context, db model.DB) error {
		openaiDB, ok := db.(DB)
		if !ok {
			return fmt.Errorf("DB не соответствует интерфейсу openai.DB")
		}

		// Создаём универсальный обработчик функций
		actionHandler := model.NewUniversalActionHandler(ctx)

		// Создаём OpenAI модель (клиент уже инициализирован в New)
		openaiModel := New(ctx, openaiDB, actionHandler)

		// Создаём UniversalModel для управления моделями
		universalModel := create.New(ctx, openaiDB)

		// Устанавливаем связь с universalModel
		openaiModel.SetUniversalModel(universalModel)

		// Регистрируем модель в роутере
		return model.WithOpenAIModel(openaiModel)(r, ctx, db)
	}
}

// SetUniversalModel устанавливает UniversalModel
func (m *Model) SetUniversalModel(um *create.UniversalModel) {
	m.universalModel = um
}

// Реализация интерфейса model.Inter
func (m *Model) NewMessage(operator model.Operator, msgType string, content *model.AssistResponse, name *string, files ...model.FileUpload) model.Message {
	var nameStr string
	if name != nil {
		nameStr = *name
	}

	return model.Message{
		Operator:  operator,
		Type:      msgType,
		Content:   *content,
		Name:      nameStr,
		Timestamp: time.Now(),
		Files:     files,
	}
}

func (m *Model) GetFileAsReader(_ uint32, url string) (io.Reader, error) {
	if url == "" {
		return nil, fmt.Errorf("не указан источник файла: отсутствуют URL")
	}

	if strings.HasPrefix(url, "openai_file:") {
		fileID := strings.TrimPrefix(url, "openai_file:")
		content, err := m.client.DownloadFileContent(m.ctx, fileID)
		if err != nil {
			return nil, fmt.Errorf("ошибка получения файла из OpenAI: %w", err)
		}
		return bytes.NewReader(content), nil
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка подготовки запроса загрузки файла: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки файла по URL: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("ошибка HTTP при загрузке файла: статус %d", resp.StatusCode)
	}

	return resp.Body, nil
}

func (m *Model) GetOrSetRespGPT(assist model.Assistant, dialogID, respId uint64, respName string) (*model.RespModel, error) {
	// Сначала проверяем кэш
	// Используем respId как ключ
	if val, ok := m.responders.Load(respId); ok {
		respModel := val.(*RespModel)
		respModel.TTL = time.Now().Add(m.UserModelTTl) // Обновляем TTL

		// АВТОМАТИЧЕСКАЯ предзагрузка истории диалога (если кэш пустой)
		m.preloadDialogHistoryIfNeeded(dialogID, assist.UserID)

		return m.convertToModelRespModel(respModel), nil
	}

	// Используем helper-функцию для создания базовых компонентов
	userCtx, cancel, ch, ttl := model.CreateBaseResponder(m.ctx, m.UserModelTTl, assist, dialogID, respName)

	user := &RespModel{
		Assist:      assist,
		RespName:    respName,
		TTL:         ttl,
		Chan:        ch,
		Services:    Services{},
		Ctx:         userCtx,
		Cancel:      cancel,
		AgentConfig: nil, // Будет загружена ниже
	}

	// Загружаем конфигурацию агента из БД
	agentConfig, haunter, err := m.loadAgentConfig(assist.UserID, user)
	if err != nil {
		//logger.Warn("Ошибка загрузки конфигурации агента: %v, используем конфигурацию по умолчанию", err, assist.userID)
	} else {
		user.AgentConfig = agentConfig
		user.Haunter = haunter
	}

	// Используем respId как ключ
	m.responders.Store(respId, user)

	// Уведомляем ожидающие горутины о создании респондента
	model.NotifyWaitChannels(&m.waitChannels, respId)

	// АВТОМАТИЧЕСКАЯ предзагрузка истории диалога для нового респондента
	m.preloadDialogHistoryIfNeeded(dialogID, assist.UserID)

	return m.convertToModelRespModel(user), nil
}

func (m *Model) GetCh(respId uint64) (*model.Ch, error) {
	return model.GetChannel(
		respId,
		m.ctx,
		&m.waitChannels,
		&m.responders,
		func(val any) (*model.Ch, error) {
			respModel := val.(*RespModel)
			return model.ExtractChannelWithPriority(respModel)
		},
	)
}

func (m *Model) GetRespIdByDialogID(dialogID uint64) (uint64, error) {
	return model.GetRespIdBydialogIDUniversal(dialogID, &m.responders)
}

// ============================================================================
// AGENT CONFIG METHODS
// ============================================================================

// loadAgentConfig загружает конфигурацию агента для OpenAI модели из БД
// По образцу Google провайдера - конфигурация хранится в БД, а не в OpenAI API
// Возвращает конфигурацию агента и haunter флаг явно
func (m *Model) loadAgentConfig(userID uint32, _ *RespModel) (*AgentConfig, bool, error) {
	// Получаем API-ключ напрямую через DB: это обеспечивает правильную обработку $mk$-ключей —
	// если MasterKey недоступен, ошибка и уведомление пропагируются явно, а не теряются в HasAPIKey.
	apiKey, err := m.db.GetUserAPIKey(userID, create.ProviderOpenAI)
	if err != nil {
		return nil, false, fmt.Errorf("ошибка получения OpenAI API-ключа для пользователя %d: %w", userID, err)
	}
	if m.client == nil || apiKey == "" {
		return nil, false, fmt.Errorf("OpenAI API ключ не настроен для пользователя %d: добавьте персональный ключ через настройки", userID)
	}

	// Получаем все модели пользователя
	userModels, err := m.db.GetAllUserModels(userID)
	if err != nil {
		return nil, false, fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	// Ищем активную модель OpenAI
	var found *create.UserModelRecord
	for i := range userModels {
		if userModels[i].Provider == create.ProviderOpenAI {
			found = &userModels[i]
			break
		}
	}

	if found == nil {
		return nil, false, fmt.Errorf("модель OpenAI не найдена для userID %d", userID)
	}

	// Инициализируем базовую конфигурацию.
	// ModelName берём из user_gpt.AssistantId — там хранится имя модели выбранной пользователем
	// из каталога gpt_models (например "gpt-5-mini").
	modelName := found.AssistId
	if modelName == "" {
		// AssistId не заполнен — берём модель по умолчанию из gpt_models (IsDefault=1)
		_, defaultName, err := m.db.DefaultProvidersModels(create.ProviderOpenAI.String())
		if err != nil {
			return nil, false, fmt.Errorf("имя модели OpenAI не задано и получить модель по умолчанию не удалось: %w", err)
		}
		modelName = defaultName
	}
	agentConfig := &AgentConfig{
		ModelId:   found.ModelId,
		ModelName: modelName,
	}

	var haunter bool

	// Загружаем полные данные модели из БД
	compressedData, _, err := m.db.ReadUserModelByProvider(userID, create.ProviderOpenAI)
	if err != nil {
		//logger.Warn("Ошибка чтения данных модели из БД: %v, используем конфигурацию по умолчанию", err, userID)
	} else if compressedData != nil {
		// Распаковываем полные данные модели для получения параметров
		if m.universalModel != nil {
			modelData, decompressErr := m.universalModel.DecompressModelData(compressedData, nil)
			if decompressErr != nil {
				//logger.Warn("Ошибка распаковки данных модели: %v", decompressErr, userID)
			} else {
				agentConfig.MetaAction = modelData.MetaAction
				agentConfig.Haunter = modelData.Haunter
				agentConfig.Search = modelData.Search
				agentConfig.Operator = modelData.Operator
				agentConfig.Interpreter = modelData.Interpreter
				agentConfig.WebSearch = modelData.WebSearch
				agentConfig.RealtimeEnabled = modelData.Realtime
				agentConfig.Image = modelData.Image
				agentConfig.RealtimeVAD = modelData.RealtimeVAD

				haunter = modelData.Haunter
			}
		}
	}

	// Загружаем vector store IDs если есть файлы
	if found.FileIds != nil && len(found.FileIds) > 0 {
		// Извлекаем VectorId из VecIds в AllIds
		var vecIds create.VecIds
		if err := json.Unmarshal(found.AllIds, &vecIds); err == nil {
			agentConfig.VectorStoreIds = vecIds.VectorId
			// Конвертируем []Ids в []any
			agentConfig.FileIds = make([]any, len(found.FileIds))
			for i, fileId := range found.FileIds {
				agentConfig.FileIds[i] = fileId
			}
		}
	}

	// Формируем system_prompt, tools и response_format динамически
	if err := m.buildAgentConfiguration(userID, agentConfig, compressedData); err != nil {
		//logger.Warn("Ошибка формирования конфигурации агента: %v", err, userID)
	}

	//logger.Debug("Загружена конфигурация агента (GPT Model: %s, AssistName: %s)", agentConfig.ModelName, respModel.Assist.AssistName, userID)
	return agentConfig, haunter, nil
}

// buildAgentConfiguration формирует system_prompt, tools и response_format.
// Если MCP доступен: system_prompt = modelData.Prompt + hint от MCP, function-tools от MCP.
// Если MCP недоступен: system_prompt = modelData.Prompt (без инструкций), function-tools не добавляются.
// Нативные OpenAI инструменты (code_interpreter, web_search) добавляются всегда локально.
func (m *Model) buildAgentConfiguration(userID uint32, config *AgentConfig, compressedData []byte) error {
	// Распаковываем данные модели
	modelData, err := m.universalModel.DecompressModelData(compressedData, nil)
	if err != nil {
		return fmt.Errorf("ошибка распаковки: %w", err)
	}

	// =========================================================================
	// SYSTEM PROMPT — получаем hint от MCP.
	// Если MCP недоступен — используем только modelData.Prompt (без function-инструкций).
	// =========================================================================
	mcpAvailable := false
	if mcpProvider, ok := m.actionHandler.(model.MCPConfigProvider); ok {
		if hint, err := mcpProvider.FetchSystemPrompt(m.ctx, userID, create.ProviderOpenAI); err == nil {
			config.SystemPrompt = modelData.Prompt + "\n\n" + hint
			mcpAvailable = true
		}
		// При ошибке — MCP недоступен, используем plain prompt
	}

	if !mcpAvailable {
		config.SystemPrompt = modelData.Prompt
	}

	// =========================================================================
	// TOOLS — нативные OpenAI инструменты (всегда локально).
	// Function-инструменты добавляются только если MCP доступен.
	// =========================================================================
	var tools []any

	// Code Interpreter — нативный OpenAI инструмент, не через MCP
	// ВАЖНО: Responses API требует поле "container" для code_interpreter!
	if config.Interpreter {
		tools = append(tools, map[string]any{
			"type": "code_interpreter",
			"container": map[string]any{
				"type":         "auto",
				"memory_limit": "1g",
			},
		})
	}

	// Web Search — нативный OpenAI инструмент, не через MCP
	if config.WebSearch {
		tools = append(tools, map[string]any{
			"type": "web_search",
		})
	}

	// Function tools — только от MCP; если сервер недоступен — не добавляем
	if mcpAvailable {
		if mcpProvider, ok := m.actionHandler.(model.MCPConfigProvider); ok {
			if mcpTools, err := mcpProvider.FetchToolsList(m.ctx, userID, create.ProviderOpenAI); err == nil {
				for _, t := range mcpTools {
					tools = append(tools, map[string]any{
						"type":        "function",
						"name":        t.Name,
						"description": t.Description,
						"strict":      false,
						"parameters":  t.InputSchema,
					})
				}
			}
		}
	}

	config.Tools = tools

	// Формируем response format с динамической схемой
	hasMetaAction := config.MetaAction != ""
	hasOperator := config.Operator
	dynamicSchema := create.GenerateModelSchema(hasMetaAction, hasOperator)

	config.ResponseFormat = map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   "assist_response",
			"strict": true,
			"schema": dynamicSchema,
		},
	}

	config.RealtimeModel = create.RealtimeOpenAIModel

	// Передаём RealtimeVAD конфигурацию из распакованных данных модели
	// (с уже применёнными дефолтными значениями из DecompressModelData)

	config.RealtimeVAD = modelData.RealtimeVAD

	return nil
}

// ============================================================================
// DIALOG CACHE METHODS
// ============================================================================

// periodicFlush периодически очищает истекшие записи из dialogCache
func (m *Model) periodicFlush() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			flushedCount := 0

			m.dialogCache.Range(func(key, value any) bool {
				cache := value.(*DialogCache)
				if cache.ExpireAt.Before(now) {
					m.dialogCache.Delete(key)
					flushedCount++
				}
				return true
			})

			//if flushedCount > 0 {
			//	logger.Debug("OpenAI periodicFlush: удалено %d истекших кэшей диалогов", flushedCount)
			//}
		case <-m.ctx.Done():
			return
		}
	}
}

// getOrCreateDialogCache получает или создаёт кэш для диалога
func (m *Model) getOrCreateDialogCache(dialogID uint64) *DialogCache {
	if val, ok := m.dialogCache.Load(dialogID); ok {
		cache := val.(*DialogCache)
		// Продлеваем срок жизни кэша
		cache.ExpireAt = time.Now().Add(create.DialogLiveTimeout)
		return cache
	}

	cache := &DialogCache{
		dialogID: dialogID,
		Messages: []ChatMessage{},
		ExpireAt: time.Now().Add(create.DialogLiveTimeout),
	}
	m.dialogCache.Store(dialogID, cache)
	return cache
}

// getDialogHistoryFromCache получает историю диалога из кэша
func (m *Model) getDialogHistoryFromCache(dialogID uint64) ([]ChatMessage, bool) {
	if val, ok := m.dialogCache.Load(dialogID); ok {
		cache := val.(*DialogCache)
		// Проверяем что кэш не истёк
		if cache.ExpireAt.After(time.Now()) {
			// Продлеваем срок жизни
			cache.ExpireAt = time.Now().Add(create.DialogLiveTimeout)
			return cache.Messages, true
		}
		// Кэш истёк - удаляем
		m.dialogCache.Delete(dialogID)
	}
	return nil, false
}

// addMessageToCache добавляет сообщение в кэш диалога
func (m *Model) addMessageToCache(dialogID uint64, message ChatMessage) {
	cache := m.getOrCreateDialogCache(dialogID)
	cache.Messages = append(cache.Messages, message)

	// Ограничиваем размер истории
	maxMessages := int(create.DialogHistoryLimit)
	if len(cache.Messages) > maxMessages {
		cache.Messages = cache.Messages[len(cache.Messages)-maxMessages:]
	}
}

// preloadDialogHistoryIfNeeded автоматически загружает историю диалога если кэш пустой
// Вызывается неявно в GetOrSetRespGPT для обеспечения контекста с первого сообщения
func (m *Model) preloadDialogHistoryIfNeeded(dialogID uint64, _ uint32) {
	// Проверяем наличие кэша
	if _, found := m.getDialogHistoryFromCache(dialogID); found {
		// Кэш уже есть - ничего не делаем
		return
	}

	// Загружаем историю из БД в фоновой горутине для неблокирующей работы
	go func() {
		history, err := m.ConvertDialogToOpenAIFormat(dialogID)
		if err != nil {
			// Пустая история - это нормально для нового диалога
			//logger.Debug("История диалога %d не найдена или пуста: %v", dialogID, err)
			history = []ChatMessage{}
		}

		// Ограничиваем размер истории
		maxMessages := int(create.DialogHistoryLimit)
		if len(history) > maxMessages {
			history = history[len(history)-maxMessages:]
		}

		// Сохраняем в кэш
		cache := m.getOrCreateDialogCache(dialogID)
		cache.Messages = history

		//if len(history) > 0 {
		//	logger.Info("Автоматически предзагружена история диалога %d: %d сообщений", dialogID, len(history))
		//} else {
		//	logger.Debug("Диалог %d начат с пустой историей (новый диалог)", dialogID)
		//}
	}()
}

// ============================================================================
// EXISTING METHODS
// ============================================================================

func (m *Model) SaveAllContextDuringExit() {
	// В новом подходе с Chat Completions API история сохраняется автоматически через БД
	//logger.Info("SaveAllContextDuringExit: пропускаем (Chat Completions API не требует сохранения контекста)")
}

func (m *Model) CleanDialogData(dialogID uint64) {
	// Получаем respId по dialogID
	respId, err := m.GetRespIdByDialogID(dialogID)
	if err != nil {
		//logger.Warn("CleanDialogData: не удалось получить respId для dialogID %d: %v", dialogID, err)
		return
	}

	// Удаляем респондента по respId
	if value, ok := m.responders.Load(respId); ok {
		respModel := value.(*RespModel)
		if respModel.Cancel != nil {
			respModel.Cancel()
		}
		// Закрываем каналы
		m.closeResponderChannels(respModel)
		m.responders.Delete(respId)
		//logger.Info("Очищены данные диалога %d (respId: %d)", dialogID, respId)
	}

	// Также удаляем кэш диалога
	m.dialogCache.Delete(dialogID)
}

func (m *Model) DeleteTempFile(fileID string) error {
	// Удаляем временный файл из OpenAI
	if err := m.client.DeleteFile(m.ctx, fileID); err != nil {
		return fmt.Errorf("ошибка удаления временного файла: %w", err)
	}
	return nil
}

func (m *Model) TranscribeAudio(_ uint32, audioData []byte, fileName string) (string, error) {
	// Используем существующий метод OpenAIAgentClient.TranscribeAudio
	if m.client == nil {
		return "", fmt.Errorf("OpenAI клиент не инициализирован")
	}

	text, err := m.client.TranscribeAudio(m.ctx, audioData, fileName)
	if err != nil {
		return "", fmt.Errorf("ошибка транскрипции аудио: %w", err)
	}

	return text, nil
}

func (m *Model) Shutdown(shutCh chan<- com.LogMsg) {
	var shutdownErrors []string

	m.shutdownOnce.Do(func() {
		shutCh <- com.LogMsg{
			Msg: "начало shutdown",
			Mod: "OpenAIModel",
			Log: 0, // 0 - Info
			UID: 0,
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		shutCh <- com.LogMsg{
			Msg: "сохранение всех контекстов при завершении работы",
			Mod: "OpenAIModel",
			Log: 0, // 0 - Info
			UID: 0,
		}
		if err := m.saveAllContextsGracefullyCtx(shutdownCtx); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Sprintf("ошибка сохранения контекстов: %v", err))
		}

		// Закрываем все активные Realtime-сессии
		m.realtimeSessions.Range(func(key, value any) bool {
			if rs, ok := value.(*RealtimeSession); ok {
				rs.cancel()
				_ = rs.openaiConn.Close()
			}
			m.realtimeSessions.Delete(key)
			return true
		})

		if m.cancel != nil {
			m.cancel()
		}

		m.cleanupAllResponders()
		m.cleanupWaitChannels()

		shutCh <- com.LogMsg{
			Msg: "процесс завершения работы модуля завершен",
			Mod: "OpenAIModel",
			Log: 0, // 0 - Info
			UID: 0,
		}
	})

	if len(shutdownErrors) > 0 {

		shutCh <- com.LogMsg{
			Msg: fmt.Sprintf("ошибки при завершении работы: %s", strings.Join(shutdownErrors, "; ")),
			Mod: "OpenAIModel",
			Log: 2, // 2 - Error
			UID: 0,
		}
	}

	shutCh <- com.LogMsg{
		Msg: "модуль успешно завершил работу",
		Mod: "OpenAIModel",
		Log: 0, // 0 - Info
		UID: 0,
	}
}

// Вспомогательная функция для конвертации внутреннего RespModel в model.RespModel
func (m *Model) convertToModelRespModel(internal *RespModel) *model.RespModel {
	// Используем существующий ChanMap или создаем новый
	chanMap := internal.ChanMap
	if chanMap == nil {
		chanMap = make(map[uint64]*model.Ch)
		internal.ChanMap = chanMap
	}

	// Добавляем основной канал если он есть и не добавлен
	if internal.Chan != nil {
		if _, exists := chanMap[internal.Chan.DialogID]; !exists {
			chanMap[internal.Chan.DialogID] = internal.Chan
		}
	}

	return &model.RespModel{
		Ctx:      internal.Ctx,
		Cancel:   internal.Cancel,
		Chan:     chanMap, // Возвращаем ссылку на тот же map
		TTL:      internal.TTL,
		Assist:   internal.Assist,
		RespName: internal.RespName,
		Services: model.Services{
			Listener:   &internal.Services.Listener,
			Respondent: &internal.Services.Respondent,
		},
	}
}

// ============================================================================
// CLEANUP METHODS
// ============================================================================

// CleanUp периодически очищает просроченные RespModel
func (m *Model) CleanUp() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			deletedRespCount := 0
			checkedRespCount := 0

			m.responders.Range(func(key, value any) bool {
				responder := value.(*RespModel)
				checkedRespCount++
				ttlExpired := responder.TTL.Before(now)

				respId, ok := key.(uint64)
				if !ok {
					//logger.Error("Некорректный тип ключа: %T, ожидался uint64", key)
					return true
				}

				if ttlExpired {
					if responder.Cancel != nil {
						responder.Cancel()
					}
					m.closeResponderChannels(responder)
					m.responders.Delete(respId)
					deletedRespCount++
					//logger.Debug("Удален просроченный RespModel для respId %d (TTL истёк)", respId)
				}

				return true
			})

			if deletedRespCount > 0 {
				//logger.Debug("Очистка завершена: проверено %d RespModel, удалено %d RespModel",
				//	checkedRespCount, deletedRespCount)
			}
		case <-m.ctx.Done():
			return
		}
	}
}

func (m *Model) closeResponderChannels(respModel *RespModel) {
	model.CloseResponderChannelsUniversal(respModel)
}

func (m *Model) cleanupWaitChannels() {
	deletedCount := model.CleanupWaitChannelsUniversal(&m.waitChannels, &m.responders)
	if deletedCount > 0 {
		//logger.Debug("Очищено %d wait channels", deletedCount)
	}
}

func (m *Model) cleanupAllResponders() {
	model.CleanupAllRespondersUniversal(
		&m.responders,
		func(val any) {
			if respModel, ok := val.(*RespModel); ok && respModel.Cancel != nil {
				respModel.Cancel()
			}
		},
		func(val any) {
			if respModel, ok := val.(*RespModel); ok {
				m.closeResponderChannels(respModel)
			}
		},
	)
}

// ============================================================================
// CACHE INVALIDATION METHODS
// ============================================================================

// InvalidateUserAgentConfigCache инвалидирует кэш конфигурации модели для пользователя
// Вызывается при обновлении модели чтобы новые сессии получили актуальные настройки
// Удаляет все кэшированные респондентов пользователя из m.responders
func (m *Model) InvalidateUserAgentConfigCache(userID uint32) {
	var invalidatedCount int
	m.responders.Range(func(key, value any) bool {
		respModel := value.(*RespModel)
		if respModel.Assist.UserID == userID {
			// Удаляем кэшированный респондент для этого пользователя
			m.responders.Delete(key)
			invalidatedCount++
		}
		return true // продолжаем итерацию
	})

	if invalidatedCount > 0 {
		//logger.Debug("Инвалидирован кэш конфигурации модели для userID=%d (удалено %d респондентов)", userID, invalidatedCount, userID)
	}
}

// DisconnectUser выполняет graceful завершение всех активных сессий пользователя:
// 1. Закрывает все realtime-сессии (WebSocket + каналы)
// 2. Отменяет контексты всех респондентов
// 3. Удаляет респондентов из кэша
func (m *Model) DisconnectUser(userID uint32) {
	// Шаг 1: закрываем realtime-сессии пользователя
	m.realtimeSessions.Range(func(key, value any) bool {
		rs := value.(*RealtimeSession)
		if rs.userID == userID {
			respId := key.(uint64)
			m.CloseRealtimeSession(respId)
		}
		return true
	})

	// Шаг 2: отменяем контексты и удаляем респондентов
	m.responders.Range(func(key, value any) bool {
		respModel := value.(*RespModel)
		if respModel.Assist.UserID == userID {
			if respModel.Cancel != nil {
				respModel.Cancel()
			}
			m.responders.Delete(key)
		}
		return true
	})
}

// ============================================================================
// NEW METHOD
// ============================================================================

func (m *Model) saveAllContextsGracefullyCtx(_ context.Context) error {
	// В новом подходе с Chat Completions API нет thread_id для сохранения
	// История диалога сохраняется через dialog.Dialog в БД автоматически
	// Этот метод оставлен для совместимости, но не выполняет действий
	//logger.Debug("saveAllContextsGracefullyCtx: пропускаем (Chat Completions API не использует threads)")
	return nil
}

func (m *Model) UpdateModelsListByProvider(ctx context.Context, provider create.ProviderType, apiKey string) error {
	if provider != create.ProviderOpenAI {
		return fmt.Errorf("неверный провайдер для OpenAI модели: %s", provider)
	}
	return provider_catalog.SyncProviderModels(ctx, m.db, create.ProviderOpenAI, apiKey)
}
