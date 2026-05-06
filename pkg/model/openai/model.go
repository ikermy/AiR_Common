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

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

type DB = comdb.Exterior

// OpenAIModel управляет OpenAI моделями и респондентами
type OpenAIModel struct {
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
	universalModel   *create.UniversalModel // Для доступа к GetRealUserID
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
	AgentConfig *OpenAIAgentConfig
}

// GetChannel реализует интерфейс model.ChannelProvider
func (r *RespModel) GetChannel() *model.Ch {
	return r.Chan
}

// GetChannelMap реализует интерфейс model.ChannelProvider
func (r *RespModel) GetChannelMap() map[uint64]*model.Ch {
	return r.ChanMap
}

// OpenAIAgentConfig хранит конфигурацию агента для OpenAI модели
// В отличие от Assistants API, конфигурация хранится в БД и передается с каждым запросом
type OpenAIAgentConfig struct {
	ModelId        uint64                 `json:"model_id"`   // ID модели в БД
	ModelName      string                 `json:"model_name"` // Имя модели OpenAI (gpt-4o-mini и т.д.)
	SystemPrompt   string                 `json:"system_prompt"`
	Tools          []interface{}          `json:"tools"`
	ResponseFormat map[string]interface{} `json:"response_format"`
	VectorStoreIds []string               `json:"vector_store_ids,omitempty"`
	FileIds        []interface{}          `json:"file_ids,omitempty"`

	// Дополнительные возможности
	Search      bool   `json:"search"`       // Поиск по векторному хранилищу
	Interpreter bool   `json:"interpreter"`  // Code Interpreter
	S3          bool   `json:"s3"`           // S3 хранилище
	Haunter     bool   `json:"haunter"`      // Модель для поиска лидов
	Operator    bool   `json:"operator"`     // Вызов оператора
	MetaAction  string `json:"meta_action"`  // Целевое действие
	HasCalendar bool   `json:"has_calendar"` // Google Calendar
	HasSheets   bool   `json:"has_sheets"`   // Google Sheets
	WebSearch   bool   `json:"web_search"`   // Веб-поиск
	Image       bool   `json:"image"`        // Генерация изображений

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
	realUserID  uint64
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
	Role       string        `json:"role"`    // "system", "user", "assistant"
	Content    interface{}   `json:"content"` // string или массив content parts
	Name       string        `json:"name,omitempty"`
	ToolCalls  []interface{} `json:"tool_calls,omitempty"`
	ToolCallId string        `json:"tool_call_id,omitempty"`
}

type Services struct {
	Listener   atomic.Bool
	Respondent atomic.Bool
}

// New создаёт новый экземпляр OpenAIModel
func New(parent context.Context, conf *conf.Conf, d DB, actionHandler model.ActionHandler) *OpenAIModel {
	ctx, cancel := context.WithCancel(parent)

	// Создаём OpenAI клиент с API ключом через конструктор
	openaiClient := create.NewOpenAIAgentClient(ctx, conf.GPT.OpenAIKey)

	m := &OpenAIModel{
		ctx:           ctx,
		cancel:        cancel,
		client:        openaiClient,
		db:            d,
		responders:    sync.Map{},
		waitChannels:  sync.Map{},
		dialogCache:   sync.Map{},
		UserModelTTl:  time.Duration(conf.GLOB.UserModelTTl) * time.Minute,
		actionHandler: actionHandler,
	}

	// Запускаем periodicFlush в фоновой горутине для очистки истекших диалогов из кэша
	go m.periodicFlush()

	return m
}

// NewAsRouterOption создаёт OpenAI модель и возвращает её как опцию для ModelRouter
// Использование: router := model.NewModelRouter(ctx, conf, db, openai.NewAsRouterOption())
func NewAsRouterOption() model.RouterOption {
	return func(r *model.ModelRouter, ctx context.Context, cfg *conf.Conf, db model.DB) error {
		openaiDB, ok := db.(DB)
		if !ok {
			return fmt.Errorf("DB не соответствует интерфейсу openai.DB")
		}

		// Создаём универсальный обработчик функций
		actionHandler := model.NewUniversalActionHandler(ctx, openaiDB, cfg)

		// Создаём OpenAI модель (клиент уже инициализирован в New)
		openaiModel := New(ctx, cfg, openaiDB, actionHandler)

		// Создаём UniversalModel для управления моделями
		universalModel := create.New(ctx, openaiDB, cfg)

		// Устанавливаем связь с universalModel
		openaiModel.SetUniversalModel(universalModel)

		// Регистрируем модель в роутере
		return model.WithOpenAIModel(openaiModel)(r, ctx, cfg, db)
	}
}

