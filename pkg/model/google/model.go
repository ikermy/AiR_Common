package google

import (
	"context"
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
	ctx            context.Context
	cancel         context.CancelFunc
	client         *create.GoogleAgentClient
	db             DB
	responders     sync.Map // respId -> *GoogleRespModel
	waitChannels   sync.Map
	dialogCache    sync.Map // dialogId -> *DialogCache (локальный кэш истории диалогов)
	UserModelTTl   time.Duration
	actionHandler  model.ActionHandler
	universalModel *create.UniversalModel // Для доступа к GetRealUserID
	shutdownOnce   sync.Once
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
	Image      bool   `json:"image"`       // Генерация изображений (Imagen 3)
	WebSearch  bool   `json:"web_search"`  // Веб-поиск (google_search)
	Video      bool   `json:"video"`       // Генерация видео (Google Veo)
	Haunter    bool   `json:"haunter"`     // Модель используется для поиска лидов
	Search     bool   `json:"search"`      // Поиск по векторному хранилищу (эмбеддингам в MariaDB)
	Operator   bool   `json:"operator"`    // Вызов оператора включён
	MetaAction string `json:"meta_action"` // Целевое действие модели

	// Флаги для Google Services
	S3          bool `json:"s3"`           // S3 хранилище
	Interpreter bool `json:"interpreter"`  // Code Interpreter
	HasCalendar bool `json:"has_calendar"` // Google Calendar доступен
	HasSheets   bool `json:"has_sheets"`   // Google Sheets доступен
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
		googleDB, ok := db.(DB)
		if !ok {
			return fmt.Errorf("DB не соответствует интерфейсу google.DB")
		}

		// Создаём ActionHandler с Google OAuth конфигом из cfg
		actionHandler := model.NewUniversalActionHandler(ctx, googleDB, cfg)

		googleModel := New(ctx, cfg, googleDB, actionHandler)

		return model.WithGoogleModel(googleModel)(r, ctx, cfg, db)
	}
}

// SetClient устанавливает GoogleAgentClient (вызывается из universalModel)
func (m *GoogleModel) SetClient(client *create.GoogleAgentClient) {
	m.client = client
}

