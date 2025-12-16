package mistral

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
	models "github.com/ikermy/AiR_Common/pkg/model/create"
)

// MistralModel реализует интерфейс model.UniversalModel для работы с Mistral AI
type MistralModel struct {
	ctx           context.Context
	cancel        context.CancelFunc
	client        *MistralAgentClient
	db            model.DialogDB
	dialogSaver   model.DialogSaver
	responders    sync.Map      // map[uint64]*RespModel
	waitChannels  sync.Map      // map[uint64]chan struct{}
	UserModelTTl  time.Duration // Время жизни пользовательской модели в памяти
	actionHandler model.ActionHandler
	shutdownOnce  sync.Once
}

type RespModel struct {
	Ctx      context.Context
	Cancel   context.CancelFunc
	Chan     *model.Ch      // Один канал для этого респондента
	Context  *DialogContext // Один текущий контекст диалога
	TTL      time.Time
	Assist   model.Assistant
	RespName string
	Services Services
}

// DialogContext хранит историю сообщений диалога в памяти
type DialogContext struct {
	Messages []Message
	LastUsed time.Time
}

// Message представляет сообщение в контексте диалога
type Message struct {
	Type      string    `json:"type"`      // "user" или "assistant"
	Content   string    `json:"content"`   // Текст сообщения
	Timestamp time.Time `json:"timestamp"` // Время создания
}

type Services struct {
	Listener   atomic.Bool
	Respondent atomic.Bool
}

// New создает новую модель Mistral
func New(parent context.Context, conf *conf.Conf, db model.DialogDB, actionHandler model.ActionHandler) *MistralModel {
	ctx, cancel := context.WithCancel(parent)

	// Создаем DialogSaver для батчированного сохранения
	dialogSaver := model.NewDialogSaver(ctx, db, mode.BatchSize)

	return &MistralModel{
		ctx:           ctx,
		cancel:        cancel,
		client:        NewMistralAgentClient(parent, conf),
		db:            db,
		dialogSaver:   dialogSaver,
		responders:    sync.Map{},
		waitChannels:  sync.Map{},
		UserModelTTl:  time.Duration(conf.GLOB.UserModelTTl) * time.Minute,
		actionHandler: actionHandler,
	}
}

// NewAsRouterOption создаёт Mistral модель и возвращает её как опцию для ModelRouter
// Использование: router := model.NewModelRouter(ctx, conf, db, mistral.NewAsRouterOption())
func NewAsRouterOption() model.RouterOption {
	return func(r *model.ModelRouter, ctx context.Context, cfg *conf.Conf, db model.DB) error {
		// Создаём универсальный обработчик функций
		actionHandler := &model.UniversalActionHandler{}

		// Приводим DB к типу model.DialogDB через интерфейс
		dialogDB, ok := db.(model.DialogDB)
		if !ok {
			return fmt.Errorf("DB не соответствует интерфейсу model.DialogDB")
		}

		// Создаём Mistral модель с action handler
		mistralModel := New(ctx, cfg, dialogDB, actionHandler)

		// Регистрируем модель в роутере
		return model.WithMistralModel(mistralModel)(r, ctx, cfg, db)
	}
}

// NewMessage создает новое сообщение (реализация model.UniversalModel)
func (m *MistralModel) NewMessage(operator model.Operator, msgType string, content *model.AssistResponse, name *string, files ...model.FileUpload) model.Message {
	return model.Message{
		Operator:  operator,
		Type:      msgType,
		Content:   *content,
		Name:      *name,
		Timestamp: time.Now(),
		Files:     files,
	}
}