// SetUniversalModel устанавливает UniversalModel для доступа к GetRealUserID
func (m *OpenAIModel) SetUniversalModel(um *create.UniversalModel) {
	m.universalModel = um
}

// Реализация интерфейса model.Inter
func (m *OpenAIModel) NewMessage(operator model.Operator, msgType string, content *model.AssistResponse, name *string, files ...model.FileUpload) model.Message {
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

func (m *OpenAIModel) GetFileAsReader(userId uint32, url string) (io.Reader, error) {
	if url == "" {
		return nil, fmt.Errorf("не указан источник файла: отсутствуют URL")
	}

	if strings.HasPrefix(url, "openai_file:") {
		fileID := strings.TrimPrefix(url, "openai_file:")
		content, err := m.client.DownloadFileContent(m.ctx, fileID, userId)
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

func (m *OpenAIModel) GetOrSetRespGPT(assist model.Assistant, dialogID, respId uint64, respName string) (*model.RespModel, error) {
	// Сначала проверяем кэш
	// Используем respId как ключ
	if val, ok := m.responders.Load(respId); ok {
		respModel := val.(*RespModel)
		respModel.TTL = time.Now().Add(m.UserModelTTl) // Обновляем TTL

		// АВТОМАТИЧЕСКАЯ предзагрузка истории диалога (если кэш пустой)
		m.preloadDialogHistoryIfNeeded(dialogID, assist.UserId)

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
	agentConfig, haunter, err := m.loadAgentConfig(assist.UserId, user)
	if err != nil {
		//logger.Warn("Ошибка загрузки конфигурации агента: %v, используем конфигурацию по умолчанию", err, assist.UserId)
	} else {
		user.AgentConfig = agentConfig
		user.Haunter = haunter
	}

	// Используем respId как ключ
	m.responders.Store(respId, user)

	// Уведомляем ожидающие горутины о создании респондента
	model.NotifyWaitChannels(&m.waitChannels, respId)

	// АВТОМАТИЧЕСКАЯ предзагрузка истории диалога для нового респондента
	m.preloadDialogHistoryIfNeeded(dialogID, assist.UserId)

	return m.convertToModelRespModel(user), nil
}

func (m *OpenAIModel) GetCh(respId uint64) (*model.Ch, error) {
	return model.GetChannel(
		respId,
		m.ctx,
		&m.waitChannels,
		&m.responders,
		func(val interface{}) (*model.Ch, error) {
			respModel := val.(*RespModel)
			return model.ExtractChannelWithPriority(respModel)
		},
	)
}

func (m *OpenAIModel) getTryCh(respId uint64) (*model.Ch, error) {
	return model.GetChannel(
		respId,
		m.ctx,
		&m.waitChannels,
		&m.responders,
		func(val interface{}) (*model.Ch, error) {
			respModel := val.(*RespModel)
			return model.ExtractChannelWithPriority(respModel)
		},
	)
}

func (m *OpenAIModel) GetRespIdBydialogID(dialogID uint64) (uint64, error) {
	return model.GetRespIdBydialogIDUniversal(dialogID, &m.responders)
}

// ============================================================================
// AGENT CONFIG METHODS
// ============================================================================

// loadAgentConfig загружает конфигурацию агента для OpenAI модели из БД
// По образцу Google провайдера - конфигурация хранится в БД, а не в OpenAI API
// Возвращает конфигурацию агента и haunter флаг явно
func (m *OpenAIModel) loadAgentConfig(userId uint32, _ *RespModel) (*OpenAIAgentConfig, bool, error) {
	// Получаем все модели пользователя
	userModels, err := m.db.GetAllUserModels(userId)
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
		return nil, false, fmt.Errorf("модель OpenAI не найдена для userId %d", userId)
	}

	// Инициализируем базовую конфигурацию
	// ModelName берем из AssistId (имя GPT-модели OpenAI, например "gpt-4o-mini")
	// По аналогии с Google провайдером
	agentConfig := &OpenAIAgentConfig{
		ModelId:   found.ModelId,
		ModelName: found.AssistId,
	}

	// Устанавливаем значение по умолчанию если AssistId пустой
	if agentConfig.ModelName == "" {
		agentConfig.ModelName = "gpt-4o-mini"
		//logger.Warn("AssistId пустой, используется модель по умолчанию: gpt-4o-mini", userId)
	}

	var haunter bool

	// Загружаем полные данные модели из БД
	compressedData, _, err := m.db.ReadUserModelByProvider(userId, create.ProviderOpenAI)
	if err != nil {
		//logger.Warn("Ошибка чтения данных модели из БД: %v, используем конфигурацию по умолчанию", err, userId)
	} else if compressedData != nil {
		// Распаковываем полные данные модели для получения параметров
		if m.universalModel != nil {
			modelData, decompressErr := m.universalModel.DecompressModelData(compressedData, nil)
			if decompressErr != nil {
				//logger.Warn("Ошибка распаковки данных модели: %v", decompressErr, userId)
			} else {
				agentConfig.MetaAction = modelData.MetaAction
				agentConfig.Haunter = modelData.Haunter
				agentConfig.Search = modelData.Search
				agentConfig.Operator = modelData.Operator
				agentConfig.S3 = modelData.S3
				agentConfig.Interpreter = modelData.Interpreter
				agentConfig.HasCalendar = modelData.GOAuth.Calendar
				agentConfig.HasSheets = modelData.GOAuth.Sheets
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
			// Конвертируем []Ids в []interface{}
			agentConfig.FileIds = make([]interface{}, len(found.FileIds))
			for i, fileId := range found.FileIds {
				agentConfig.FileIds[i] = fileId
			}
		}
	}

	// Формируем system_prompt, tools и response_format динамически
	if err := m.buildAgentConfiguration(userId, agentConfig, compressedData); err != nil {
		//logger.Warn("Ошибка формирования конфигурации агента: %v", err, userId)
	}

	//logger.Debug("Загружена конфигурация агента (GPT Model: %s, AssistName: %s)", agentConfig.ModelName, respModel.Assist.AssistName, userId)
	return agentConfig, haunter, nil
}

// buildAgentConfiguration формирует system_prompt, tools и response_format
// По образцу Google провайдера - вся конфигурация в памяти
func (m *OpenAIModel) buildAgentConfiguration(userId uint32, config *OpenAIAgentConfig, compressedData []byte) error {
	// Получаем real_user_id
	var realUserID uint64
	if m.universalModel != nil {
		var err error
		realUserID, err = m.universalModel.GetRealUserID(userId)
		if err != nil {
			//logger.Warn("Не удалось получить real_user_id: %v", err, userId)
			realUserID = uint64(userId)
		}
	} else {
		realUserID = uint64(userId)
	}

	// Распаковываем данные модели
	modelData, err := m.universalModel.DecompressModelData(compressedData, nil)
	if err != nil {
		return fmt.Errorf("ошибка распаковки: %w", err)
	}

	// Используем компактный формат, убираем повторения, сокращаем текст
	systemPrompt := modelData.Prompt + "\n\n"

	// Используем UID как аббревиатуру (вместо повторения user_id 4 раза)
	systemPrompt += fmt.Sprintf("UID=%d. Time: get_current_time(UID)\n", realUserID)

	// Компактная форма defaults (вместо длинных блоков ##ВАЖНО)
	systemPrompt += "JSON: target=false"
	if config.Operator {
		systemPrompt += ", operator=false (op=true if ask)"
	}
	systemPrompt += "\n"

	// Условие для target (если есть) - короткая форма
	if config.MetaAction != "" {
		systemPrompt += fmt.Sprintf("target=true: %s\n", config.MetaAction)
	}

	// Компактный список доступных tools через запятую (вместо отдельных блоков ##)
	var availableTools []string
	if config.S3 {
		availableTools = append(availableTools, "S3")
	}
	if config.Interpreter {
		availableTools = append(availableTools, "Py")
	}
	if config.HasCalendar {
		availableTools = append(availableTools, "Cal")
	}
	if config.HasSheets {
		availableTools = append(availableTools, "Sheets")
	}
	if config.WebSearch {
		availableTools = append(availableTools, "Web")
	}

	if len(availableTools) > 0 {
		systemPrompt += fmt.Sprintf("Tools: %s\n", strings.Join(availableTools, ","))
	}

	// Добавляем детальные инструкции для Google Sheets
	if config.HasSheets {
		systemPrompt += fmt.Sprintf("\nSheets: CALL sheets_read_range(UID=%d, spreadsheet_id, range)\n", realUserID) +
			"DON'T say 'cannot get' - CALL function!\n" +
			"spreadsheet_id find in prompt or user request.\n" +
			"Row count: call sheets_read_range → calc len(values)-1\n" +
			"IMPORTANT: Show table data in MESSAGE text, NOT in files! DON'T create files with table data!\n"
	}

	// Добавляем инструкции для Calendar если включен
	if config.HasCalendar {
		systemPrompt += fmt.Sprintf("\nCal: get_current_time → calendar_list_events/create/delete (UID=%d)\n", realUserID)
	}

	// Короткая инструкция по send_files (самое важное без избыточных объяснений)
	systemPrompt += "\nsend_files=[] (S3 only)\nReturn: valid JSON"

	// TODO паралельный вызов функций почему то не работает.. возможно не те функции
	// ВАЖНО: Инструкция о параллельных вызовах функций (для ускорения)
	//systemPrompt += "\n\n⚡ PARALLEL CALLS: Call multiple independent functions SIMULTANEOUSLY in one turn when possible!\n" +
	//	"Example: User asks about calendar events → call get_current_time AND calendar_list_events TOGETHER (not one by one)!"

	// КРИТИЧНО: Запрет создавать файлы с данными таблиц (всегда, независимо от инструментов)
	if config.HasSheets {
		systemPrompt += "\nTable data -> show in message text, NOT create files!"
	}

	// КРИТИЧНО: Разделение инструментов для создания файлов
	if config.S3 && config.Interpreter {
		// Оба инструмента
		systemPrompt += "\n\nINSTRUMENTS:\n" +
			"create_file - for user files in send_files\n" +
			"python tool - ONLY calculations/graphs, NOT user files!\n" +
			"User 'create file' -> create_file, NOT python!"
	} else if config.S3 {
		systemPrompt += "\n\nFile creation: use create_file function!"
	} else if config.Interpreter {
		systemPrompt += "\n\nPython tool: calculations only, NOT for creating user files!"
	}

	// КРИТИЧНО: Глобальная инструкция об использовании результатов функций
	if config.S3 {
		systemPrompt += "\n\nIMPORTANT: After calling functions (create_file, get_s3_files) use their results in final JSON response! Function results contain ready data for send_files (file_name, Url, type) - DO NOT IGNORE!"
	}

	config.SystemPrompt = systemPrompt

	// Формируем tools
	// ВАЖНО: Responses API поддерживает ВСЕ типы tools!
	// file_search, code_interpreter, web_search, function - все работают!
	var tools []interface{}
	userIDStr := fmt.Sprintf("%d", realUserID)

	// ПРИМЕЧАНИЕ: file_search tool больше НЕ используется для OpenAI
	// Семантический поиск теперь выполняется через RAG с локальными эмбеддингами:
	// 1. Генерация эмбеддинга запроса (OpenAI Embeddings API)
	// 2. Поиск похожих документов в MariaDB (VEC_Distance_Cosine)
	// 3. Добавление контекста в system_prompt перед вызовом модели
	// Реализация RAG: openai/request.go или openai/manager.go

	// Code Interpreter - выполнение Python кода
	// ВАЖНО: Responses API требует поле "container" для code_interpreter!
	if config.Interpreter {
		tools = append(tools, map[string]interface{}{
			"type": "code_interpreter",
			"container": map[string]interface{}{
				"type":         "auto", // auto - автоматическое создание/переиспользование контейнера
				"memory_limit": "1g",   // 1g (по умолчанию), 4g, 16g, или 64g
			},
		})
	}

	// Web VSearch - поиск актуальной информации в интернете
	if config.WebSearch {
		tools = append(tools, map[string]interface{}{
			"type": "web_search",
		})
	}

	// get_current_time - обязательная функция для работы с временем
	tools = append(tools, map[string]interface{}{
		"type": "function",
		"name": "get_current_time",
		"description": "Get EXACT current server time and date in user's timezone. " +
			"MUST use this function BEFORE any date calculations.",
		"strict": true, // Строгий режим - обязательное соответствие схеме
		"parameters": map[string]interface{}{
			"type":                 "object",
			"properties":           map[string]interface{}{"user_id": map[string]interface{}{"type": "string", "const": userIDStr}},
			"required":             []string{"user_id"},
			"additionalProperties": false, // Запрет дополнительных полей (требование strict mode)
		},
	})

	if config.S3 {
		tools = append(tools,
			map[string]interface{}{
				"type": "function",
				"name": "get_s3_files",
				"description": "Get list of user's available files from S3 storage. " +
					"Returns array of objects with fields: file_name, Url (full URL), type (file type).",
				"strict": true,
				"parameters": map[string]interface{}{
					"type":                 "object",
					"properties":           map[string]interface{}{"user_id": map[string]interface{}{"type": "string", "const": userIDStr}},
					"required":             []string{"user_id"},
					"additionalProperties": false,
				},
			},
			map[string]interface{}{
				"type": "function",
				"name": "create_file",
				"description": "PRIMARY function for creating files (txt, pdf, doc, etc). " +
					"When user asks to create or send file - call this function IMMEDIATELY! " +
					"Returns object {file_name: string, Url: string, type: string}. " +
					"WORKFLOW: " +
					"1) Call create_file with file content " +
					"2) Get result with fields file_name, Url, type " +
					"3) MUST use these fields in action.send_files of final response! " +
					"EXAMPLE RESULT: {\"file_name\":\"story.pdf\", \"Url\":\"https://...\", \"type\":\"doc\"} " +
					"DO NOT IGNORE function result - it must go to send_files!",
				"strict": true,
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id":   map[string]interface{}{"type": "string", "const": userIDStr},
						"content":   map[string]interface{}{"type": "string", "description": "File content"},
						"file_name": map[string]interface{}{"type": "string", "description": "File name (e.g.: story.txt, report.pdf)"},
					},
					"required":             []string{"user_id", "content", "file_name"},
					"additionalProperties": false,
				},
			},
		)
	}

	if config.HasCalendar {
		tools = append(tools,
			map[string]interface{}{
				"type": "function",
				"name": "calendar_create_event",
				"description": "Create new event in user's Google Calendar. " +
					"Time format: RFC3339 with timezone (e.g.: 2026-02-15T14:00:00-03:00). " +
					"MUST call get_current_time BEFORE date calculations!",
				"strict": true,
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id":    map[string]interface{}{"type": "string", "const": userIDStr},
						"title":      map[string]interface{}{"type": "string", "description": "Event title"},
						"start_time": map[string]interface{}{"type": "string", "description": "Start time (RFC3339)"},
						"end_time":   map[string]interface{}{"type": "string", "description": "End time (RFC3339)"},
					},
					"required":             []string{"user_id", "title", "start_time", "end_time"},
					"additionalProperties": false,
				},
			},
			map[string]interface{}{
				"type":        "function",
				"name":        "calendar_list_events",
				"description": "Get list of events from user's Google Calendar.",
				"strict":      true,
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id":     map[string]interface{}{"type": "string", "const": userIDStr},
						"time_min":    map[string]interface{}{"type": []string{"string", "null"}, "description": "Period start (RFC3339)"},
						"time_max":    map[string]interface{}{"type": []string{"string", "null"}, "description": "Period end (RFC3339)"},
						"max_results": map[string]interface{}{"type": []string{"integer", "null"}, "description": "Max events count"},
					},
					"required":             []string{"user_id", "time_min", "time_max", "max_results"},
					"additionalProperties": false,
				},
			},
			map[string]interface{}{
				"type":        "function",
				"name":        "calendar_delete_event",
				"description": "Delete event from user's Google Calendar.",
				"strict":      true,
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id":  map[string]interface{}{"type": "string", "const": userIDStr},
						"event_id": map[string]interface{}{"type": "string", "description": "Event ID to delete"},
					},
					"required":             []string{"user_id", "event_id"},
					"additionalProperties": false,
				},
			},
			map[string]interface{}{
				"type":        "function",
				"name":        "calendar_get_event",
				"description": "Get event details from user's Google Calendar.",
				"strict":      true,
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id":  map[string]interface{}{"type": "string", "const": userIDStr},
						"event_id": map[string]interface{}{"type": "string", "description": "Event ID"},
					},
					"required":             []string{"user_id", "event_id"},
					"additionalProperties": false,
				},
			},
		)
	}

	if config.HasSheets {
		tools = append(tools,
			map[string]interface{}{
				"type": "function",
				"name": "sheets_read_range",
				"description": "Read data from user's Google Sheets table. " +
					"Returns array of rows with data from specified range.",
				"strict": true,
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
						"spreadsheet_id": map[string]interface{}{"type": "string", "description": "Table ID from URL"},
						"range":          map[string]interface{}{"type": "string", "description": "Range (e.g.: Sheet1!A:F)"},
					},
					"required":             []string{"user_id", "spreadsheet_id", "range"},
					"additionalProperties": false,
				},
			},
		)
	}

	config.Tools = tools

	// Формируем response format с динамической схемой
	hasMetaAction := config.MetaAction != ""
	hasOperator := config.Operator
	dynamicSchema := create.GenerateModelSchema(hasMetaAction, hasOperator)

	config.ResponseFormat = map[string]interface{}{
		"type": "json_schema",
		"json_schema": map[string]interface{}{
			"name":   "assist_response",
			"strict": true,
			"schema": dynamicSchema,
		},
	}

	config.RealtimeModel = create.RealtimeDefaultModel

	// Передаём RealtimeVAD конфигурацию из распакованных данных модели
	// (с уже применёнными дефолтными значениями из DecompressModelData)
	config.RealtimeVAD = modelData.RealtimeVAD

	return nil
}

