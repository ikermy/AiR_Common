package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/sashabaranov/go-openai"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Model интерфейс для работы с моделями Assistant
type Model interface {
	NewMessage(msgType string, content *AssistResponse, name *string, files ...FileUpload) Message
	GetFileAsReader(url string) (io.Reader, error)
	GetOrSetRespGPT(assist Assistant, dialogId, respId uint64, respName string) (*RespModel, error)
	GetCh(respId uint64) (Ch, error)
	SaveAllContextDuringExit()
	Request(modelId string, dialogId uint64, text *string, files ...FileUpload) (AssistResponse, error)
	CleanDialogData(dialogId uint64)
	TranscribeAudio(audioData []byte, fileName string) (string, error)
}

type DB interface {
	ReadContext(dialogId uint64) (json.RawMessage, error)
	SaveContext(treadId uint64, context json.RawMessage) error
}

type Models struct {
	ctx           context.Context
	client        *openai.Client
	db            DB
	responders    sync.Map // Хранит указатели на RespModel, а не сами структуры
	waitChannels  sync.Map // Хранит каналы для синхронизации горутин
	UserModelTTl  time.Duration
	actionHandler ActionHandler
}

// Notifications структура для хранения настроек уведомлений о событиях
type Notifications struct {
	Start  bool
	End    bool
	Target bool
}

// Target структура для хранения целей модели
type Target struct {
	MetaAction string
	Triggers   []string
}

type Assistant struct {
	// Размещаем поля от большего к меньшему
	AssistId   string
	AssistName string
	Metas      Target
	Events     Notifications
	UserId     uint32
	Limit      uint32
	Espero     uint8
	Ignore     bool
}

type RespModel struct {
	Ctx       context.Context
	Cancel    context.CancelFunc
	TreadsGPT map[uint64]*openai.Thread
	Chan      map[uint64]Ch
	TTL       time.Time
	Assist    Assistant
	RespName  string
	Services  Services
	mu        sync.RWMutex
	//activeOps sync.WaitGroup // ТЕСТИРОВАТЬ
}

type Services struct {
	Listener   bool
	Respondent bool
}

// FileUpload представляет файл для отправки для code interpreter
type FileUpload struct {
	Name     string    `json:"name"`
	Content  io.Reader `json:"-"`
	MimeType string    `json:"mime_type"`
}

type Action struct {
	SendFiles []File `json:"send_files,omitempty"` // Массив файлов для отправки
}

type FileType string

const (
	Photo FileType = "photo"
	Video FileType = "video"
	Audio FileType = "audio"
	Doc   FileType = "doc"
)

type File struct {
	Type     FileType `json:"type,omitempty"`      // Тип файла, не может быть пустым, должно быть одним из Photo, Video, Audio, Doc
	URL      string   `json:"url,omitempty"`       // URL файла для загрузки может быть пустым если используется
	FileName string   `json:"file_name,omitempty"` // Имя файла для сохранения может быть пустым
	Caption  string   `json:"caption,omitempty"`   // Подпись к файлу может быть пустым
}

// AssistResponse представляет ответ от AI-ассистента
type AssistResponse struct {
	Message string `json:"message,omitempty"` // Текстовое сообщение ответа может быть пустым если есть Action
	Action  Action `json:"action,omitempty"`  // Действия для выполнения может быть пустым если есть Message
	Meta    bool   `json:"target,omitempty"`  // Флаг, что ответ содержит достижение цели по мнению ассистента
}

type Ch struct {
	TxCh     chan Message
	RxCh     chan Message
	UserId   uint32
	DialogId uint64
	RespName string
}

type Message struct {
	Type      string
	Content   AssistResponse
	Name      string
	Timestamp time.Time
	Files     []FileUpload `json:"files,omitempty"`
}

// StartCh структура для передачи данных для запуска слушателя
type StartCh struct {
	Model   *RespModel
	Chanel  Ch
	TreadId uint64
	RespId  uint64
}

// ActionHandler интерфейс для обработки функций OpenAI ассистента
type ActionHandler interface {
	RunAction(functionName, arguments string) string
}

