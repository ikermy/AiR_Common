package mistral

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// MistralModel реализует интерфейс model.UniversalModel для работы с Mistral AI
type MistralModel struct {
	ctx           context.Context
	cancel        context.CancelFunc
	client        *MistralAgentClient
	db            DB
	responders    sync.Map      // map[uint64]*RespModel
	waitChannels  sync.Map      // map[uint64]chan struct{}
	UserModelTTl  time.Duration // Время жизни пользовательской модели в памяти
	actionHandler model.ActionHandler
	shutdownOnce  sync.Once
	router        model.RouterInterface // Ссылка на router для GetRealUserID
}

// DB интерфейс для работы с базой данных (расширяет model.DialogDB)
//type DB interface {
//	//db.DialogDB
//	ReadContext(dialogId uint64) (json.RawMessage, error)
//	SaveContext(dialogId uint64, context json.RawMessage) error
//}

type DB comdb.Exterior

type RespModel struct {
	Ctx            context.Context
	Cancel         context.CancelFunc
	Chan           *model.Ch      // Один канал для этого респондента
	Context        *DialogContext // Один текущий контекст диалога
	TTL            time.Time
	Assist         model.Assistant
	RespName       string
	Services       Services
	RealUserId     uint64 // Кэшированный реальный user_id
	ConversationId string // ID conversation для Mistral Conversations API
	Haunter        bool   // Модель используется для поиска лидов
	//LibraryId string // ID библиотеки Mistral для document_library (кэш из БД)
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
func New(parent context.Context, conf *conf.Conf, actionHandler model.ActionHandler, db DB, router model.RouterInterface) *MistralModel {
	ctx, cancel := context.WithCancel(parent)

	return &MistralModel{
		ctx:           ctx,
		cancel:        cancel,
		client:        NewMistralAgentClient(parent, conf),
		db:            db,
		responders:    sync.Map{},
		waitChannels:  sync.Map{},
		UserModelTTl:  time.Duration(conf.GLOB.UserModelTTl) * time.Minute,
		actionHandler: actionHandler,
		router:        router,
	}
}

// NewAsRouterOption создаёт Mistral модель и возвращает её как опцию для ModelRouter
// Использование: router := model.NewModelRouter(ctx, conf, db, mistral.NewAsRouterOption())
func NewAsRouterOption() model.RouterOption {
	return func(r *model.ModelRouter, ctx context.Context, cfg *conf.Conf, db model.DB) error {
		// Создаём универсальный обработчик функций
		actionHandler := &model.UniversalActionHandler{}

		// Создаём Mistral модель с action handler и router
		mistralModel := New(ctx, cfg, actionHandler, db, r)

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
func (m *MistralModel) GetFileAsReader(userId uint32, url string) (io.Reader, error) {
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
		return m.convertToModelrespModel(respModel), nil
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
		Context: &DialogContext{
			Messages: []Message{}, // Пустой контекст - при использовании Conversations API история хранится на стороне Mistral
			LastUsed: time.Now(),
		},
		Services: Services{},
		Ctx:      userCtx,
		Cancel:   cancel,
	}

	// ВАЖНО: При использовании Conversations API история диалога НЕ загружается из БД!
	// Mistral хранит всю историю на своей стороне через conversation_id.
	// Локальный контекст используется ТОЛЬКО для сохранения в БД при выходе.

	// Загружаем conversation_id из БД (если есть)
	contextData, err := m.db.ReadContext(dialogId, create.ProviderMistral)
	if err != nil {
		if strings.Contains(err.Error(), "получены пустые данные") {
			//logger.Debug("Инициализация нового диалога %d", dialogId, assist.UserId)
			// ConversationId будет создан при первом запросе
		} else {
			logger.Error("Ошибка чтения контекста для dialogId %d: %v", dialogId, err)
		}
	} else if contextData != nil {
		//logger.Debug("Контекст загружен для dialogId %d: %s", dialogId, string(contextData), assist.UserId)

		var contextObj struct {
			ConversationID string `json:"conversation_id"`
		}

		// JSON_EXTRACT может вернуть строку с кавычками, пробуем распарсить
		err = json.Unmarshal(contextData, &contextObj)
		if err != nil {
			// Если не получилось, пробуем убрать внешние кавычки и распарсить снова
			var rawString string
			if err2 := json.Unmarshal(contextData, &rawString); err2 == nil {
				// Успешно извлекли строку, теперь парсим её как JSON
				if err3 := json.Unmarshal([]byte(rawString), &contextObj); err3 != nil {
					logger.Error("Ошибка десериализации контекста для dialogId %d: %v", dialogId, err3)
				}
			} else {
				logger.Error("Ошибка десериализации контекста для dialogId %d: %v", dialogId, err)
			}
		}

		if contextObj.ConversationID != "" {
			user.ConversationId = contextObj.ConversationID
			//logger.Debug("Загружен conversation_id: %s", contextObj.ConversationID, assist.UserId)
		}
	}

	// Загружаем RealUserId ОДИН РАЗ при создании (избегаем повторных HTTP запросов)
	if realUserId, err := m.GetRealUserID(assist.UserId); err == nil {
		user.RealUserId = realUserId
	} else {
		logger.Warn("Не удалось загрузить RealUserId: %v", err, assist.UserId)
		user.RealUserId = 0 // Будет пропущена генерация изображений
	}

	// Загружаем параметры модели из БД (включая Haunter)
	compressedData, _, err := m.db.ReadUserModelByProvider(assist.UserId, create.ProviderMistral)
	if err != nil {
		logger.Warn("Ошибка чтения данных модели из БД: %v, используем конфигурацию по умолчанию", err, assist.UserId)
	} else if compressedData != nil {
		// Используем функцию из пакета db для распаковки и извлечения всех параметров
		_, _, _, _, _, _, haunter, _, err := comdb.DecompressAndExtractMetadata(compressedData)
		if err != nil {
			logger.Warn("Ошибка распаковки параметров модели: %v", err, assist.UserId)
		} else {
			user.Haunter = haunter
		}
	}

	//// Загружаем LibraryId ОДИН РАЗ при создании (избегаем запросов к БД при каждом сообщении)
	//if libraryID, err := m.loadLibraryIdFromDB(assist.UserId); err == nil {
	//	user.LibraryId = libraryID
	//	logger.Debug("LibraryId загружен для пользователя %d: %s", assist.UserId, libraryID, assist.UserId)
	//} else {
	//	logger.Debug("LibraryId не найден для пользователя %d (будет создан при загрузке файлов)", assist.UserId, assist.UserId)
	//}

	// Используем respId как ключ (один пользователь может иметь несколько диалогов)
	m.responders.Store(respId, user)

	if waitChIface, exists := m.waitChannels.Load(respId); exists {
		waitCh := waitChIface.(chan struct{})
		close(waitCh)
		m.waitChannels.Delete(respId)
	}

	return m.convertToModelrespModel(user), nil
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

		if respModel.Chan != nil {
			dialogId := respModel.Chan.DialogId

			// Сохраняем conversation_id (если есть)
			if respModel.ConversationId != "" {
				contextObj := map[string]interface{}{
					"conversation_id": respModel.ConversationId,
				}

				contextJSON, err := json.Marshal(contextObj)
				if err != nil {
					logger.Error("Ошибка сериализации conversation_id для dialogId %d: %v", dialogId, err)
				} else {
					err = m.db.SaveContext(dialogId, create.ProviderMistral, contextJSON)
					if err != nil {
						logger.Error("Ошибка сохранения conversation_id для dialogId %d: %v", dialogId, err)
					}
				}
			}

			// Сохраняем контекст сообщений (если есть)
			if respModel.Context != nil && len(respModel.Context.Messages) > 0 {
				// Сохраняем в простом json.RawMessage формате
				jsonData, err := json.Marshal(respModel.Context.Messages)
				if err != nil {
					logger.Error("Ошибка сериализации контекста диалога %d: %v", dialogId, err)
				} else {
					if err := m.db.SaveDialog(dialogId, jsonData); err != nil {
						logger.Error("Не удалось сохранить контекст диалога %d: %v", dialogId, err)
					}
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
			return false // Прекращаем поиск
		}
		return true // Продолжаем поиск
	})
}

// saveConversationId сохраняет conversation_id в БД (или удаляет если пустой)
func (m *MistralModel) saveConversationId(dialogId uint64, conversationId string) {
	if conversationId == "" {
		// Удаляем conversation_id из БД (сброс)
		contextObj := map[string]interface{}{
			"conversation_id": "",
		}

		contextJSON, err := json.Marshal(contextObj)
		if err != nil {
			logger.Error("Ошибка сериализации пустого conversation_id для dialogId %d: %v", dialogId, err)
			return
		}

		err = m.db.SaveContext(dialogId, create.ProviderMistral, contextJSON)
		if err != nil {
			logger.Error("Ошибка удаления conversation_id для dialogId %d: %v", dialogId, err)
		}
		return
	}

	contextObj := map[string]interface{}{
		"conversation_id": conversationId,
	}

	contextJSON, err := json.Marshal(contextObj)
	if err != nil {
		logger.Error("Ошибка сериализации conversation_id для dialogId %d: %v", dialogId, err)
		return
	}

	err = m.db.SaveContext(dialogId, create.ProviderMistral, contextJSON)
	if err != nil {
		logger.Error("Ошибка сохранения conversation_id для dialogId %d: %v", dialogId, err)
	}
}

// TranscribeAudio обёртка
func (m *MistralModel) TranscribeAudio(userid uint32, audioData []byte, fileName string) (string, error) {
	return m.transcribeAudioFile(audioData, fileName)
}

// TranscribeAudio транскрибирует аудио файл используя Mistral Audio Transcription API
func (m *MistralModel) transcribeAudioFile(audioData []byte, fileName string) (string, error) {
	if len(audioData) == 0 {
		return "", fmt.Errorf("пустые аудиоданные")
	}

	if m.client == nil {
		return "", fmt.Errorf("mistral client не инициализирован")
	}

	// Формируем multipart request для отправки аудио файла
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)
	defer func() {
		if err := writer.Close(); err != nil {
			logger.Error("TranscribeAudio: ошибка закрытия writer: %v", err)
		}
	}()

	if err := writer.WriteField("model", "voxtral-mini-latest"); err != nil {
		return "", fmt.Errorf("ошибка добавления поля model: %w", err)
	}

	// Добавляем аудио файл
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return "", fmt.Errorf("ошибка создания form file для аудио: %w", err)
	}

	if _, err := part.Write(audioData); err != nil {
		return "", fmt.Errorf("ошибка записи аудио данных: %w", err)
	}

	// Закрываем writer перед отправкой запроса
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("ошибка закрытия writer: %w", err)
	}

	// Отправляем запрос на Mistral API
	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, "https://api.mistral.ai/v1/audio/transcriptions", &requestBody)
	if err != nil {
		return "", fmt.Errorf("ошибка создания HTTP запроса: %w", err)
	}

	// Используем x-api-key заголовок согласно документации Mistral
	req.Header.Set("x-api-key", m.client.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка отправки запроса на Mistral: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Error("TranscribeAudio: ошибка закрытия response body: %v", err)
		}
	}()

	// Читаем ответ
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ошибка чтения ответа: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ошибка API Mistral (статус %d): %s", resp.StatusCode, string(responseBody))
	}

	// Парсим ответ
	var result struct {
		Text string `json:"text"`
	}

	if err := json.Unmarshal(responseBody, &result); err != nil {
		return "", fmt.Errorf("ошибка парсинга ответа Mistral: %w", err)
	}

	if result.Text == "" {
		return "", fmt.Errorf("Mistral вернул пустой текст транскрипции")
	}

	//logger.Debug("TranscribeAudio: успешно транскрибировано аудио, длина текста: %d символов", len(result.Text))
	return result.Text, nil
}

// DeleteTempFile удаляет загруженный файл из Mistral Files API
// Используется для очистки временных файлов после обработки
func (m *MistralModel) DeleteTempFile(fileID string) error {
	if m.client == nil {
		return fmt.Errorf("mistral client не инициализирован")
	}

	if fileID == "" {
		return fmt.Errorf("fileID не может быть пустым")
	}

	err := m.client.DeleteFile(fileID)
	if err != nil {
		logger.Error("DeleteTempFile: ошибка удаления файла %s: %v", fileID, err)
		return err
	}

	//logger.Debug("DeleteTempFile: файл %s успешно удалён", fileID)
	return nil
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

func (m *MistralModel) convertToModelrespModel(internal *RespModel) *model.RespModel {
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

// GetRealUserID получает реальный userId через ModelRouter
// Использует единый метод для всех провайдеров (OpenAI, Mistral)
func (m *MistralModel) GetRealUserID(userId uint32) (uint64, error) {
	if m.router == nil {
		return 0, fmt.Errorf("router не инициализирован")
	}
	return m.router.GetRealUserID(userId)
}
