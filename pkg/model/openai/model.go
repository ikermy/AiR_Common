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
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
	"github.com/sashabaranov/go-openai"
)

type DB = comdb.Exterior

type OpenAIModel struct {
	ctx           context.Context
	cancel        context.CancelFunc
	client        *openai.Client
	db            DB
	responders    sync.Map // Хранит указатели на RespModel
	waitChannels  sync.Map // Хранит каналы для синхронизации горутин
	UserModelTTl  time.Duration
	actionHandler model.ActionHandler
	shutdownOnce  sync.Once
}

type RespModel struct {
	Ctx      context.Context
	Cancel   context.CancelFunc
	Thread   *openai.Thread // Один thread для этого респондента
	Chan     *model.Ch      // Один канал для этого респондента
	TTL      time.Time
	Assist   model.Assistant
	RespName string
	Services Services
	Haunter  bool // Модель используется для поиска лидов
}

type Services struct {
	Listener   atomic.Bool
	Respondent atomic.Bool
}

func New(parent context.Context, conf *conf.Conf, d DB, actionHandler model.ActionHandler) *OpenAIModel {
	ctx, cancel := context.WithCancel(parent)
	return &OpenAIModel{
		ctx:           ctx,
		cancel:        cancel,
		client:        openai.NewClient(conf.GPT.OpenAIKey),
		db:            d,
		responders:    sync.Map{},
		waitChannels:  sync.Map{},
		UserModelTTl:  time.Duration(conf.GLOB.UserModelTTl) * time.Minute,
		actionHandler: actionHandler,
	}
}

// NewAsRouterOption создаёт OpenAI модель и возвращает её как опцию для ModelRouter
// Использование: router := model.NewModelRouter(ctx, conf, db, openai.NewAsRouterOption())
func NewAsRouterOption() model.RouterOption {
	return func(r *model.ModelRouter, ctx context.Context, cfg *conf.Conf, db model.DB) error {
		// Создаём универсальный обработчик функций
		actionHandler := &model.UniversalActionHandler{}

		// Приводим DB к типу OpenAIModel.DB через интерфейс
		openaiDB, ok := db.(DB)
		if !ok {
			return fmt.Errorf("DB не соответствует интерфейсу openai.DB")
		}

		// Создаём OpenAI модель с action handler
		openaiModel := New(ctx, cfg, openaiDB, actionHandler)

		// Регистрируем модель в роутере
		return model.WithOpenAIModel(openaiModel)(r, ctx, cfg, db)
	}
}

// Реализация интерфейса model.Inter
func (m *OpenAIModel) NewMessage(operator model.Operator, msgType string, content *model.AssistResponse, name *string, files ...model.FileUpload) model.Message {
	return model.Message{
		Operator:  operator,
		Type:      msgType,
		Content:   *content,
		Name:      *name,
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
		content, err := m.downloadFileFromOpenAI(fileID)
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

func (m *OpenAIModel) GetOrSetRespGPT(assist model.Assistant, dialogId, respId uint64, respName string) (*model.RespModel, error) {
	// Сначала проверяем кэш
	// Используем respId как ключ
	if val, ok := m.responders.Load(respId); ok {
		respModel := val.(*RespModel)
		respModel.TTL = time.Now().Add(m.UserModelTTl) // Обновляем TTL
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
		Thread:   nil, // Будет загружен ниже
		Services: Services{},
		Ctx:      userCtx,
		Cancel:   cancel,
	}

	// Загружаем thread из БД
	contextData, err := m.db.ReadContext(dialogId, create.ProviderOpenAI)
	if err != nil {
		if strings.Contains(err.Error(), "получены пустые данные") {
			logger.Info("Инициализация нового диалога %d", dialogId, assist.UserId)
			// Thread будет создан при первом запросе
		} else {
			logger.Error("Ошибка чтения контекста для dialogId %d: %v", dialogId, err)
		}
	} else if contextData != nil {
		// Загружаем структуру с thread_id
		var contextObj struct {
			ThreadID string `json:"thread_id"`
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
					return nil, err3
				}
			} else {
				logger.Error("Ошибка десериализации контекста для dialogId %d: %v", dialogId, err)
				return nil, err
			}
		}

		// Если thread_id есть, загружаем thread от OpenAI
		if contextObj.ThreadID != "" {
			thread, err := m.client.RetrieveThread(context.Background(), contextObj.ThreadID)
			if err != nil {
				logger.Error("Ошибка загрузки thread %s для dialogId %d: %v", contextObj.ThreadID, dialogId, err)
				// Если ошибка загрузки, создадим новый thread
			} else {
				user.Thread = &thread
				logger.Info("Загружен существующий thread %s для dialogId %d", thread.ID, dialogId)
			}
		}
	}

	// Загружаем параметры модели из БД (включая Haunter)
	compressedData, _, err := m.db.ReadUserModelByProvider(assist.UserId, create.ProviderOpenAI)
	if err != nil {
		logger.Warn("Ошибка чтения данных модели из БД: %v, используем конфигурацию по умолчанию", err, assist.UserId)
	} else if compressedData != nil {
		// Используем функцию из пакета db для распаковки и извлечения всех параметров
		_, _, _, image, webSearch, video, haunter, err := comdb.DecompressAndExtractMetadata(compressedData)
		if err != nil {
			logger.Warn("Ошибка распаковки параметров модели: %v", err, assist.UserId)
		} else {
			user.Haunter = haunter
			logger.Debug("Загружены параметры модели: Image=%v, WebSearch=%v, Video=%v, Haunter=%v",
				image, webSearch, video, haunter, assist.UserId)
		}
	}

	// Используем respId как ключ
	m.responders.Store(respId, user)

	if waitChIface, exists := m.waitChannels.Load(respId); exists {
		waitCh := waitChIface.(chan struct{})
		close(waitCh)
		m.waitChannels.Delete(respId)
	}

	return m.convertToModelRespModel(user), nil
}