func New(conf *conf.Conf, d DB, actionHandler ActionHandler) *Models {
	return &Models{
		client:        openai.NewClient(conf.GPT.Key),
		ctx:           context.Background(),
		db:            d,
		responders:    sync.Map{},
		waitChannels:  sync.Map{},
		UserModelTTl:  time.Duration(conf.GLOB.UserModelTTl) * time.Minute,
		actionHandler: actionHandler,
	}
}

func (m *Models) OldNewMessage(msgType string, content *AssistResponse, name *string) Message {
	return Message{
		Type:      msgType,
		Content:   *content,
		Name:      *name,
		Timestamp: time.Now(),
	}
}

func (m *Models) NewMessage(msgType string, content *AssistResponse, name *string, files ...FileUpload) Message {
	return Message{
		Type:      msgType,
		Content:   *content,
		Name:      *name,
		Timestamp: time.Now(),
		Files:     files,
	}
}

func createMsgWithFiles(text *string, fileIDs []string) openai.MessageRequest {
	msg := openai.MessageRequest{
		Role:    "user",
		Content: *text,
	}

	if len(fileIDs) > 0 {
		attachments := make([]openai.ThreadAttachment, 0, len(fileIDs))
		for _, fileID := range fileIDs {
			attachments = append(attachments, openai.ThreadAttachment{
				FileID: fileID,
				Tools: []openai.ThreadAttachmentTool{
					{Type: "code_interpreter"},
				},
			})
		}
		msg.Attachments = attachments
	}

	return msg
}

// GetFileAsReader получает файл в виде io.Reader из векторного хранилища или по URL
func (m *Models) GetFileAsReader(url string) (io.Reader, error) {
	// Проверяем, что у нас есть источник файла
	if url == "" {
		return nil, fmt.Errorf("не указан источник файла: отсутствуют URL")
	}

	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки файла по URL: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("ошибка HTTP при загрузке файла: статус %d", resp.StatusCode)
	}

	return resp.Body, nil
}

func (m *Models) CreateThead(dialogId uint64) error {
	// Загружаем модель пользователя из sync.Map
	val, ok := m.responders.Load(dialogId)
	if !ok {
		return fmt.Errorf("RespModel не найден для userId %d", dialogId)
	}

	// Приведение типа к указателю на RespModel
	respModel := val.(*RespModel)

	// Блокируем для записи
	respModel.mu.Lock()
	defer respModel.mu.Unlock()

	// Инициализируем карту тредов, если она nil
	if respModel.TreadsGPT == nil {
		respModel.TreadsGPT = make(map[uint64]*openai.Thread)
	}

	// Проверяем, существует ли уже тред для dialogId
	thread, exists := respModel.TreadsGPT[dialogId]
	if exists && thread != nil {
		return nil
	}

	// Создаем новый тред с использованием клиента OpenAI
	th, err := m.client.CreateThread(m.ctx, openai.ThreadRequest{
		Messages: []openai.ThreadMessage{},
		Metadata: map[string]interface{}{
			"dialogId": fmt.Sprintf("%d", dialogId),
		},
	})
	if err != nil {
		return fmt.Errorf("не удалось создать тред: %w", err)
	}

	// Сохраняем новый тред в карте тредов пользователя
	respModel.TreadsGPT[dialogId] = &th
	// Поскольку мы работаем с указателем, нет необходимости обновлять значение в m.responders

	return nil
}

