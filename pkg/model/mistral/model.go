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

	"github.com/ikermy/AiR_Common/pkg/com"
	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// Model реализует интерфейс model.UniversalModel для работы с Mistral AI
type Model struct {
	ctx            context.Context
	cancel         context.CancelFunc
	client         *MistralAgentClient
	db             DB
	responders     sync.Map      // map[uint64]*RespModel
	waitChannels   sync.Map      // map[uint64]chan struct{}
	UserModelTTl   time.Duration // Время жизни пользовательской модели в памяти
	actionHandler  model.ActionHandler
	shutdownOnce   sync.Once
	router         model.RouterInterface  // Ссылка на router для GetRealuserID
	universalModel *create.UniversalModel // Для доступа к DecompressModelData
}

type DB comdb.Exterior

type RespModel struct {
	Ctx            context.Context
	Cancel         context.CancelFunc
	Chan           *model.Ch            // Канал для этого респондента (основной, deprecated - используйте ChanMap)
	ChanMap        map[uint64]*model.Ch // Map каналов для поддержки множественных dialogID (унификация с OpenAI/Google)
	Context        *DialogContext       // Один текущий контекст диалога
	TTL            time.Time
	Assist         model.Assistant
	RespName       string
	Services       Services
	RealuserID     uint64 // Кэшированный реальный user_id
	ConversationId string // ID conversation для Mistral Conversations API
	Haunter        bool   // Модель используется для поиска лидов
	ToolsSynced    bool   // true — агент уже синхронизирован с MCP tools в этой сессии
	//LibraryId string // ID библиотеки Mistral для document_library (кэш из БД)
}

// GetChannel реализует интерфейс model.ChannelProvider
func (r *RespModel) GetChannel() *model.Ch {
	return r.Chan
}