// GetFileAsReader загружает файл по URL (реализация model.UniversalModel)
func (m *MistralModel) GetFileAsReader(url string) (io.Reader, error) {
	if url == "" {
		return nil, fmt.Errorf("не указан источник файла: отсутствуют URL")
	}

	req, err := http.NewRequestWithContext(m.ctx, "GET", url, nil)
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

// GetOrSetRespGPT получает или создает RespModel (реализация model.UniversalModel)
func (m *MistralModel) GetOrSetRespGPT(assist model.Assistant, dialogId, respId uint64, respName string) (*model.RespModel, error) {
	// Используем respId как ключ
	if val, ok := m.responders.Load(respId); ok {
		respModel := val.(*RespModel)
		respModel.TTL = time.Now().Add(m.UserModelTTl) // Обновляем TTL при каждом обращении
		return m.convertToModelRespModel(respModel), nil
	}

	userCtx, cancel := context.WithCancel(m.ctx)

	user := &RespModel{
		Assist:   assist,
		RespName: respName,
		TTL:      time.Now().Add(m.UserModelTTl),
		Chan: &model.Ch{
			TxCh:     make(chan model.Message, 1),
			RxCh:     make(chan model.Message, 1),
			UserId:   assist.UserId,
			DialogId: dialogId,
			RespName: respName,
		},
		Context:  nil, // Будет загружено ниже
		Services: Services{},
		Ctx:      userCtx,
		Cancel:   cancel,
	}

	// Загружаем историю диалога из БД ОДИН РАЗ при создании
	dialogContext := &DialogContext{
		Messages: []Message{},
		LastUsed: time.Now(),
	}

	dialogData, err := m.db.ReadDialog(dialogId)
	if err != nil {
		//logger.Debug("Не удалось загрузить историю диалога %d: %v. Начинаем новый диалог.", dialogId, err)
	} else {
		// Конвертируем загруженную историю в формат Message
		for _, msg := range dialogData.Data {
			msgType := "user"
			if msg.Creator == int(model.CreatorAssistant) {
				msgType = "assistant"
			}

			// Пропускаем пустые сообщения ассистента (Mistral API их не принимает)
			if msgType == "assistant" && msg.Message == "" {
				logger.Debug("Пропущено пустое сообщение ассистента при загрузке истории диалога %d", dialogId)
				continue
			}

			// Парсим timestamp
			timestamp := time.Now()
			if msg.Timestamp != "" {
				if t, err := time.Parse(time.RFC3339, msg.Timestamp); err == nil {
					timestamp = t
				}
			}

			dialogContext.Messages = append(dialogContext.Messages, Message{
				Type:      msgType,
				Content:   msg.Message,
				Timestamp: timestamp,
			})
		}
	}

	// Сохраняем контекст
	user.Context = dialogContext

	// Используем respId как ключ (один пользователь может иметь несколько диалогов)
	m.responders.Store(respId, user)

	if waitChIface, exists := m.waitChannels.Load(respId); exists {
		waitCh := waitChIface.(chan struct{})
		close(waitCh)
		m.waitChannels.Delete(respId)
	}

	return m.convertToModelRespModel(user), nil
}

// GetCh получает канал для респондента (реализация model.UniversalModel)
func (m *MistralModel) GetCh(respId uint64) (*model.Ch, error) {
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

func (m *MistralModel) getTryCh(respId uint64) (*model.Ch, error) {
	val, ok := m.responders.Load(respId)
	if !ok {
		return nil, fmt.Errorf("RespModel не найден для respId %d", respId)
	}

	respModel := val.(*RespModel)
	if respModel.Chan == nil {
		return nil, fmt.Errorf("канал не найден для respId %d", respId)
	}

	return respModel.Chan, nil
}

// GetRespIdByDialogId получает ID респондента по ID диалога (реализация model.UniversalModel)
func (m *MistralModel) GetRespIdByDialogId(dialogId uint64) (uint64, error) {
	// Ищем responder по dialogId в канале
	var foundRespId uint64
	found := false

	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		if respModel.Chan != nil && respModel.Chan.DialogId == dialogId {
			respId, ok := key.(uint64)
			if ok {
				foundRespId = respId
				found = true
				return false // Прекращаем поиск
			}
		}
		return true // Продолжаем поиск
	})

	if !found {
		return 0, fmt.Errorf("RespModel не найден для dialogId %d", dialogId)
	}

	return foundRespId, nil
}

// SaveAllContextDuringExit сохраняет контекст при выходе (реализация model.UniversalModel)
func (m *MistralModel) SaveAllContextDuringExit() {
	logger.Info("MistralModel: сохранение всех контекстов диалогов")

	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		if respModel.Context != nil && len(respModel.Context.Messages) > 0 && respModel.Chan != nil {
			dialogId := respModel.Chan.DialogId

			// Конвертируем контекст обратно в формат DialogData для сохранения
			dialogData := model.DialogData{
				Data: make([]model.DialogMessage, 0, len(respModel.Context.Messages)),
			}

			for _, msg := range respModel.Context.Messages {
				creator := model.CreatorUser
				if msg.Type == "assistant" {
					creator = model.CreatorAssistant
				}

				dialogData.Data = append(dialogData.Data, model.DialogMessage{
					Creator:   int(creator),
					Message:   msg.Content,
					Timestamp: msg.Timestamp.Format(time.RFC3339),
				})
			}

			// Сохраняем в БД
			if jsonData, err := json.Marshal(dialogData); err == nil {
				if err := m.db.SaveDialog(dialogId, jsonData); err != nil {
					logger.Error("Не удалось сохранить контекст диалога %d: %v", dialogId, err)
				} else {
					logger.Debug("Сохранен контекст диалога %d (%d сообщений)", dialogId, len(respModel.Context.Messages))
				}
			}
		}

		return true
	})

	logger.Info("MistralModel: сохранение контекстов завершено")
}