func (m *Models) GetOrSetRespGPT(assist Assistant, dialogId, respId uint64, respName string) (*RespModel, error) {
	// Сначала проверяем кэш
	if val, ok := m.responders.Load(dialogId); ok {
		// Если пользователь найден в кэше
		respModel := val.(*RespModel)
		respModel.mu.Lock()
		respModel.TTL = time.Now().Add(m.UserModelTTl) // Обновляем TTL
		respModel.mu.Unlock()
		logger.Info("dialogId %d found in cache, TTL updated", dialogId, assist.UserId)
		return respModel, nil
	}
	// Если пользователь не найден в кэше, создаем новую запись
	userCtx, cancel := context.WithCancel(context.Background())

	// Создаем новый RespModel и добавляем в кэш
	user := &RespModel{
		Assist:   assist,
		RespName: respName,
		TTL:      time.Now().Add(m.UserModelTTl * time.Minute), // Устанавливаем TTL
		Chan:     make(map[uint64]Ch),
		Services: Services{Listener: false, Respondent: false},
		Ctx:      userCtx,
		Cancel:   cancel,
	}

	// Добавляем новый Ch в map
	user.Chan[respId] = Ch{
		TxCh:     make(chan Message, 1),
		RxCh:     make(chan Message, 1),
		UserId:   assist.UserId,
		DialogId: dialogId,
		RespName: respName,
	}

	// Читаю контекст из базы данных
	contextData, err := m.db.ReadContext(dialogId)
	if err != nil {
		//log.Printf("ошибка чтения контекста для dialogId %d: %v", dialogId, err)
		// Проверяем тип ошибки
		if strings.Contains(err.Error(), "получены пустые данные") {
			// Для нового диалога это нормально - инициализируем структуры без ошибки
			logger.Info("Инициализация нового диалога %d", dialogId, assist.UserId)
			user.TreadsGPT = make(map[uint64]*openai.Thread)
		} else {
			// Это реальная ошибка - логируем
			logger.Error("ошибка чтения контекста для dialogId %d: %v", dialogId, err, assist.UserId)
		}
	} else {
		// Если есть данные, десериализуем их
		var threadMap *openai.Thread
		err = json.Unmarshal(contextData, &threadMap)
		if err != nil {
			logger.Error("ошибка десериализации контекста для dialogId %d: %v", dialogId, err, assist.UserId)
			return nil, err
		}

		user.TreadsGPT = make(map[uint64]*openai.Thread)
		user.TreadsGPT[dialogId] = threadMap
	}

	m.responders.Store(dialogId, user)

	//fmt.Printf("dialogId %d cached successfully with TTL %v minutes.\n", dialogId, mode.UserModelTTl)

	// Сигнализируем ожидающим горутинам
	if waitChIface, exists := m.waitChannels.Load(respId); exists {
		waitCh := waitChIface.(chan struct{})
		close(waitCh)
		m.waitChannels.Delete(respId)
	}

	return user, nil
}

func (m *Models) GetCh(respId uint64) (Ch, error) {
	// Проверяем, есть ли уже канал для ожидания этого responderId
	waitChInterface, exists := m.waitChannels.Load(respId)
	var waitCh chan struct{}

	if !exists {
		// Создаем канал ожидания, если его нет
		waitCh = make(chan struct{})
		m.waitChannels.Store(respId, waitCh)
	} else {
		waitCh = waitChInterface.(chan struct{})
	}

	// Пробуем получить канал
	userCh, err := m.getTryCh(respId)
	if err == nil {
		return userCh, nil
	}

	// Если канала нет, ждем сигнала с таймаутом
	select {
	case <-waitCh:
		// Сигнал получен, пробуем снова
		return m.getTryCh(respId)
	case <-time.After(1 * time.Second):
		return Ch{}, fmt.Errorf("тайм-аут при ожидании канала для responderId %d", respId)
	}
}

// Метод для проверки существования канала
func (m *Models) getTryCh(respId uint64) (Ch, error) {
	var userCh Ch
	var found bool

	m.responders.Range(func(key, value interface{}) bool {
		// Используем указатель на RespModel вместо копирования структуры
		model, ok := value.(*RespModel)
		if !ok {
			return true
		}

		// Безопасно читаем Chan с использованием блокировки
		model.mu.RLock()
		ch, exists := model.Chan[respId]
		model.mu.RUnlock()

		if exists {
			userCh = ch
			found = true
			return false
		}
		return true
	})

	if found {
		return userCh, nil
	}
	return Ch{}, fmt.Errorf("respModel не найден для responderId %d", respId)
}