// GetChannelMap реализует интерфейс model.ChannelProvider
func (r *RespModel) GetChannelMap() map[uint64]*model.Ch {
	return r.ChanMap
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
func New(parent context.Context, actionHandler model.ActionHandler, db DB, router model.RouterInterface) *Model {
	ctx, cancel := context.WithCancel(parent)

	mistralClient := NewMistralAgentClient(parent)

	// Резолвер персональных ключей Mistral: возвращаем только ключ из БД или пустую строку.
	mistralClient.SetKeyResolver(func(userID uint32) string {
		if key, err := db.GetUserAPIKey(userID, create.ProviderMistral); err == nil {
			return key
		}
		return ""
	})

	return &Model{
		ctx:           ctx,
		cancel:        cancel,
		client:        mistralClient,
		db:            db,
		responders:    sync.Map{},
		waitChannels:  sync.Map{},
		UserModelTTl:  mode.UserModelTTl,
		actionHandler: actionHandler,
		router:        router,
	}
}

// NewAsRouterOption создаёт Mistral модель и возвращает её как опцию для ModelRouter
// Использование: router := model.NewModelRouter(ctx, db, mistral.NewAsRouterOption())
func NewAsRouterOption() model.RouterOption {
	return func(r *model.Router, ctx context.Context, db model.DB) error {
		// Создаём универсальный обработчик функций с Google OAuth конфигом
		actionHandler := model.NewUniversalActionHandler(ctx)

		// Создаём Mistral модель с action handler и router
		mistralModel := New(ctx, actionHandler, db, r)

		// Устанавливаем UniversalModel для доступа к DecompressModelData
		universalModel := create.New(ctx, db)

		// Подключаем MCP fetchers для create-time операций (создание агента через Mistral API).
		// Аналогично google/model.go: function declarations и prompt hint — только от MCP.
		if mcpProvider, ok := model.ActionHandler(actionHandler).(model.MCPConfigProvider); ok {
			universalModel.SetMistralMCPFetchers(
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

		mistralModel.SetUniversalModel(universalModel)

		// Регистрируем модель в роутере
		return model.WithMistralModel(mistralModel)(r, ctx, db)
	}
}

// NewMessage создает новое сообщение (реализация model.UniversalModel)
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

// GetFileAsReader загружает файл по URL (реализация model.UniversalModel)
func (m *Model) GetFileAsReader(_ uint32, url string) (io.Reader, error) {
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
func (m *Model) GetOrSetRespGPT(assist model.Assistant, dialogID, respId uint64, respName string) (*model.RespModel, error) {
	// Используем respId как ключ
	if val, ok := m.responders.Load(respId); ok {
		respModel := val.(*RespModel)
		respModel.TTL = time.Now().Add(m.UserModelTTl) // Обновляем TTL при каждом обращении
		return m.convertToModelRespModel(respModel), nil
	}

	// Проверяем наличие API-ключа для пользователя до создания респондента.
	// Если ключа нет — возвращаем явную ошибку, иначе все запросы к Mistral упадут с 401.
	if m.client == nil || !m.client.HasAPIKey(assist.UserID) {
		return nil, fmt.Errorf("Mistral API ключ не настроен для пользователя %d: добавьте персональный ключ через настройки", assist.UserID)
	}

	// Используем helper-функцию для создания базовых компонентов
	userCtx, cancel, ch, ttl := model.CreateBaseResponder(m.ctx, m.UserModelTTl, assist, dialogID, respName)

	user := &RespModel{
		Assist:   assist,
		RespName: respName,
		TTL:      ttl,
		Chan:     ch,
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
	contextData, err := m.db.ReadContext(dialogID, create.ProviderMistral)
	if err != nil {
		if strings.Contains(err.Error(), "получены пустые данные") {
			//logger.Debug("Инициализация нового диалога %d", dialogID, assist.userID)
			// ConversationId будет создан при первом запросе
			//} else {
			//	logger.Error("Ошибка чтения контекста для dialogID %d: %v", dialogID, err)
		}
	} else if contextData != nil {
		//logger.Debug("Контекст загружен для dialogID %d: %s", dialogID, string(contextData), assist.userID)

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
					//logger.Error("Ошибка десериализации контекста для dialogID %d: %v", dialogID, err3)
				}
				//} else {
				//	logger.Error("Ошибка десериализации контекста для dialogID %d: %v", dialogID, err)
			}
		}

		if contextObj.ConversationID != "" {
			user.ConversationId = contextObj.ConversationID
			//logger.Debug("Загружен conversation_id: %s", contextObj.ConversationID, assist.userID)
		}
	}

	// Загружаем RealuserID ОДИН РАЗ при создании (избегаем повторных HTTP запросов)
	if realuserID, err := m.GetRealUserID(assist.UserID); err == nil {
		user.RealuserID = realuserID
	} else {
		//logger.Warn("Не удалось загрузить RealuserID: %v", err, assist.userID)
		user.RealuserID = 0 // Будет пропущена генерация изображений
	}

	// Загружаем параметры модели из БД (включая Haunter)
	compressedData, _, err := m.db.ReadUserModelByProvider(assist.UserID, create.ProviderMistral)
	if err != nil {
		//logger.Warn("Ошибка чтения данных модели из БД: %v, используем конфигурацию по умолчанию", err, assist.userID)
	} else if compressedData != nil && m.universalModel != nil {
		if modelData, decompErr := m.universalModel.DecompressModelData(compressedData, nil); decompErr == nil {
			user.Haunter = modelData.Haunter
			//} else {
			//	logger.Warn("Ошибка распаковки параметров модели: %v", decompErr, assist.userID)
		}
	}

	//// Загружаем LibraryId ОДИН РАЗ при создании (избегаем запросов к БД при каждом сообщении)
	//if libraryID, err := m.loadLibraryIdFromDB(assist.userID); err == nil {
	//	user.LibraryId = libraryID
	//	logger.Debug("LibraryId загружен для пользователя %d: %s", assist.userID, libraryID, assist.userID)
	//} else {
	//	logger.Debug("LibraryId не найден для пользователя %d (будет создан при загрузке файлов)", assist.userID, assist.userID)
	//}

	// Используем respId как ключ (один пользователь может иметь несколько диалогов)
	m.responders.Store(respId, user)

	// Уведомляем ожидающие горутины о создании респондента
	m.responders.Store(respId, user)

	return m.convertToModelRespModel(user), nil
}

// GetCh получает канал для респондента (реализация model.UniversalModel)
func (m *Model) GetCh(respId uint64) (*model.Ch, error) {
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

// GetRespIdBydialogID получает ID респондента по ID диалога (реализация model.UniversalModel)
func (m *Model) GetRespIdBydialogID(dialogID uint64) (uint64, error) {
	return model.GetRespIdBydialogIDUniversal(dialogID, &m.responders)
}

// SaveAllContextDuringExit сохраняет контекст при выходе (реализация model.UniversalModel)
func (m *Model) SaveAllContextDuringExit() {
	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		if respModel.Chan != nil {
			dialogID := respModel.Chan.DialogID

			// Сохраняем conversation_id (если есть)
			if respModel.ConversationId != "" {
				contextObj := map[string]interface{}{
					"conversation_id": respModel.ConversationId,
				}

				contextJSON, err := json.Marshal(contextObj)
				if err != nil {
					//logger.Error("Ошибка сериализации conversation_id для dialogID %d: %v", dialogID, err)
				} else {
					err = m.db.SaveContext(dialogID, create.ProviderMistral, contextJSON)
					if err != nil {
						//logger.Error("Ошибка сохранения conversation_id для dialogID %d: %v", dialogID, err)
					}
				}
			}

			// Сохраняем контекст сообщений (если есть)
			if respModel.Context != nil && len(respModel.Context.Messages) > 0 {
				// Сохраняем в простом json.RawMessage формате
				jsonData, err := json.Marshal(respModel.Context.Messages)
				if err != nil {
					//logger.Error("Ошибка сериализации контекста диалога %d: %v", dialogID, err)
				} else {
					if err := m.db.SaveDialog(dialogID, jsonData); err != nil {
						//logger.Error("Не удалось сохранить контекст диалога %d: %v", dialogID, err)
					}
				}
			}
		}

		return true
	})
}

// CleanDialogData очищает данные конкретного диалога (реализация model.UniversalModel)
func (m *Model) CleanDialogData(dialogID uint64) {
	// Ищем responder по dialogID в Chan
	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		if respModel.Chan != nil && respModel.Chan.DialogID == dialogID {
			// Очищаем контекст этого диалога
			respModel.Context = nil
			return false // Прекращаем поиск
		}
		return true // Продолжаем поиск
	})
}