// CleanDialogData очищает данные конкретного диалога (реализация model.UniversalModel)
func (m *MistralModel) CleanDialogData(dialogId uint64) {
	// Ищем responder по dialogId в Chan
	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		if respModel.Chan != nil && respModel.Chan.DialogId == dialogId {
			// Очищаем контекст этого диалога
			respModel.Context = nil
			logger.Debug("Очищен контекст диалога %d из памяти", dialogId)
			return false // Прекращаем поиск
		}
		return true // Продолжаем поиск
	})
}

// TranscribeAudio транскрибирует аудио (реализация model.UniversalModel)
func (m *MistralModel) TranscribeAudio(audioData []byte, fileName string) (string, error) {
	return "", fmt.Errorf("транскрибирование аудио не поддерживается для Mistral")
}

// CleanUp запускает фоновую очистку устаревших респондеров (реализация model.UniversalModel)
func (m *MistralModel) CleanUp() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()

			m.responders.Range(func(key, value interface{}) bool {
				responder := value.(*RespModel)
				ttlExpired := responder.TTL.Before(now)

				respId, ok := key.(uint64)
				if !ok {
					logger.Error("Некорректный тип ключа: %T, ожидался uint64", key)
					return true
				}

				if ttlExpired {
					// Удаляем весь RespModel (вместе с Context)
					if responder.Cancel != nil {
						responder.Cancel()
					}
					m.closeResponderChannels(responder)
					m.responders.Delete(respId)
				}
				// Отдельная очистка Context не нужна - он удаляется вместе с RespModel

				return true
			})

		case <-m.ctx.Done():
			return
		}
	}
}

// Shutdown корректно завершает работу модели (реализация model.UniversalModel)
func (m *MistralModel) Shutdown() {
	m.shutdownOnce.Do(func() {
		logger.Info("Начинается процесс завершения работы модуля MistralModel")

		// Сначала завершаем DialogSaver чтобы сохранить все сообщения
		if m.dialogSaver != nil {
			m.dialogSaver.Shutdown()
		}

		if m.cancel != nil {
			m.cancel()
		}

		if m.client != nil {
			m.client.Shutdown()
		}

		m.cleanupAllResponders()
		m.cleanupWaitChannels()

		logger.Info("Модуль MistralModel успешно завершил работу")
	})
}

func (m *MistralModel) convertToModelRespModel(internal *RespModel) *model.RespModel {
	// Создаем map с одним каналом для совместимости
	chanMap := make(map[uint64]*model.Ch)
	if internal.Chan != nil {
		// Используем DialogId как ключ
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

func (m *MistralModel) closeResponderChannels(respModel *RespModel) {
	if respModel.Chan != nil {
		close(respModel.Chan.TxCh)
		close(respModel.Chan.RxCh)
	}
}

func (m *MistralModel) cleanupAllResponders() {
	logger.Info("Закрытие всех каналов и очистка респондеров")

	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		if respModel.Cancel != nil {
			respModel.Cancel()
		}

		m.closeResponderChannels(respModel)
		m.responders.Delete(key)

		return true
	})
}