func (m *Models) CleanDialogData(dialogId uint64) {
	// Получаем RespModel из структуры Models
	val, ok := m.responders.Load(dialogId)
	if !ok {
		logger.Warn("RespModel не найден для dialogId %d", dialogId)
		return
	}

	// Приведение типа к указателю на RespModel
	respModel := val.(*RespModel)

	// Получаем userId для логирования
	userId := respModel.Assist.UserId

	// Блокируем доступ к полям RespModel для чтения
	respModel.mu.RLock()
	tread := respModel.TreadsGPT[dialogId]
	respModel.mu.RUnlock()

	// Отменяем активные runs перед сохранением и очисткой
	if tread != nil {
		if err := m.cancelActiveRuns(tread.ID); err != nil {
			logger.Warn("Ошибка при отмене активных runs для dialogId %d: %v", dialogId, err, userId)
		}
	}

	// Сохраняем контекст модели
	threadsJSON, err := json.Marshal(tread)
	if err != nil {
		logger.Error("ошибка сериализации контекста для treadId %v: %v", tread, err, userId)
		return
	}

	err = m.db.SaveContext(dialogId, threadsJSON)
	if err != nil {
		logger.Error("ошибка сохранения контекста для dialogId %d: %v", dialogId, err, userId)
	}

	// Вызов функции Cancel, если она существует
	if respModel.Cancel != nil {
		// Блокируем доступ к полям RespModel для записи при закрытии каналов
		respModel.mu.Lock()
		// Закрытие всех каналов в TxCh
		for respId, ch := range respModel.Chan {
			safeClose(ch.TxCh)
			safeClose(ch.RxCh)
			//log.Printf("Каналы закрыты для responderId %d", respId)
			// Удаляем канал из TxCh
			delete(respModel.Chan, respId)
		}
		respModel.mu.Unlock()

		//log.Printf("Channels closed for dialogId %d", dialogId)

		// Освобождаю контекст и завершаю горутины
		respModel.Cancel()
		// Удаляю запись из кэша
		//respModel.activeOps.Wait() // ТЕСТИРОВАТЬ
		m.responders.Delete(dialogId)

		//log.Printf("Контекст отменен для dialogId %d", dialogId)
	} else {
		logger.Error("Контекст не найден для dialogId %d", dialogId, userId)
	}
}

// safeClose закрывает канал и обрабатывает панику, если канал уже закрыт
func safeClose(ch chan Message) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Паника при закрытии канала: %v", r)
		}
	}()
	close(ch)
}

// CleanUp Удаление устаревших записей
func (m *Models) CleanUp() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()

			m.responders.Range(func(key, value interface{}) bool {
				responder := value.(*RespModel) // Используем указатель вместо копирования

				// Используем блокировку для безопасного чтения TTL
				responder.mu.RLock()
				ttlExpired := responder.TTL.Before(now)
				responder.mu.RUnlock()

				if ttlExpired {
					dialogId, ok := key.(uint64)
					if !ok {
						logger.Error("Некорректный тип ключа: %T, ожидался uint64", key)
						return true
					}

					// Очищаем данные и закрываем каналы
					m.CleanDialogData(dialogId)
					// m.responders.Delete не требуется, так как уже вызывается в CleanDialogData

					logger.Info("Удалена просроченная запись для dialogId %d\n", dialogId)
				}
				return true
			})
		}
	}
}

// cancelActiveRuns отменяет все активные runs в треде
func (m *Models) cancelActiveRuns(threadID string) error {
	// Получаем список активных runs
	runsList, err := m.client.ListRuns(m.ctx, threadID, openai.Pagination{
		Limit: func(i int) *int { return &i }(20),
	})
	if err != nil {
		return fmt.Errorf("не удалось получить список runs: %w", err)
	}

	// Отменяем активные runs
	for _, run := range runsList.Runs {
		if run.Status == openai.RunStatusQueued ||
			run.Status == openai.RunStatusInProgress ||
			run.Status == openai.RunStatusRequiresAction {

			logger.Debug("Отменяю активный run %s со статусом %s", run.ID, run.Status)

			_, err := m.client.CancelRun(m.ctx, threadID, run.ID)
			if err != nil {
				logger.Warn("Не удалось отменить run %s: %v", run.ID, err)
				continue
			}

			// Ждем отмены run
			if err := m.waitForRunCancellation(threadID, run.ID); err != nil {
				logger.Warn("Ошибка при ожидании отмены run %s: %v", run.ID, err)
			}
		}
	}

	return nil
}