// saveConversationId сохраняет conversation_id в БД (или удаляет если пустой)
func (m *Model) saveConversationId(dialogID uint64, conversationId string) {
	if conversationId == "" {
		// Удаляем conversation_id из БД (сброс)
		contextObj := map[string]interface{}{
			"conversation_id": "",
		}

		contextJSON, err := json.Marshal(contextObj)
		if err != nil {
			//logger.Error("Ошибка сериализации пустого conversation_id для dialogID %d: %v", dialogID, err)
			return
		}

		err = m.db.SaveContext(dialogID, create.ProviderMistral, contextJSON)
		if err != nil {
			//logger.Error("Ошибка удаления conversation_id для dialogID %d: %v", dialogID, err)
		}
		return
	}

	contextObj := map[string]interface{}{
		"conversation_id": conversationId,
	}

	contextJSON, err := json.Marshal(contextObj)
	if err != nil {
		//logger.Error("Ошибка сериализации conversation_id для dialogID %d: %v", dialogID, err)
		return
	}

	err = m.db.SaveContext(dialogID, create.ProviderMistral, contextJSON)
	if err != nil {
		//logger.Error("Ошибка сохранения conversation_id для dialogID %d: %v", dialogID, err)
	}
}

// TranscribeAudio обёртка
func (m *Model) TranscribeAudio(_ uint32, audioData []byte, fileName string) (string, error) {
	return m.transcribeAudioFile(audioData, fileName)
}