// ============================================================================
// DIALOG CACHE METHODS
// ============================================================================

// periodicFlush периодически очищает истекшие записи из dialogCache
func (m *OpenAIModel) periodicFlush() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			flushedCount := 0

			m.dialogCache.Range(func(key, value interface{}) bool {
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
func (m *OpenAIModel) getOrCreateDialogCache(dialogID uint64) *DialogCache {
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
func (m *OpenAIModel) getDialogHistoryFromCache(dialogID uint64) ([]ChatMessage, bool) {
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
func (m *OpenAIModel) addMessageToCache(dialogID uint64, message ChatMessage) {
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
func (m *OpenAIModel) preloadDialogHistoryIfNeeded(dialogID uint64, _ uint32) {
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

func (m *OpenAIModel) SaveAllContextDuringExit() {
	// В новом подходе с Chat Completions API история сохраняется автоматически через БД
	//logger.Info("SaveAllContextDuringExit: пропускаем (Chat Completions API не требует сохранения контекста)")
}

func (m *OpenAIModel) CleanDialogData(dialogID uint64) {
	// Получаем respId по dialogID
	respId, err := m.GetRespIdBydialogID(dialogID)
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

func (m *OpenAIModel) DeleteTempFile(fileID string) error {
	// Удаляем временный файл из OpenAI
	if err := m.client.DeleteFile(m.ctx, fileID); err != nil {
		return fmt.Errorf("ошибка удаления временного файла: %w", err)
	}
	return nil
}

func (m *OpenAIModel) TranscribeAudio(userId uint32, audioData []byte, fileName string) (string, error) {
	// Используем существующий метод OpenAIAgentClient.TranscribeAudio
	if m.client == nil {
		return "", fmt.Errorf("OpenAI клиент не инициализирован")
	}

	text, err := m.client.TranscribeAudio(m.ctx, audioData, fileName, userId)
	if err != nil {
		return "", fmt.Errorf("ошибка транскрипции аудио: %w", err)
	}

	return text, nil
}

func (m *OpenAIModel) Shutdown(shutCh chan<- map[string]any) {
	var shutdownErrors []string

	m.shutdownOnce.Do(func() {
		shutCh <- map[string]any{"msg": "начало shutdown",
			"mod":  "OpenAIModel",
			"type": 0, // 0 - Info
			"uid":  0}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		shutCh <- map[string]any{"msg": "сохранение всех контекстов при завершении работы",
			"mod":  "OpenAIModel",
			"type": 0, // 0 - Info
			"uid":  0}
		if err := m.saveAllContextsGracefullyCtx(shutdownCtx); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Sprintf("ошибка сохранения контекстов: %v", err))
		}

		// Закрываем все активные Realtime-сессии
		m.realtimeSessions.Range(func(key, value interface{}) bool {
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

		shutCh <- map[string]any{"msg": "процесс завершения работы модуля завершен",
			"mod":  "OpenAIModel",
			"type": 0, // 0 - Info
			"uid":  0}
	})

	if len(shutdownErrors) > 0 {

		shutCh <- map[string]any{"msg": fmt.Sprintf("ошибки при завершении работы: %s", strings.Join(shutdownErrors, "; ")),
			"mod":  "OpenAIModel",
			"type": 2, // 2 - Error
			"uid":  0}
	}

	shutCh <- map[string]any{"msg": "модуль успешно завершил работу",
		"mod":  "OpenAIModel",
		"type": 0, // 0 - Info
		"uid":  0}
}

// Вспомогательная функция для конвертации внутреннего RespModel в model.RespModel
func (m *OpenAIModel) convertToModelRespModel(internal *RespModel) *model.RespModel {
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
func (m *OpenAIModel) CleanUp() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			deletedRespCount := 0
			checkedRespCount := 0

			m.responders.Range(func(key, value interface{}) bool {
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

func (m *OpenAIModel) closeResponderChannels(respModel *RespModel) {
	model.CloseResponderChannelsUniversal(respModel)
}

func (m *OpenAIModel) cleanupWaitChannels() {
	deletedCount := model.CleanupWaitChannelsUniversal(&m.waitChannels, &m.responders)
	if deletedCount > 0 {
		//logger.Debug("Очищено %d wait channels", deletedCount)
	}
}

func (m *OpenAIModel) cleanupAllResponders() {
	model.CleanupAllRespondersUniversal(
		&m.responders,
		func(val interface{}) {
			if respModel, ok := val.(*RespModel); ok && respModel.Cancel != nil {
				respModel.Cancel()
			}
		},
		func(val interface{}) {
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
func (m *OpenAIModel) InvalidateUserAgentConfigCache(userId uint32) {
	var invalidatedCount int
	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)
		if respModel.Assist.UserId == userId {
			// Удаляем кэшированный респондент для этого пользователя
			m.responders.Delete(key)
			invalidatedCount++
		}
		return true // продолжаем итерацию
	})

	if invalidatedCount > 0 {
		//logger.Debug("Инвалидирован кэш конфигурации модели для userId=%d (удалено %d респондентов)", userId, invalidatedCount, userId)
	}
}

// ============================================================================
// NEW METHOD
// ============================================================================

func (m *OpenAIModel) saveAllContextsGracefullyCtx(_ context.Context) error {
	// В новом подходе с Chat Completions API нет thread_id для сохранения
	// История диалога сохраняется через dialog.Dialog в БД автоматически
	// Этот метод оставлен для совместимости, но не выполняет действий
	//logger.Debug("saveAllContextsGracefullyCtx: пропускаем (Chat Completions API не использует threads)")
	return nil
}