// SetUniversalModel устанавливает UniversalModel для доступа к GetRealUserID
func (m *GoogleModel) SetUniversalModel(um *create.UniversalModel) {
	m.universalModel = um
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

	logger.Debug("Создан новый Google респондент для dialogId %d", dialogId)

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
	var found *create.UserModelRecord
	for i := range userModels {
		if userModels[i].Provider == create.ProviderGoogle {
			found = &userModels[i]
			break
		}
	}

	if found == nil {
		return fmt.Errorf("модель Google не найдена для userId %d", userId)
	}

	// Инициализируем базовую конфигурацию
	agentConfig := GoogleAgentConfig{
		ModelId:   found.ModelId,
		ModelName: found.AssistId,
		HasVector: false,
	}

	// Загружаем полные данные модели из БД для получения всех параметров
	compressedData, _, err := m.db.ReadUserModelByProvider(userId, create.ProviderGoogle)
	if err != nil {
		logger.Warn("Ошибка чтения данных модели из БД: %v, используем конфигурацию по умолчанию", err, userId)
	} else if compressedData != nil {
		// Используем функцию из пакета db для распаковки и извлечения всех параметров
		metaAction, _, _, image, webSearch, video, haunter, search, operator, s3, interpreter, calendar, sheets, extractErr := comdb.DecompressAndExtractMetadata(compressedData)
		if extractErr != nil {
			logger.Warn("Ошибка распаковки параметров модели: %v", extractErr, userId)
		} else {
			agentConfig.Image = image
			agentConfig.WebSearch = webSearch
			agentConfig.Video = video
			agentConfig.Haunter = haunter
			agentConfig.Search = search
			agentConfig.Operator = operator
			agentConfig.MetaAction = metaAction
			agentConfig.S3 = s3
			agentConfig.Interpreter = interpreter
			agentConfig.HasCalendar = calendar
			agentConfig.HasSheets = sheets
		}
	}

	// Формируем массив Tools на основе загруженных параметров
	// ВАЖНО: WebSearch (google_search) добавляем только если он включен
	if agentConfig.WebSearch {
		agentConfig.Tools = append(agentConfig.Tools, map[string]interface{}{
			"google_search": map[string]interface{}{},
		})
	}

	// Получаем real_user_id для использования в функциях
	// КРИТИЧЕСКИ ВАЖНО: без real_user_id функции S3, Calendar и Sheets работать не будут!
	var realUserID uint64
	var hasRealUserID bool

	logger.Debug("[USER:%d] Проверка universalModel: установлен=%v", userId, m.universalModel != nil)

	if m.universalModel != nil {
		var err error
		realUserID, err = m.universalModel.GetRealUserID(userId)
		if err != nil {
			logger.Error("Не удалось получить real_user_id для userId=%d: %v. Функции S3, Calendar и Sheets будут ОТКЛЮЧЕНЫ!", userId, err, userId)
			hasRealUserID = false
		} else {
			hasRealUserID = true
		}
	} else {
		logger.Warn("UniversalModel не установлен для Google модели! Функции S3, Calendar и Sheets будут ОТКЛЮЧЕНЫ!", userId)
		hasRealUserID = false
	}

	// Формируем function_declarations для различных сервисов
	var functionDeclarations []map[string]interface{}

	// 1. Добавляем S3 функции ТОЛЬКО если есть real_user_id
	if agentConfig.S3 && hasRealUserID {
		functionDeclarations = append(functionDeclarations,
			map[string]interface{}{
				"name":        "get_s3_files",
				"description": fmt.Sprintf("Получает список файлов пользователя из S3. ВАЖНО: user_id должен быть СТРОКОЙ \"%d\"", realUserID),
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserID),
						},
					},
					"required": []string{"user_id"},
				},
			},
			map[string]interface{}{
				"name":        "create_file",
				"description": "Создает новый файл в S3 хранилище пользователя",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserID),
						},
						"content": map[string]interface{}{
							"type":        "string",
							"description": "Содержимое файла",
						},
						"file_name": map[string]interface{}{
							"type":        "string",
							"description": "Имя файла с расширением",
						},
					},
					"required": []string{"user_id", "content", "file_name"},
				},
			},
		)
	}

	// 1.5. Добавляем функцию получения текущего времени (всегда доступна)
	if hasRealUserID {
		functionDeclarations = append(functionDeclarations,
			map[string]interface{}{
				"name": "get_current_time",
				"description": "Получает ТОЧНОЕ текущее время и дату с сервера в часовом поясе пользователя. " +
					"ОБЯЗАТЕЛЬНО используй эту функцию ПЕРЕД расчётом дат (завтра, через неделю, в понедельник и т.д.). " +
					"НЕ используй свои внутренние знания о дате - они УСТАРЕЛИ!",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserID),
						},
					},
					"required": []string{"user_id"},
				},
			},
		)
	}

	// 2. Добавляем Google Calendar функции ТОЛЬКО если есть real_user_id
	if agentConfig.HasCalendar && hasRealUserID {
		functionDeclarations = append(functionDeclarations,
			map[string]interface{}{
				"name":        "calendar_create_event",
				"description": "Создает новое событие в Google Calendar пользователя",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserID),
						},
						"title": map[string]interface{}{
							"type":        "string",
							"description": "Название события",
						},
						"description": map[string]interface{}{
							"type":        "string",
							"description": "Описание события (опционально)",
						},
						"start_time": map[string]interface{}{
							"type":        "string",
							"description": "Время начала в RFC3339 формате (например: '2026-02-04T10:00:00Z')",
						},
						"end_time": map[string]interface{}{
							"type":        "string",
							"description": "Время окончания в RFC3339 формате",
						},
						"location": map[string]interface{}{
							"type":        "string",
							"description": "Место проведения (опционально)",
						},
						"attendees": map[string]interface{}{
							"type":        "array",
							"description": "Email адреса участников (опционально)",
							"items": map[string]interface{}{
								"type": "string",
							},
						},
					},
					"required": []string{"user_id", "title", "start_time", "end_time"},
				},
			},
			map[string]interface{}{
				"name":        "calendar_list_events",
				"description": "Получает список событий из Google Calendar пользователя",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserID),
						},
						"time_min": map[string]interface{}{
							"type":        "string",
							"description": "Начало периода в RFC3339 (опционально, по умолчанию - текущее время)",
						},
						"time_max": map[string]interface{}{
							"type":        "string",
							"description": "Конец периода в RFC3339 (опционально)",
						},
						"max_results": map[string]interface{}{
							"type":        "integer",
							"description": "Максимальное количество событий (по умолчанию 10)",
						},
					},
					"required": []string{"user_id"},
				},
			},
			map[string]interface{}{
				"name":        "calendar_delete_event",
				"description": "Удаляет событие из Google Calendar",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserID),
						},
						"event_id": map[string]interface{}{
							"type":        "string",
							"description": "ID события для удаления",
						},
					},
					"required": []string{"user_id", "event_id"},
				},
			},
		)
	}

	// 3. Добавляем Google Sheets функции ТОЛЬКО если есть real_user_id
	if agentConfig.HasSheets && hasRealUserID {
		functionDeclarations = append(functionDeclarations,
			map[string]interface{}{
				"name": "sheets_read_range",
				"description": "Читает данные из указанного диапазона в Google Sheets. " +
					"КРИТИЧЕСКИ ВАЖНО: spreadsheet_id это ДЛИННАЯ строка (~40 символов) типа '18kxy_zkXIrTIvPkxC1OJx6cKi_dpzDLsAGy4mQHcGYs', " +
					"а НЕ название таблицы ('crm', 'лиды' и т.д.). Найди полный ID в системном промпте (ищи 'spreadsheet_id: \"...\"').",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserID),
						},
						"spreadsheet_id": map[string]interface{}{
							"type": "string",
							"description": "ПОЛНЫЙ ID таблицы Google Sheets из системного промпта (длинная строка ~40 символов, например '18kxy_zkXIrTIvPkxC1OJx6cKi_dpzDLsAGy4mQHcGYs'). " +
								"ЗАПРЕЩЕНО использовать название таблицы ('crm', 'лиды') - только ПОЛНЫЙ ID!",
						},
						"range": map[string]interface{}{
							"type":        "string",
							"description": "Диапазон для чтения (например: 'Лиды!A:F' где 'Лиды' - название листа из системного промпта)",
						},
					},
					"required": []string{"user_id", "spreadsheet_id", "range"},
				},
			},
			map[string]interface{}{
				"name": "sheets_write_range",
				"description": "Записывает данные в указанный диапазон Google Sheets. " +
					"ВАЖНО: используй ПОЛНЫЙ spreadsheet_id из системного промпта, а не название таблицы!",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserID),
						},
						"spreadsheet_id": map[string]interface{}{
							"type": "string",
							"description": "ПОЛНЫЙ ID таблицы из системного промпта (длинная строка ~40 символов). " +
								"НЕ используй название таблицы!",
						},
						"range": map[string]interface{}{
							"type":        "string",
							"description": "Начальная ячейка для записи (например: 'Sheet1!A1')",
						},
						"values": map[string]interface{}{
							"type":        "array",
							"description": "Двумерный массив значений для записи",
							"items": map[string]interface{}{
								"type": "array",
								"items": map[string]interface{}{
									"type": "string",
								},
							},
						},
					},
					"required": []string{"user_id", "spreadsheet_id", "range", "values"},
				},
			},
			map[string]interface{}{
				"name": "sheets_append_range",
				"description": "Добавляет данные в конец таблицы Google Sheets. " +
					"ВАЖНО: используй ПОЛНЫЙ spreadsheet_id из системного промпта, а не название таблицы!",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserID),
						},
						"spreadsheet_id": map[string]interface{}{
							"type": "string",
							"description": "ПОЛНЫЙ ID таблицы из системного промпта (длинная строка ~40 символов). " +
								"НЕ используй название таблицы!",
						},
						"range": map[string]interface{}{
							"type":        "string",
							"description": "Диапазон колонок для добавления (например: 'Sheet1!A:D')",
						},
						"values": map[string]interface{}{
							"type":        "array",
							"description": "Двумерный массив значений для добавления",
							"items": map[string]interface{}{
								"type": "array",
								"items": map[string]interface{}{
									"type": "string",
								},
							},
						},
					},
					"required": []string{"user_id", "spreadsheet_id", "range", "values"},
				},
			},
			map[string]interface{}{
				"name":        "sheets_create_spreadsheet",
				"description": "Создает новую таблицу Google Sheets",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": fmt.Sprintf("ID пользователя (СТРОКА): \"%d\"", realUserID),
						},
						"title": map[string]interface{}{
							"type":        "string",
							"description": "Название новой таблицы",
						},
						"sheet_names": map[string]interface{}{
							"type":        "array",
							"description": "Названия листов (опционально)",
							"items": map[string]interface{}{
								"type": "string",
							},
						},
					},
					"required": []string{"user_id", "title"},
				},
			},
		)
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
	// ВАЖНО: Загружаем эмбеддинги ТОЛЬКО если флаг Search включен
	if agentConfig.Search {
		embeddings, err := m.db.ListModelEmbeddings(found.ModelId)
		if err != nil {
			logger.Warn("Ошибка получения эмбеддингов для modelId=%d: %v", found.ModelId, err, userId)
		} else if len(embeddings) > 0 {
			agentConfig.HasVector = true
			logger.Debug("Найдено %d эмбеддингов в vector_embeddings для modelId=%d", len(embeddings), found.ModelId, userId)

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
			logger.Debug("Search включен для modelId=%d, но эмбеддинги отсутствуют", found.ModelId, userId)
		}
	} else {
		logger.Debug("Search отключен для modelId=%d, пропускаем загрузку эмбеддингов", found.ModelId, userId)
	}

	respModel.AgentConfig = &agentConfig
	respModel.Assist.AssistId = found.AssistId

	// Логируем загруженную конфигурацию для отладки
	logger.Debug("[USER:%d] Загружена конфигурация Google агента: model=%s, tools=%d, WebSearch=%v, S3=%v, Calendar=%v, Sheets=%v, Interpreter=%v, Search=%v, hasVector=%v",
		userId, agentConfig.ModelName, len(agentConfig.Tools),
		agentConfig.WebSearch, agentConfig.S3, agentConfig.HasCalendar, agentConfig.HasSheets,
		agentConfig.Interpreter, agentConfig.Search, agentConfig.HasVector)

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
			logger.Debug("Удален неактивный Google респондент для respId %d", respId)
		}

		return true
	})
}

// Shutdown корректно завершает работу модели
func (m *GoogleModel) Shutdown() {
	m.shutdownOnce.Do(func() {
		logger.Infoln("Начало shutdown для GoogleModel")

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

		logger.Infoln("GoogleModel shutdown завершен")
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
