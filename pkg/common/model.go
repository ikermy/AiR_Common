package common

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/sashabaranov/go-openai"
	"log"
	"strings"
	"sync"
	"time"
)

// Model интерфейс для работы с моделями Assistant
type Model interface {
	GetOrSetRespGPT(assist Assistant, dialogId, respId uint64, respName string) (*RespModel, error)
	GetCh(respId uint64) (Ch, error)
	SaveAllContextDuringExit()
	Request(modelId string, dialogId uint64, text *string) (string, error)
	CleanDialogData(dialogId uint64)
	TranscribeAudio(audioData []byte, fileName string) (string, error)
}

type DB interface {
	ReadContext(dialogId uint64) (json.RawMessage, error)
	SaveContext(treadId uint64, context json.RawMessage) error
}

type Models struct {
	ctx          context.Context
	client       *openai.Client
	db           DB
	responders   sync.Map // Хранит указатели на RespModel, а не сами структуры
	waitChannels sync.Map // Хранит каналы для синхронизации горутин
	UserModelTTl time.Duration
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
	mu        sync.RWMutex // Мютекс для защиты полей структуры
	//activeOps sync.WaitGroup // ТЕСТИРОВАТЬ
}

type Services struct {
	Listener   bool
	Respondent bool
}

type Ch struct {
	TxCh     chan Message
	RxCh     chan Message
	UserId   uint32
	DialogId uint64
	RespName string
}

type Message struct {
	UserName  string    `json:"uname"` // Фактически не используется
	Type      string    `json:"type"`
	Content   string    `json:"content"`
	Name      string    `json:"name"`
	Token     string    `json:"token"`
	Timestamp time.Time `json:"timestamp"`
}

// StartCh структура для передачи данных для запуска слушателя
type StartCh struct {
	Model   RespModel
	Chanel  Ch
	TreadId uint64
	RespId  uint64
}

func NewMod(conf *conf.Conf, d DB) *Models {
	return &Models{
		client:       openai.NewClient(conf.GPT.Key),
		ctx:          context.Background(),
		db:           d,
		responders:   sync.Map{},
		waitChannels: sync.Map{},
		UserModelTTl: time.Duration(conf.GLOB.UserModelTTl) * time.Minute,
	}
}

