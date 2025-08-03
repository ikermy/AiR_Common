package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/handler"
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
	Shutdown()
}

type DB interface {
	ReadContext(dialogId uint64) (json.RawMessage, error)
	SaveContext(treadId uint64, context json.RawMessage) error
}

type Models struct {
	ctx           context.Context
	cancel        context.CancelFunc
	client        *openai.Client
	db            DB
	responders    sync.Map // Хранит указатели на RespModel, а не сами структуры
	waitChannels  sync.Map // Хранит каналы для синхронизации горутин
	UserModelTTl  time.Duration
	actionHandler ActionHandler
	shutdownOnce  sync.Once    // Гарантирует однократное выполнение shutdown
	shutdownMu    sync.RWMutex // Мьютекс для безопасного доступа к флагу
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

func New(conf *conf.Conf, d DB) *Models {
	ctx, cancel := context.WithCancel(context.Background())
	return &Models{
		ctx:           ctx,
		cancel:        cancel,
		client:        openai.NewClient(conf.GPT.Key),
		db:            d,
		responders:    sync.Map{},
		waitChannels:  sync.Map{},
		UserModelTTl:  time.Duration(conf.GLOB.UserModelTTl) * time.Minute,
		actionHandler: &handler.ActionHandlerOpenAI{},
	}
}

// getAssistantVectorStore получает векторное хранилище ассистента
func (m *Models) getAssistantVectorStore(assistantID string) (*openai.VectorStore, error) {
	assistant, err := m.client.RetrieveAssistant(m.ctx, assistantID)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить ассистента: %w", err)
	}

	// Проверяем, есть ли привязанные векторные хранилища
	if assistant.ToolResources != nil &&
		assistant.ToolResources.FileSearch != nil &&
		len(assistant.ToolResources.FileSearch.VectorStoreIDs) > 0 {

		vectorStoreID := assistant.ToolResources.FileSearch.VectorStoreIDs[0]
		vectorStore, err := m.client.RetrieveVectorStore(m.ctx, vectorStoreID)
		if err != nil {
			return nil, fmt.Errorf("не удалось получить векторное хранилище: %w", err)
		}

		return &vectorStore, nil
	}

	return nil, fmt.Errorf("у ассистента нет привязанного векторного хранилища")
}

// addFilesToVectorStore добавляет файлы в векторное хранилище
func (m *Models) addFilesToVectorStore(vectorStoreID string, fileIDs []string) error {
	for _, fileID := range fileIDs {
		_, err := m.client.CreateVectorStoreFile(m.ctx, vectorStoreID, openai.VectorStoreFileRequest{
			FileID: fileID,
		})
		if err != nil {
			logger.Error("Не удалось добавить файл %s в векторное хранилище: %v", fileID, err)
			continue
		}

		// Ожидаем завершения обработки файла в векторном хранилище
		err = m.waitForFileProcessing(vectorStoreID, fileID)
		if err != nil {
			logger.Warn("Файл %s не был полностью обработан в векторном хранилище: %v", fileID, err)
			// Продолжаем работу, даже если файл не обработался
			// Всё равно все файлы потом удалим по их IDs
		}
	}
	return nil
}

// waitForFileProcessing ожидает завершения обработки файла в векторном хранилище
func (m *Models) waitForFileProcessing(vectorStoreID, fileID string) error {
	maxRetries := 60
	retryDelay := 1 * time.Second

	for i := 0; i < maxRetries; i++ {
		// Получает статус файла в векторном хранилище
		vectorStoreFile, err := m.client.RetrieveVectorStoreFile(
			context.Background(),
			vectorStoreID,
			fileID,
		)
		if err != nil {
			logger.Warn("Ошибка получения статуса файла %s: %v", fileID, err)
			time.Sleep(retryDelay)
			continue
		}

		//logger.Debug("Статус файла %s в векторном хранилище: %s", fileID, vectorStoreFile.Status)

		switch vectorStoreFile.Status {
		case "completed":
			//logger.Debug("Файл %s успешно обработанно векторном хранилище %s", fileID, vectorStoreID)
			return nil
		case "failed":
			return fmt.Errorf("обработка файла %s в векторном хранилище завершилась неудачей", fileID)
		case "cancelled":
			return fmt.Errorf("обработка файла %s в векторном хранилище была отменена", fileID)
		case "in_progress":
			// Продолжаем ожидание
			time.Sleep(retryDelay)
			continue
		default:
			logger.Warn("Неизвестный статус файла %s: %s", fileID, vectorStoreFile.Status)
			time.Sleep(retryDelay)
			continue
		}
	}

	return fmt.Errorf("превышено время ожидания обработки файла %s", fileID)
}