func (m *MistralModel) cleanupWaitChannels() {
	logger.Info("Очистка каналов ожидания")

	m.waitChannels.Range(func(key, value interface{}) bool {
		if ch, ok := value.(chan struct{}); ok {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
		m.waitChannels.Delete(key)
		return true
	})
}

// Request выполняет запрос к Mistral модели, используя историю диалога как контекст
func (m *MistralModel) Request(modelId string, dialogId uint64, text *string, files ...model.FileUpload) (model.AssistResponse, error) {
	var emptyResponse model.AssistResponse

	// Проверяем наличие текста или файлов
	hasText := text != nil && *text != ""
	hasFiles := len(files) > 0

	if !hasText && !hasFiles {
		return emptyResponse, fmt.Errorf("пустое сообщение и нет файлов")
	}

	// Ищем RespModel по dialogId в Chan
	var respModel *RespModel
	m.responders.Range(func(key, value interface{}) bool {
		rm := value.(*RespModel)

		if rm.Chan != nil && rm.Chan.DialogId == dialogId {
			respModel = rm
			return false // Прекращаем поиск
		}
		return true // Продолжаем поиск
	})

	if respModel == nil {
		return emptyResponse, fmt.Errorf("RespModel не найден для dialogId %d", dialogId)
	}

	// Получаем контекст диалога из памяти
	if respModel.Context == nil {
		return emptyResponse, fmt.Errorf("контекст диалога не найден для dialogId %d", dialogId)
	}

	// Обновляем TTL респондера при каждом запросе
	respModel.TTL = time.Now().Add(m.UserModelTTl)

	// Добавляем сообщение пользователя в контекст
	var userContent string
	if text != nil {
		userContent = *text
	}

	userMessage := Message{
		Type:      "user",
		Content:   userContent,
		Timestamp: time.Now(),
	}

	respModel.Context.Messages = append(respModel.Context.Messages, userMessage)
	respModel.Context.LastUsed = time.Now()

	// Формируем массив сообщений для Mistral API
	// Берем последние сообщения для контекста
	startIndex := 0
	if mode.ContextMessagesLimit > 0 && len(respModel.Context.Messages) > mode.ContextMessagesLimit {
		startIndex = len(respModel.Context.Messages) - mode.ContextMessagesLimit
	}
	contextMessages := respModel.Context.Messages[startIndex:]

	// Конвертируем в формат Mistral
	messages := make([]MistralMessage, 0, len(contextMessages))
	for _, msg := range contextMessages {
		role := "user"
		if msg.Type == "assistant" {
			role = "assistant"
		}

		// Пропускаем пустые сообщения ассистента (Mistral API их не принимает)
		if role == "assistant" && msg.Content == "" {
			logger.Debug("Пропущено пустое сообщение ассистента из истории диалога")
			continue
		}

		messages = append(messages, MistralMessage{
			Role:    role,
			Content: msg.Content,
		})
	}

	// Получаем инструменты через ActionHandler
	var tools interface{}
	if m.actionHandler != nil {
		tools = m.actionHandler.GetTools(models.ProviderMistral)
	}

	// Вызываем Mistral API с инструментами
	response, err := m.client.ChatWithAgent(modelId, messages, tools)
	if err != nil {
		return emptyResponse, fmt.Errorf("ошибка запроса к Mistral: %w", err)
	}

	// Обрабатываем ответ
	assistResponse := m.processResponse(response)

	// Добавляем ответ ассистента в контекст только если он не пустой
	if assistResponse.Message != "" {
		assistantMessage := Message{
			Type:      "assistant",
			Content:   assistResponse.Message,
			Timestamp: time.Now(),
		}

		respModel.Context.Messages = append(respModel.Context.Messages, assistantMessage)
		respModel.Context.LastUsed = time.Now()
	} else {
		logger.Warn("Получен пустой ответ от ассистента, не добавляем в контекст")
	}

	// Сохраняем сообщение пользователя через DialogSaver
	userResp := model.AssistResponse{Message: userContent}
	m.dialogSaver.SaveDialog(model.CreatorUser, dialogId, &userResp)

	// Сохраняем ответ ассистента через DialogSaver (даже если пустой, для истории)
	m.dialogSaver.SaveDialog(model.CreatorAssistant, dialogId, &assistResponse)

	// Если была вызвана функция, выполняем её
	if response.HasFunc && m.actionHandler != nil {
		funcResult := m.actionHandler.RunAction(m.ctx, response.FuncName, response.FuncArgs)
		logger.Debug("Результат выполнения функции %s: %s", response.FuncName, funcResult)

		// Формируем ответ с результатом функции
		assistResponse.Message += "\n\n" + funcResult
	}

	return assistResponse, nil
}

// convertDialogToMistralMessages конвертирует историю диалога в формат Mistral
func (m *MistralModel) convertDialogToMistralMessages(dialogMessages []model.DialogMessage) []MistralMessage {
	var messages []MistralMessage

	// Применяем ограничение на количество сообщений
	startIndex := 0
	if mode.ContextMessagesLimit > 0 && len(dialogMessages) > mode.ContextMessagesLimit {
		startIndex = len(dialogMessages) - mode.ContextMessagesLimit
	}

	for i := startIndex; i < len(dialogMessages); i++ {
		msg := dialogMessages[i]
		role := "user"
		if msg.Creator == int(model.CreatorAssistant) {
			role = "assistant"
		}

		messages = append(messages, MistralMessage{
			Role:    role,
			Content: msg.Message,
		})
	}

	return messages
}

// processResponse обрабатывает ответ от Mistral
func (m *MistralModel) processResponse(response Response) model.AssistResponse {
	assistResponse := model.AssistResponse{
		Message:  response.Message,
		Meta:     false,
		Operator: false,
	}

	// Если есть вызов функции, но нет сообщения, используем пустую строку
	if response.HasFunc && response.Message == "" {
		assistResponse.Message = ""
	}

	return assistResponse
}