// TranscribeAudio транскрибирует аудио файл используя Mistral Audio Transcription API
func (m *Model) transcribeAudioFile(audioData []byte, fileName string) (string, error) {
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
			//logger.Error("TranscribeAudio: ошибка закрытия writer: %v", err)
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
			//logger.Error("TranscribeAudio: ошибка закрытия response body: %v", err)
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
func (m *Model) DeleteTempFile(fileID string) error {
	if m.client == nil {
		return fmt.Errorf("mistral client не инициализирован")
	}

	if fileID == "" {
		return fmt.Errorf("fileID не может быть пустым")
	}

	err := m.client.DeleteFile(fileID)
	if err != nil {
		//logger.Error("DeleteTempFile: ошибка удаления файла %s: %v", fileID, err)
		return err
	}

	//logger.Debug("DeleteTempFile: файл %s успешно удалён", fileID)
	return nil
}

// CleanUp запускает фоновую очистку устаревших респондеров (реализация model.UniversalModel)
func (m *Model) CleanUp() {
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
					//logger.Error("Некорректный тип ключа: %T, ожидался uint64", key)
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
func (m *Model) Shutdown(shutCh chan<- com.LogMsg) {
	m.shutdownOnce.Do(func() {
		shutCh <- com.LogMsg{
			Msg: "начало shutdown",
			Mod: "MistralModel",
			Log: 0, // 0 - Info
			UID: 0,
		}

		if m.cancel != nil {
			m.cancel()
		}

		if m.client != nil {
			m.client.Shutdown()
		}

		m.cleanupAllResponders()
		m.cleanupWaitChannels()

		shutCh <- com.LogMsg{
			Msg: "модуль успешно завершил работу",
			Mod: "MistralModel",
			Log: 0, // 0 - Info
			UID: 0,
		}
	})
}

func (m *Model) convertToModelRespModel(internal *RespModel) *model.RespModel {
	// Создаем map с одним каналом для совместимости
	chanMap := make(map[uint64]*model.Ch)
	if internal.Chan != nil {
		// Используем dialogID как ключ
		chanMap[internal.Chan.DialogID] = internal.Chan
	}

	return &model.RespModel{
		Ctx:      internal.Ctx,
		Cancel:   internal.Cancel,
		Chan:     chanMap,
		TTL:      internal.TTL,
		Assist:   internal.Assist,
		RespName: internal.RespName,
		Services: model.Services{
			Listener:   &internal.Services.Listener,
			Respondent: &internal.Services.Respondent,
		},
	}
}

func (m *Model) closeResponderChannels(respModel *RespModel) {
	model.CloseResponderChannelsUniversal(respModel)
}

func (m *Model) cleanupAllResponders() {
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

func (m *Model) cleanupWaitChannels() {
	deletedCount := model.CleanupWaitChannelsUniversal(&m.waitChannels, &m.responders)
	if deletedCount > 0 {
		//logger.Debug("Очищено %d wait channels", deletedCount)
	}
}

// SetUniversalModel устанавливает UniversalModel для доступа к DecompressModelData
func (m *Model) SetUniversalModel(um *create.UniversalModel) {
	m.universalModel = um
}

// GetRealuserID получает реальный userID через ModelRouter
// Использует единый метод для всех провайдеров (OpenAI, Mistral)
func (m *Model) GetRealUserID(userID uint32) (uint64, error) {
	if m.router == nil {
		return 0, fmt.Errorf("router не инициализирован")
	}
	return m.router.GetRealUserID(userID)
}

// InvalidateUserAgentConfigCache инвалидирует кэш конфигурации модели для пользователя
func (m *Model) InvalidateUserAgentConfigCache(userID uint32) {
	var invalidatedCount int
	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)
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