// uploadFilesForAssistant загружает файлы и добавляет их в векторное хранилище ассистента
func (m *Models) uploadFilesForAssistant(files []FileUpload, vectorStore *openai.VectorStore) ([]string, []string, error) {
	if len(files) == 0 {
		return nil, nil, nil
	}

	// Загружаем файлы в OpenAI
	fileIDs, err := m.uploadFiles(files)
	if err != nil {
		return nil, nil, fmt.Errorf("не удалось загрузить файлы: %w", err)
	}

	// Извлекаем имена файлов
	var fileNames []string
	for _, file := range files {
		fileNames = append(fileNames, file.Name)
	}

	// Добавляем файлы в векторное хранилище
	err = m.addFilesToVectorStore(vectorStore.ID, fileIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("Не удалось добавить файлы в векторное хранилище: %v", err)
	}

	return fileIDs, fileNames, nil
}

// GetFileAsReader получает файл в виде io.Reader из векторного хранилища или по URL
func (m *Models) GetFileAsReader(url string) (io.Reader, error) {
	// Проверяем, что у нас есть источник файла
	if url == "" {
		return nil, fmt.Errorf("не указан источник файла: отсутствуют URL")
	}

	// Проверяем, это файл из OpenAI
	if strings.HasPrefix(url, "openai_file:") {
		fileID := strings.TrimPrefix(url, "openai_file:")
		content, err := m.downloadFileFromOpenAI(fileID)
		if err != nil {
			return nil, fmt.Errorf("ошибка получения файла из OpenAI: %w", err)
		}
		return bytes.NewReader(content), nil
	}

	// Обычная загрузка по HTTP
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

			// ПРОТЕСТИРОВАТЬ!
			//logger.Debug("Отменяю активный run %s со статусом %s", run.ID, run.Status)

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

func (m *Models) cleanupFiles(fileIDs []string, vectorStoreID ...string) {
	for _, fileID := range fileIDs {
		// Сначала удаляем из векторного хранилища, если оно указанно
		if len(vectorStoreID) > 0 && vectorStoreID[0] != "" {
			err := m.client.DeleteVectorStoreFile(m.ctx, vectorStoreID[0], fileID)
			if err != nil {
				// Проверяем, является ли ошибка 404 (файл не найден)
				if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "Not found") {
					//logger.Debug("Файл %s уже удален из векторного хранилища %s", fileID, vectorStoreID[0])
				} else {
					logger.Warn("не удалось удалить файл %s из векторного хранилища %s: %v", fileID, vectorStoreID[0], err)
				}
			}
		}

		// Затем удаляю из общего хранилища
		err := m.client.DeleteFile(m.ctx, fileID)
		if err != nil {
			// Аналогично для общего хранилища
			if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "Not found") {
				//logger.Debug("Файл %s уже удален из общего хранилища", fileID)
			} else {
				logger.Warn("не удалось удалить файл %s из общего хранилища: %v", fileID, err)
			}
		}
	}
}

// Метод для получения файлов из ответа ассистента
func (m *Models) extractGeneratedFiles(ctx context.Context, run *openai.Run) ([]string, error) {
	order := "desc"
	messagesList, err := m.client.ListMessage(ctx, run.ThreadID, nil, &order, nil, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить сообщения: %w", err)
	}

	var generatedFileIDs []string

	for _, message := range messagesList.Messages {
		if message.Role == "assistant" && int64(message.CreatedAt) >= run.CreatedAt {
			for _, content := range message.Content {
				// Проверяем файлы в содержимом сообщения
				if content.ImageFile != nil {
					generatedFileIDs = append(generatedFileIDs, content.ImageFile.FileID)
				}
			}

			for _, fileID := range message.FileIds {
				generatedFileIDs = append(generatedFileIDs, fileID)
			}
		}
	}

	return generatedFileIDs, nil
}

// downloadFileFromOpenAI скачивает файл из OpenAI и возвращает данные
func (m *Models) downloadFileFromOpenAI(fileID string) ([]byte, error) {
	rawResponse, err := m.client.GetFileContent(m.ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("не удалось скачать содержимое файла: %w", err)
	}
	defer rawResponse.Close()

	// Читаем содержимое из RawResponse
	content, err := io.ReadAll(rawResponse)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать содержимое файла: %w", err)
	}

	return content, nil
}