func (m *OpenAIModel) GetCh(respId uint64) (*model.Ch, error) {
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

func (m *OpenAIModel) getTryCh(respId uint64) (*model.Ch, error) {
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

func (m *OpenAIModel) GetRespIdByDialogId(dialogId uint64) (uint64, error) {
	// Ищем responder по dialogId в Chan
	var foundRespId uint64
	found := false

	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

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

func (m *OpenAIModel) CleanDialogData(dialogId uint64) {
	// Ищем responder по dialogId в Chan
	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		if respModel.Chan != nil && respModel.Chan.DialogId == dialogId {
			// Очищаем thread этого диалога
			respModel.Thread = nil
			logger.Debug("Очищен thread диалога %d из памяти", dialogId)
			return false
		}
		return true
	})
}

func (m *OpenAIModel) SaveAllContextDuringExit() {
	logger.Info("OpenAIModel: сохранение всех thread перед выходом...")

	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		if respModel.Thread != nil && respModel.Chan != nil {
			dialogId := respModel.Chan.DialogId

			// Сохраняем только thread_id, не весь объект Thread
			contextData := map[string]interface{}{
				"thread_id": respModel.Thread.ID,
			}
			threadsJSON, err := json.Marshal(contextData)
			if err != nil {
				logger.Error("Ошибка сериализации thread для dialogId %d: %v", dialogId, err)
				return true
			}

			err = m.db.SaveContext(dialogId, create.ProviderOpenAI, threadsJSON)
			if err != nil {
				logger.Error("Ошибка сохранения thread для dialogId %d: %v", dialogId, err)
			} else {
				logger.Debug("Сохранен thread для dialogId %d", dialogId)
			}
		}

		return true
	})

	logger.Info("OpenAIModel: сохранение thread завершено")
}

// TranscribeAudio обёртка
func (m *OpenAIModel) TranscribeAudio(userid uint32, audioData []byte, fileName string) (string, error) {
	return m.transcribeAudioFile(audioData, fileName)
}

func (m *OpenAIModel) transcribeAudioFile(audioData []byte, fileName string) (string, error) {
	if len(audioData) == 0 {
		return "", fmt.Errorf("пустые аудиоданные")
	}

	req := openai.AudioRequest{
		Model:    openai.Whisper1,
		FilePath: fileName,
		Reader:   bytes.NewReader(audioData),
	}

	resp, err := m.client.CreateTranscription(m.ctx, req)
	if err != nil {
		return "", fmt.Errorf("ошибка транскрибирования аудио: %w", err)
	}

	if resp.Text == "" {
		return "", fmt.Errorf("получен пустой текст при транскрибировании")
	}

	return resp.Text, nil
}

// DeleteTempFile удаляет загруженный файл (OpenAI не требует явного удаления)
func (m *OpenAIModel) DeleteTempFile(fileID string) error {
	// OpenAI не требует явного удаления файлов после использования
	return nil
}

func (m *OpenAIModel) Shutdown() {
	var shutdownErrors []string

	m.shutdownOnce.Do(func() {
		logger.Info("Начинается процесс завершения работы модуля OpenAIModel")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if err := m.cancelAllActiveRunsCtx(shutdownCtx); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Sprintf("ошибка отмены активных runs: %v", err))
		}

		logger.Info("Сохранение всех контекстов при завершении работы")
		if err := m.saveAllContextsGracefullyCtx(shutdownCtx); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Sprintf("ошибка сохранения контекстов: %v", err))
		}

		if m.cancel != nil {
			m.cancel()
		}

		m.cleanupAllResponders()
		m.cleanupWaitChannels()

		logger.Info("Процесс завершения работы модуля OpenAIModel завершен")
	})

	if len(shutdownErrors) > 0 {
		logger.Error("ошибки при завершении работы: %s", strings.Join(shutdownErrors, "; "))
	}

	logger.Info("Модуль OpenAIModel успешно завершил работу")
}

// Вспомогательная функция для конвертации внутреннего RespModel в model.RespModel
func (m *OpenAIModel) convertToModelRespModel(internal *RespModel) *model.RespModel {
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