func (m *Models) CreateThead(dialogId uint64) error {
	// Загружаем модель пользователя из sync.Map
	val, ok := m.responders.Load(dialogId)
	if !ok {
		return fmt.Errorf("RespModel не найден для userId %d", dialogId)
	}

	// Приведение типа к указателю на RespModel
	respModel := val.(*RespModel)

	//  ТЕСТИРОВАТЬ
	//respModel := val.(*RespModel)
	//if respModel.isClosing {
	//	return // Не начинаем новую операцию с закрывающимся объектом
	//}
	//
	//respModel.activeOps.Add(1)
	//defer respModel.activeOps.Done()

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

func createMsg(text *string) openai.MessageRequest {
	lastMessage := openai.MessageRequest{
		Role:    "user",
		Content: *text,
	}
	return lastMessage
}

func (m *Models) Request(modelId string, dialogId uint64, text *string) (string, error) {
	if *text == "" {
		return "", fmt.Errorf("пустое сообщение")
	}
	// Возможно стоит использовать канал о сигнализации о готовности треда
	//time.Sleep(1 * time.Second) // Протестировать!!!
	err := m.CreateThead(dialogId)
	if err != nil {
		// Логируем ошибку, но не прерываем выполнение, так как тред мог уже существовать
		log.Printf("не удалось создать тред: %v", err)
	}

	// Получение RespModel, который содержит информацию о тредах пользователя
	val, ok := m.responders.Load(dialogId)
	if !ok {
		return "", fmt.Errorf("RespModel не найден для dialogId %d", dialogId)
	}

	// Приведение типа к указателю на RespModel
	respModel := val.(*RespModel)

	// Используем блокировку для чтения, чтобы безопасно получить доступ к треду
	respModel.mu.RLock()
	thead, ok := respModel.TreadsGPT[dialogId]
	respModel.mu.RUnlock()

	if !ok || thead == nil {
		// Если тред не найден после попытки создания, возвращаем ошибку
		return "", fmt.Errorf("тред не найден для dialogId %d после попытки создания", dialogId)
	}

	_, err = m.client.CreateMessage(m.ctx, thead.ID, createMsg(text))
	if err != nil {
		return "", fmt.Errorf("не удалось создать сообщение: %w", err)
	}

	runRequest := openai.RunRequest{
		AssistantID: modelId,
	}

	run, err := m.client.CreateRun(m.ctx, thead.ID, runRequest)
	if err != nil {
		return "", fmt.Errorf("не удалось создать запуск: %w", err)
	}

	// Опрашиваем статус запуска, пока он не будет завершен
	for run.Status == openai.RunStatusQueued || run.Status == openai.RunStatusInProgress {
		run, err = m.client.RetrieveRun(m.ctx, run.ThreadID, run.ID)
		if err != nil {
			return "", fmt.Errorf("не удалось получить статус запуска: %w", err)
		}
		time.Sleep(100 * time.Millisecond) // Небольшая задержка перед следующим опросом
	}
	if run.Status != openai.RunStatusCompleted {
		return "", fmt.Errorf("запуск завершился неудачно со статусом %s", run.Status)
	}

	// Получаем список сообщений из треда
	numMessages := 1 // Нас интересует только последнее сообщение
	order := "desc"  // Сортировка по убыванию, чтобы последнее сообщение было первым
	messagesList, err := m.client.ListMessage(m.ctx, run.ThreadID, &numMessages, &order, nil, nil, nil)
	if err != nil {
		return "", fmt.Errorf("не удалось получить список сообщений: %w", err)
	}

	if len(messagesList.Messages) == 0 || len(messagesList.Messages[0].Content) == 0 {
		return "", fmt.Errorf("получен пустой список сообщений или пустое содержимое сообщения")
	}

	return messagesList.Messages[0].Content[0].Text.Value, nil
}

func (m *Models) GetOrSetRespGPT(assist Assistant, dialogId, respId uint64, respName string) (*RespModel, error) {
	// Сначала проверяем кэш
	if val, ok := m.responders.Load(dialogId); ok {
		// Если пользователь найден в кэше
		respModel := val.(*RespModel)
		respModel.mu.Lock()
		respModel.TTL = time.Now().Add(m.UserModelTTl) // Обновляем TTL
		respModel.mu.Unlock()
		log.Printf("dialogId %d found in cache, TTL updated.\n", dialogId)
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
			log.Printf("Инициализация нового диалога %d", dialogId)
			user.TreadsGPT = make(map[uint64]*openai.Thread)
		} else {
			// Это реальная ошибка - логируем
			log.Printf("ошибка чтения контекста для dialogId %d: %v", dialogId, err)
		}
	} else {
		// Если есть данные, десериализуем их
		var threadMap *openai.Thread
		err = json.Unmarshal(contextData, &threadMap)
		if err != nil {
			log.Printf("ошибка десериализации контекста для dialogId %d: %v", dialogId, err)
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
		log.Printf("RespModel не найден для dialogId %d", dialogId)
		return
	}

	// Приведение типа к указателю на RespModel
	respModel := val.(*RespModel)

	// Блокируем доступ к полям RespModel для чтения
	respModel.mu.RLock()
	tread := respModel.TreadsGPT[dialogId]
	respModel.mu.RUnlock()

	// Сохраняем контекст модели
	threadsJSON, err := json.Marshal(tread)
	if err != nil {
		log.Printf("ошибка сериализации контекста для treadId %v: %v", tread, err)
		return
	}

	err = m.db.SaveContext(dialogId, threadsJSON)
	if err != nil {
		log.Printf("ошибка сохранения контекста для dialogId %d: %v", dialogId, err)
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
		log.Printf("Контекст не найден для dialogId %d", dialogId)
	}
}

// safeClose закрывает канал и обрабатывает панику, если канал уже закрыт
func safeClose(ch chan Message) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("Паника при закрытии канала: %v", r)
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
						log.Printf("Некорректный тип ключа: %T, ожидался uint64", key)
						return true
					}

					// Очищаем данные и закрываем каналы
					m.CleanDialogData(dialogId)
					// m.responders.Delete не требуется, так как уже вызывается в CleanDialogData

					log.Printf("Удалена просроченная запись для dialogId %d\n", dialogId)
				}
				return true
			})
		}
	}
}

// SaveAllContextDuringExit сохраняет все контексты в БД при выходе
func (m *Models) SaveAllContextDuringExit() {
	m.responders.Range(func(key, value interface{}) bool {
		dialogId, ok := key.(uint64)
		if !ok {
			log.Printf("`SaveAllContextDuringExit` некорректный тип ключа: %T, ожидался uint64", key)
		}

		// Получаем RespModel из структуры Models
		val, ok := m.responders.Load(dialogId)
		if !ok {
			log.Printf("`SaveAllContextDuringExit` RespModel не найден для dialogId %d", dialogId)
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
			log.Printf("`SaveAllContextDuringExit` ошибка сериализации контекста для treadId %v: %v", tread, err)
		}
		// Сохраняю контекст модели
		err = m.db.SaveContext(dialogId, threadsJSON)
		if err != nil {
			log.Printf("`SaveAllContextDuranteExit` ошибка сохранения контекста для dialogId %d: %v", dialogId, err)
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