// waitForRunCancellation ждет отмены run
func (m *Models) waitForRunCancellation(threadID, runID string) error {
	maxRetries := 50 // 5 секунд максимум

	for i := 0; i < maxRetries; i++ {
		run, err := m.client.RetrieveRun(m.ctx, threadID, runID)
		if err != nil {
			return fmt.Errorf("не удалось получить статус run: %w", err)
		}

		if run.Status == openai.RunStatusCancelled ||
			run.Status == openai.RunStatusCompleted ||
			run.Status == openai.RunStatusFailed ||
			run.Status == openai.RunStatusExpired {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("превышено время ожидания отмены run %s", runID)
}

// SaveAllContextDuringExit сохраняет все контексты в БД при выходе
func (m *Models) SaveAllContextDuringExit() {
	m.responders.Range(func(key, value interface{}) bool {
		dialogId, ok := key.(uint64)
		if !ok {
			logger.Error("`SaveAllContextDuringExit` некорректный тип ключа: %T, ожидался uint64", key)
		}

		// Получаем RespModel из структуры Models
		val, ok := m.responders.Load(dialogId)
		if !ok {
			logger.Error("`SaveAllContextDuranteExit` RespModel не найден для dialogId %d", dialogId)
		}

		// Приведение типа к указателю на RespModel
		respModel := val.(*RespModel)

		// Блокируем для чтения
		respModel.mu.RLock()
		// Сохраняем контекст модели
		tread := respModel.TreadsGPT[dialogId]
		respModel.mu.RUnlock()

		threadsJSON, err := json.Marshal(tread)
		if err != nil {
			logger.Error("`SaveAllContextDuringExit` ошибка сериализации контекста для treadId %v: %v", tread, err)
		}
		// Сохраняю контекст модели
		err = m.db.SaveContext(dialogId, threadsJSON)
		if err != nil {
			logger.Error("`SaveAllContextDuranteExit` ошибка сохранения контекста для dialogId %d: %v", dialogId, err)
		}

		return true
	})
}

func (m *Models) TranscribeAudio(audioData []byte, fileName string) (string, error) {
	// Проверка наличия аудиоданных
	if len(audioData) == 0 {
		return "", fmt.Errorf("пустые аудиоданные")
	}

	// Создаем запрос для транскрибирования
	req := openai.AudioRequest{
		Model:    openai.Whisper1,
		FilePath: fileName,
		Reader:   bytes.NewReader(audioData),
	}

	// Транскрибируем аудио в текст
	resp, err := m.client.CreateTranscription(m.ctx, req)
	if err != nil {
		return "", fmt.Errorf("ошибка транскрибирования аудио: %w", err)
	}

	if resp.Text == "" {
		return "", fmt.Errorf("получен пустой текст при транскрибировании")
	}

	return resp.Text, nil
}

func (m *Models) uploadFiles(files []FileUpload) ([]string, error) {
	var fileIDs []string

	for _, file := range files {
		// Читаем содержимое файла в байты
		data, err := io.ReadAll(file.Content)
		if err != nil {
			return nil, fmt.Errorf("не удалось прочитать содержимое файла %s: %w", file.Name, err)
		}

		uploadReq := openai.FileBytesRequest{
			Name:    file.Name,
			Bytes:   data,
			Purpose: openai.PurposeAssistants,
		}

		uploadedFile, err := m.client.CreateFileBytes(m.ctx, uploadReq)
		if err != nil {
			return nil, fmt.Errorf("не удалось загрузить файл %s: %w", file.Name, err)
		}

		fileIDs = append(fileIDs, uploadedFile.ID)
	}

	return fileIDs, nil
}

func (m *Models) cleanupFiles(fileIDs []string) {
	for _, fileID := range fileIDs {
		err := m.client.DeleteFile(m.ctx, fileID)
		if err != nil {
			logger.Warn("не удалось удалить файл %s: %v", fileID, err)
		}
	}
}