// Shutdown корректно завершает работу модуля, отменяет все операции и сохраняет контексты
func (m *Models) Shutdown() {
	var shutdownErrors []string

	m.shutdownOnce.Do(func() {
		logger.Info("Начинается процесс завершения работы модуля Models")

		// Устанавливаем флаг завершения работы

		// Отменяем главный контекст, что приведет к остановке всех операций
		if m.cancel != nil {
			m.cancel()
		}

		// Ждем немного, что бы активные операции могли корректно завершиться
		time.Sleep(500 * time.Millisecond)

		// Отменяем все активные runs в тредах перед сохранением
		if err := m.cancelAllActiveRuns(); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Sprintf("ошибка отмены активных runs: %v", err))
		}

		// Сохраняем все контексты в базу данных
		logger.Info("Сохранение всех контекстов при завершении работы")
		if err := m.saveAllContextsGracefully(); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Sprintf("ошибка сохранения контекстов: %v", err))
		}

		// Закрываем все каналы и очищаем данные
		m.cleanupAllResponders()

		// Очищаем каналы ожидания
		m.cleanupWaitChannels()

		logger.Info("Процесс завершения работы модуля Models завершен")
	})

	if len(shutdownErrors) > 0 {
		logger.Error("ошибки при завершении работы: %s", strings.Join(shutdownErrors, "; "))
	}

	logger.Info("Модуль Models успешно завершил работу")
}

// cancelAllActiveRuns отменяет все активные runs во всех тредах
func (m *Models) cancelAllActiveRuns() error {
	logger.Info("Отмена всех активных runs")

	var cancelErrors []string

	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		respModel.mu.RLock()
		for _, thread := range respModel.TreadsGPT {
			if thread != nil {
				if err := m.cancelActiveRuns(thread.ID); err != nil {
					cancelErrors = append(cancelErrors, fmt.Sprintf("треда %s: %v", thread.ID, err))
				}
			}
		}
		respModel.mu.RUnlock()

		return true
	})

	if len(cancelErrors) > 0 {
		return fmt.Errorf("ошибки отмены runs: %s", strings.Join(cancelErrors, "; "))
	}

	return nil
}

// saveAllContextsGracefully сохраняет все контексты с обработкой ошибок
func (m *Models) saveAllContextsGracefully() error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var saveErrors []string

	m.responders.Range(func(key, value interface{}) bool {
		dialogId, ok := key.(uint64)
		if !ok {
			mu.Lock()
			saveErrors = append(saveErrors, fmt.Sprintf("некорректный тип ключа: %T, ожидался uint64", key))
			mu.Unlock()
			return true
		}

		wg.Add(1)
		go func(dId uint64, respModel *RespModel) {
			defer wg.Done()

			respModel.mu.RLock()
			thread := respModel.TreadsGPT[dId]
			respModel.mu.RUnlock()

			if thread != nil {
				threadsJSON, err := json.Marshal(thread)
				if err != nil {
					mu.Lock()
					saveErrors = append(saveErrors, fmt.Sprintf("сериализация для dialogId %d: %v", dId, err))
					mu.Unlock()
					return
				}

				err = m.db.SaveContext(dId, threadsJSON)
				if err != nil {
					mu.Lock()
					saveErrors = append(saveErrors, fmt.Sprintf("сохранение для dialogId %d: %v", dId, err))
					mu.Unlock()
				} else {
					logger.Debug("Контекст успешно сохранен для dialogId %d", dId)
				}
			}
		}(dialogId, value.(*RespModel))

		return true
	})

	// Ждём завершения всех операций сохранения с таймаутом
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Info("Все контексты успешно сохранены")
	case <-time.After(15 * time.Second):
		logger.Warn("Превышен таймаут при сохранении контекстов")
		saveErrors = append(saveErrors, "превышен таймаут сохранения контекстов (30 сек)")
	}

	if len(saveErrors) > 0 {
		return fmt.Errorf("ошибки сохранения: %s", strings.Join(saveErrors, "; "))
	}

	return nil
}

// cleanupAllResponders закрывает все каналы и очищает респондеры
func (m *Models) cleanupAllResponders() {
	logger.Info("Закрытие всех каналов и очистка респондеров")

	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		// Отменяем контекст респондера
		if respModel.Cancel != nil {
			respModel.Cancel()
		}

		// Закрываем все каналы
		respModel.mu.Lock()
		for respId, ch := range respModel.Chan {
			safeClose(ch.TxCh)
			safeClose(ch.RxCh)
			delete(respModel.Chan, respId)
			logger.Debug("Каналы закрыты для responderId %d", respId)
		}
		respModel.mu.Unlock()

		// Удаляем респондер из кэша
		m.responders.Delete(key)

		return true
	})
}

// cleanupWaitChannels очищает все каналы ожидания
func (m *Models) cleanupWaitChannels() {
	logger.Info("Очистка каналов ожидания")

	m.waitChannels.Range(func(key, value interface{}) bool {
		if ch, ok := value.(chan struct{}); ok {
			select {
			case <-ch:
				// Канал уже закрыт
			default:
				close(ch)
			}
		}
		m.waitChannels.Delete(key)
		return true
	})
}
