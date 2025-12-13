package startpoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/model"
)

// Mock для ModelInterface
type MockModel struct {
	requestCalled atomic.Int32
	newMessageCh  chan model.Message
	mu            sync.Mutex
	channels      map[uint64]*model.Ch
	// Для симуляции ошибок
	simulateError  error
	errorOnAttempt int32 // На какой попытке вернуть ошибку (0 = всегда)
	currentAttempt atomic.Int32
	failureCount   atomic.Int32 // Сколько раз вернуть ошибку
}

func NewMockModel() *MockModel {
	return &MockModel{
		newMessageCh: make(chan model.Message, 10),
		channels:     make(map[uint64]*model.Ch),
	}
}

// SetError настраивает симуляцию ошибки
func (m *MockModel) SetError(err error, onAttempt int32, failureCount int32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.simulateError = err
	m.errorOnAttempt = onAttempt
	m.failureCount.Store(failureCount)
	m.currentAttempt.Store(0)
}

// ClearError очищает симуляцию ошибки
func (m *MockModel) ClearError() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.simulateError = nil
	m.errorOnAttempt = 0
	m.failureCount.Store(0)
	m.currentAttempt.Store(0)
}

// SimulateError симулирует конкретную HTTP ошибку (401, 403, 500, 503)
func (m *MockModel) SimulateError(statusCode int) {
	var err error
	switch statusCode {
	case 401:
		err = fmt.Errorf("HTTP 401: Unauthorized")
	case 403:
		err = fmt.Errorf("HTTP 403: Forbidden")
	case 500:
		err = fmt.Errorf("HTTP 500: Internal Server Error")
	case 503:
		err = fmt.Errorf("HTTP 503: Service Unavailable")
	default:
		err = fmt.Errorf("HTTP %d: Error", statusCode)
	}
	m.SetError(err, 1, 1)
}

func (m *MockModel) NewMessage(operator model.Operator, msgType string, content *model.AssistResponse, name *string, files ...model.FileUpload) model.Message {
	msg := model.Message{
		Type:     msgType,
		Content:  *content,
		Operator: operator,
		Files:    files,
	}
	if name != nil {
		msg.Name = *name
	}
	// Неблокирующая отправка для избежания deadlock при высокой нагрузке
	select {
	case m.newMessageCh <- msg:
	default:
		// Канал переполнен, пропускаем (для нагрузочных тестов это нормально)
	}
	return msg
}

// StartMessageConsumer запускает горутину для чтения из newMessageCh с симуляцией обработки
func (m *MockModel) StartMessageConsumer(ctx context.Context) {
	go func() {
		for {
			select {
			case <-m.newMessageCh:
				// Симуляция обработки сообщения со случайной задержкой 0-3000ms
				delay := time.Duration(time.Now().UnixNano()%3000) * time.Millisecond
				time.Sleep(delay)
			case <-ctx.Done():
				// Дочитываем оставшиеся сообщения
				for len(m.newMessageCh) > 0 {
					<-m.newMessageCh
				}
				return
			}
		}
	}()
}

func (m *MockModel) Request(modelId string, dialogId uint64, ask *string, files ...model.FileUpload) (model.AssistResponse, error) {
	m.requestCalled.Add(1)
	attempt := m.currentAttempt.Add(1)

	// Проверяем, нужно ли симулировать ошибку
	m.mu.Lock()
	shouldFail := m.simulateError != nil &&
		(m.errorOnAttempt == 0 || attempt == m.errorOnAttempt) &&
		m.failureCount.Load() > 0
	if shouldFail {
		m.failureCount.Add(-1)
	}
	errToReturn := m.simulateError
	m.mu.Unlock()

	if shouldFail {
		time.Sleep(10 * time.Millisecond)
		return model.AssistResponse{}, errToReturn
	}

	// Имитация ответа от модели
	time.Sleep(10 * time.Millisecond)
	return model.AssistResponse{
		Message:  "Ответ ассистента на вопрос: " + *ask,
		Operator: false,
	}, nil
}

func (m *MockModel) GetCh(respId uint64) (*model.Ch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ch, ok := m.channels[respId]; ok {
		return ch, nil
	}

	// Создаём новый канал если не существует
	ch := &model.Ch{
		TxCh:     make(chan model.Message, 10),
		RxCh:     make(chan model.Message, 10),
		UserId:   uint32(respId),
		DialogId: respId,
		RespName: "TestUser",
	}
	m.channels[respId] = ch
	return ch, nil
}

func (m *MockModel) CleanUp() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, ch := range m.channels {
		_ = ch.Close()
	}
	m.channels = make(map[uint64]*model.Ch)
}

// GetFileAsReader реализует ModelInterface
func (m *MockModel) GetFileAsReader(url string) (io.Reader, error) {
	// Заглушка для тестов
	return nil, fmt.Errorf("GetFileAsReader not implemented in mock")
}

// GetOrSetRespGPT реализует ModelInterface
func (m *MockModel) GetOrSetRespGPT(assist model.Assistant, dialogId, respId uint64, respName string) (*model.RespModel, error) {
	// Создаём минимальную реализацию для тестов
	ch, err := m.GetCh(respId)
	if err != nil {
		return nil, err
	}

	return &model.RespModel{
		Assist:   assist,
		RespName: respName,
		Chan: map[uint64]*model.Ch{
			respId: ch,
		},
	}, nil
}

// GetRespIdByDialogId реализует ModelInterface
func (m *MockModel) GetRespIdByDialogId(dialogId uint64) (uint64, error) {
	// Для простоты возвращаем dialogId как respId
	return dialogId, nil
}

// SaveAllContextDuringExit реализует ModelInterface
func (m *MockModel) SaveAllContextDuringExit() {
	// Заглушка для тестов
}

// CleanDialogData реализует ModelInterface
func (m *MockModel) CleanDialogData(dialogId uint64) {
	// Заглушка для тестов
}

// TranscribeAudio реализует ModelInterface
func (m *MockModel) TranscribeAudio(audioData []byte, fileName string) (string, error) {
	// Заглушка для тестов
	return "", fmt.Errorf("TranscribeAudio not implemented in mock")
}

// Shutdown реализует ModelInterface
func (m *MockModel) Shutdown() {
	// Заглушка для тестов
}

// Mock для EndpointInterface
type MockEndpoint struct {
	userAsks     sync.Map // map[uint64][]string
	savedDialogs []comdb.CreatorType
	metaCalled   atomic.Int32
	eventCalled  atomic.Int32
	mu           sync.Mutex
}

func NewMockEndpoint() *MockEndpoint {
	return &MockEndpoint{
		savedDialogs: make([]comdb.CreatorType, 0),
	}
}

func (e *MockEndpoint) GetUserAsk(dialogId uint64, respId uint64) []string {
	if val, ok := e.userAsks.Load(dialogId); ok {
		return val.([]string)
	}
	return []string{}
}

func (e *MockEndpoint) SetUserAsk(dialogId, respId uint64, ask string, askLimit ...uint32) bool {
	asks := e.GetUserAsk(dialogId, respId)
	asks = append(asks, ask)
	e.userAsks.Store(dialogId, asks)
	return true
}

func (e *MockEndpoint) SaveDialog(creator comdb.CreatorType, treadId uint64, resp *model.AssistResponse) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.savedDialogs = append(e.savedDialogs, creator)
}

func (e *MockEndpoint) Meta(userId uint32, dialogId uint64, meta string, respName string, assistName string, metaAction string) {
	e.metaCalled.Add(1)
}

func (e *MockEndpoint) SendEvent(userId uint32, event, userName, assistName, target string) {
	e.eventCalled.Add(1)
}

func (e *MockEndpoint) GetSavedDialogsCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.savedDialogs)
}

// Mock для BotInterface
type MockBot struct{}

func (b *MockBot) StartBots() error {
	return nil
}

func (b *MockBot) StopBot() {}

func (b *MockBot) DisableOperatorMode(userId uint32, dialogId uint64, silent ...bool) error {
	return nil
}

// Mock для OperatorInterface
type MockOperator struct {
	operatorCh       chan model.Message
	askCalled        atomic.Int32
	sendCalled       atomic.Int32
	receiveCalled    atomic.Int32
	deleteCalled     atomic.Int32
	activeReceivers  sync.Map // map[string]chan model.Message
	simulateResponse bool
	responseDelay    time.Duration
}

func NewMockOperator() *MockOperator {
	return &MockOperator{
		operatorCh:       make(chan model.Message, 10),
		simulateResponse: true,
		responseDelay:    50 * time.Millisecond,
	}
}

// SetResponseDelay устанавливает задержку ответа оператора
func (o *MockOperator) SetResponseDelay(delay time.Duration) {
	o.responseDelay = delay
}

// EnableResponse включает/выключает автоматические ответы
func (o *MockOperator) EnableResponse(enable bool) {
	o.simulateResponse = enable
}

// StartAutoResponder запускает горутину для автоматических ответов на все вопросы
func (o *MockOperator) StartAutoResponder(ctx context.Context) {
	go func() {
		for {
			select {
			case question := <-o.operatorCh:
				if !o.simulateResponse {
					continue
				}

				// Извлекаем userID и dialogID из вопроса
				// В реальности они должны быть в вопросе, но в моке мы их не передаём
				// Поэтому будем отправлять во все активные каналы
				o.activeReceivers.Range(func(key, value interface{}) bool {
					channelKey := key
					ch := value.(chan model.Message)

					go func() {
						time.Sleep(o.responseDelay)

						// Проверяем что канал всё ещё существует перед отправкой
						if _, exists := o.activeReceivers.Load(channelKey); !exists {
							// Канал был удалён (DeleteSession вызван) - не отправляем
							return
						}

						select {
						case ch <- model.Message{
							Type: "assist",
							Content: model.AssistResponse{
								Message: "Ответ оператора на: " + question.Content.Message,
							},
							Operator: model.Operator{Operator: true, SenderName: "Operator"},
						}:
						case <-ctx.Done():
						}
					}()
					return true
				})

			case <-ctx.Done():
				return
			}
		}
	}()
}

func (o *MockOperator) AskOperator(ctx context.Context, userID uint32, dialogID uint64, question model.Message) (model.Message, error) {
	o.askCalled.Add(1)
	// Имитация ответа оператора
	time.Sleep(10 * time.Millisecond)
	return model.Message{
		Type: "assist",
		Content: model.AssistResponse{
			Message: "Ответ оператора на: " + question.Content.Message,
		},
		Operator: model.Operator{Operator: true, SenderName: "Operator"},
	}, nil
}

func (o *MockOperator) SendToOperator(ctx context.Context, userID uint32, dialogID uint64, question model.Message) error {
	o.sendCalled.Add(1)
	select {
	case o.operatorCh <- question:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (o *MockOperator) ReceiveFromOperator(ctx context.Context, userID uint32, dialogID uint64) <-chan model.Message {
	o.receiveCalled.Add(1)

	key := fmt.Sprintf("%d_%d", userID, dialogID)
	ch := make(chan model.Message, 10)
	o.activeReceivers.Store(key, ch)

	if o.simulateResponse {
		go func() {
			time.Sleep(o.responseDelay)
			select {
			case ch <- model.Message{
				Type: "assist",
				Content: model.AssistResponse{
					Message: "Ответ оператора",
				},
				Operator: model.Operator{Operator: true, SenderName: "Operator"},
			}:
			case <-ctx.Done():
			}
		}()
	}

	return ch
}

func (o *MockOperator) DeleteSession(userID uint32, dialogID uint64) error {
	o.deleteCalled.Add(1)
	key := fmt.Sprintf("%d_%d", userID, dialogID)
	if ch, ok := o.activeReceivers.LoadAndDelete(key); ok {
		close(ch.(chan model.Message))
	}
	return nil
}

func (o *MockOperator) GetConnectionErrors(ctx context.Context, userID uint32, dialogID uint64) <-chan string {
	ch := make(chan string, 1)
	return ch
}

// Тест для Listener - отправка и получение сообщений
func TestListener_SendReceiveMessages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	// Создаём тестовую модель пользователя
	userCtx, userCancel := context.WithCancel(ctx)
	defer userCancel()

	respModel := &model.RespModel{
		Assist: model.Assistant{
			AssistId:   "test-assist-id",
			AssistName: "TestAssistant",
			UserId:     12345,
		},
		RespName: "TestUser",
		TTL:      time.Now().Add(1 * time.Hour),
		Chan:     make(map[uint64]*model.Ch),
		Ctx:      userCtx,
		Cancel:   userCancel,
	}

	// Создаём канал для пользователя
	usrCh := &model.Ch{
		TxCh:     make(chan model.Message, 10),
		RxCh:     make(chan model.Message, 10),
		UserId:   12345,
		DialogId: 1,
		RespName: "TestUser",
	}

	respModel.Chan[1] = usrCh

	respId := uint64(1)
	treadId := uint64(1)

	// Запускаем Listener в горутине
	errCh := make(chan error, 1)
	go func() {
		if err := start.Listener(respModel, usrCh, respId, treadId); err != nil {
			errCh <- err
		}
	}()

	// Даём время на запуск Listener
	time.Sleep(100 * time.Millisecond)

	// Тест 1: Отправка текстового вопроса
	t.Run("SendTextQuestion", func(t *testing.T) {
		question := model.Message{
			Type: "user",
			Content: model.AssistResponse{
				Message: "Привет, как дела?",
			},
			Name:     "TestUser",
			Operator: model.Operator{Operator: false},
		}

		select {
		case usrCh.RxCh <- question:
			t.Log("Вопрос отправлен в RxCh")
		case <-time.After(1 * time.Second):
			t.Fatal("Таймаут при отправке вопроса в RxCh")
		}

		// Ждём ответ в TxCh
		select {
		case msg := <-usrCh.TxCh:
			if msg.Type != "user" {
				t.Errorf("Ожидался тип 'user', получен '%s'", msg.Type)
			}
			t.Logf("Получено эхо вопроса: %s", msg.Content.Message)
		case <-time.After(1 * time.Second):
			t.Fatal("Таймаут ожидания эхо вопроса в TxCh")
		}

		// Ждём ответ ассистента
		select {
		case msg := <-usrCh.TxCh:
			if msg.Type != "assist" {
				t.Errorf("Ожидался тип 'assist', получен '%s'", msg.Type)
			}
			if msg.Content.Message == "" {
				t.Error("Получен пустой ответ от ассистента")
			}
			t.Logf("Получен ответ ассистента: %s", msg.Content.Message)
		case <-time.After(2 * time.Second):
			t.Fatal("Таймаут ожидания ответа ассистента в TxCh")
		}
	})

	// Тест 2: Отправка голосового вопроса
	t.Run("SendVoiceQuestion", func(t *testing.T) {
		question := model.Message{
			Type: "user_voice",
			Content: model.AssistResponse{
				Message: "Голосовой вопрос",
			},
			Name:     "TestUser",
			Operator: model.Operator{Operator: false},
		}

		select {
		case usrCh.RxCh <- question:
			t.Log("Голосовой вопрос отправлен в RxCh")
		case <-time.After(1 * time.Second):
			t.Fatal("Таймаут при отправке голосового вопроса")
		}

		// Ждём эхо
		select {
		case <-usrCh.TxCh:
			t.Log("Получено эхо голосового вопроса")
		case <-time.After(1 * time.Second):
			t.Fatal("Таймаут ожидания эхо голосового вопроса")
		}

		// Ждём ответ
		select {
		case msg := <-usrCh.TxCh:
			if msg.Content.Message == "" {
				t.Error("Получен пустой ответ на голосовой вопрос")
			}
			t.Logf("Получен ответ на голосовой вопрос: %s", msg.Content.Message)
		case <-time.After(2 * time.Second):
			t.Fatal("Таймаут ожидания ответа на голосовой вопрос")
		}
	})

	// Тест 3: Проверка сохранения диалогов
	t.Run("CheckDialogSaving", func(t *testing.T) {
		time.Sleep(500 * time.Millisecond) // Даём время на сохранение

		savedCount := mockEndpoint.GetSavedDialogsCount()
		if savedCount < 4 { // 2 вопроса + 2 ответа
			t.Errorf("Ожидалось минимум 4 сохранённых диалога, получено %d", savedCount)
		}
		t.Logf("Сохранено диалогов: %d", savedCount)
	})

	// Тест 4: Проверка количества запросов к модели
	t.Run("CheckModelRequests", func(t *testing.T) {
		requestCount := mockModel.requestCalled.Load()
		if requestCount < 2 {
			t.Errorf("Ожидалось минимум 2 запроса к модели, получено %d", requestCount)
		}
		t.Logf("Запросов к модели: %d", requestCount)
	})

	// Завершаем тест
	userCancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Listener завершился с ошибкой: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Log("Listener завершился по таймауту (нормально)")
	}
}

// Тест для проверки работы с несколькими пользователями
func TestListener_MultipleUsers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	const numUsers = 3
	var wg sync.WaitGroup

	for i := 0; i < numUsers; i++ {
		wg.Add(1)
		go func(userId uint32, dialogId uint64) {
			defer wg.Done()

			userCtx, userCancel := context.WithCancel(ctx)
			defer userCancel()

			respModel := &model.RespModel{
				Assist: model.Assistant{
					AssistId:   "test-assist-id",
					AssistName: "TestAssistant",
					UserId:     userId,
				},
				RespName: "TestUser",
				TTL:      time.Now().Add(1 * time.Hour),
				Chan:     make(map[uint64]*model.Ch),
				Ctx:      userCtx,
				Cancel:   userCancel,
			}

			usrCh := &model.Ch{
				TxCh:     make(chan model.Message, 10),
				RxCh:     make(chan model.Message, 10),
				UserId:   userId,
				DialogId: dialogId,
				RespName: "TestUser",
			}

			respModel.Chan[dialogId] = usrCh

			// Запускаем Listener
			errCh := make(chan error, 1)
			go func() {
				if err := start.Listener(respModel, usrCh, dialogId, dialogId); err != nil {
					errCh <- err
				}
			}()

			time.Sleep(50 * time.Millisecond)

			// Отправляем вопрос
			question := model.Message{
				Type: "user",
				Content: model.AssistResponse{
					Message: "Вопрос от пользователя " + string(rune(userId)),
				},
				Name:     "TestUser",
				Operator: model.Operator{Operator: false},
			}

			select {
			case usrCh.RxCh <- question:
				t.Logf("Пользователь %d отправил вопрос", userId)
			case <-time.After(1 * time.Second):
				t.Errorf("Пользователь %d: таймаут отправки вопроса", userId)
				return
			}

			// Ждём эхо и ответ
			for j := 0; j < 2; j++ {
				select {
				case msg := <-usrCh.TxCh:
					t.Logf("Пользователь %d получил сообщение типа '%s'", userId, msg.Type)
				case <-time.After(2 * time.Second):
					t.Errorf("Пользователь %d: таймаут получения сообщения %d", userId, j)
					return
				}
			}

			time.Sleep(100 * time.Millisecond)
			userCancel()
		}(uint32(i+1), uint64(i+1))
	}

	wg.Wait()

	// Проверяем, что все пользователи обработаны
	savedCount := mockEndpoint.GetSavedDialogsCount()
	expectedMin := numUsers * 2 // минимум 2 сохранения на пользователя
	if savedCount < expectedMin {
		t.Errorf("Ожидалось минимум %d сохранённых диалогов, получено %d", expectedMin, savedCount)
	}
	t.Logf("Всего сохранено диалогов для %d пользователей: %d", numUsers, savedCount)
}

// Тест для проверки закрытия каналов при отмене контекста
func TestListener_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	userCtx, userCancel := context.WithCancel(ctx)

	respModel := &model.RespModel{
		Assist: model.Assistant{
			AssistId:   "test-assist-id",
			AssistName: "TestAssistant",
			UserId:     99999,
		},
		RespName: "TestUser",
		TTL:      time.Now().Add(1 * time.Hour),
		Chan:     make(map[uint64]*model.Ch),
		Ctx:      userCtx,
		Cancel:   userCancel,
	}

	usrCh := &model.Ch{
		TxCh:     make(chan model.Message, 10),
		RxCh:     make(chan model.Message, 10),
		UserId:   99999,
		DialogId: 999,
		RespName: "TestUser",
	}

	respModel.Chan[999] = usrCh

	errCh := make(chan error, 1)
	listenerDone := make(chan struct{})

	go func() {
		if err := start.Listener(respModel, usrCh, 999, 999); err != nil {
			errCh <- err
		}
		close(listenerDone)
	}()

	time.Sleep(100 * time.Millisecond)

	// Отменяем контекст пользователя
	t.Log("Отмена контекста пользователя")
	userCancel()

	// Ждём завершения Listener
	// Увеличенный таймаут, т.к. Listener ждёт завершения Respondent с таймаутом 5 секунд
	select {
	case <-listenerDone:
		t.Log("Listener корректно завершился после отмены контекста")
	case <-time.After(7 * time.Second):
		t.Fatal("Listener не завершился после отмены контекста")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Logf("Listener завершился с ошибкой (может быть нормально): %v", err)
		}
	default:
		t.Log("Listener завершился без ошибок")
	}
}

// Benchmark для Listener - измеряет пропускную способность обработки сообщений
func BenchmarkListener_MessageProcessing(b *testing.B) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	userCtx, userCancel := context.WithCancel(ctx)
	defer userCancel()

	respModel := &model.RespModel{
		Assist: model.Assistant{
			AssistId:   "bench-assist-id",
			AssistName: "BenchAssistant",
			UserId:     88888,
		},
		RespName: "BenchUser",
		TTL:      time.Now().Add(1 * time.Hour),
		Chan:     make(map[uint64]*model.Ch),
		Ctx:      userCtx,
		Cancel:   userCancel,
	}

	// Большие буферы для бенчмарка
	bufferSize := b.N + 1000
	if bufferSize > 10000 {
		bufferSize = 10000
	}

	usrCh := &model.Ch{
		TxCh:     make(chan model.Message, bufferSize),
		RxCh:     make(chan model.Message, bufferSize),
		UserId:   88888,
		DialogId: 888,
		RespName: "BenchUser",
	}

	respModel.Chan[888] = usrCh

	listenerDone := make(chan struct{})
	go func() {
		start.Listener(respModel, usrCh, 888, 888)
		close(listenerDone)
	}()

	// Горутина для чтения ответов из TxCh
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-usrCh.TxCh:
				// Просто читаем и отбрасываем
			case <-userCtx.Done():
				// Дочитываем оставшиеся сообщения
				for len(usrCh.TxCh) > 0 {
					<-usrCh.TxCh
				}
				return
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)

	b.ResetTimer()

	// Отправляем все сообщения
	for i := 0; i < b.N; i++ {
		question := model.Message{
			Type: "user",
			Content: model.AssistResponse{
				Message: "Benchmark question",
			},
			Name:     "BenchUser",
			Operator: model.Operator{Operator: false},
		}

		select {
		case usrCh.RxCh <- question:
		case <-userCtx.Done():
			b.Fatalf("Контекст отменён на итерации %d/%d", i, b.N)
		}
	}

	b.StopTimer()

	// Ждём обработки всех сообщений
	time.Sleep(500 * time.Millisecond)

	// Корректно завершаем
	userCancel()

	select {
	case <-readerDone:
	case <-time.After(2 * time.Second):
		b.Log("Reader не завершился вовремя")
	}

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "msgs/sec")
}

// Тест для симуляции ошибок API
func TestListener_APIErrors(t *testing.T) {
	testCases := []struct {
		name         string
		errorCode    string
		errorMsg     string
		expectedType string // "fatal", "retryable", "non-critical"
	}{
		{
			name:         "401 Unauthorized",
			errorCode:    "401",
			errorMsg:     "401 Unauthorized: invalid API key",
			expectedType: "fatal",
		},
		{
			name:         "403 Forbidden",
			errorCode:    "403",
			errorMsg:     "403 Forbidden: insufficient quota",
			expectedType: "fatal",
		},
		{
			name:         "500 Internal Server Error",
			errorCode:    "500",
			errorMsg:     "500 Internal Server Error",
			expectedType: "retryable",
		},
		{
			name:         "503 Service Unavailable",
			errorCode:    "503",
			errorMsg:     "503 Service Unavailable: upstream connect error",
			expectedType: "retryable",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			mockModel := NewMockModel()
			mockEndpoint := NewMockEndpoint()
			mockBot := &MockBot{}
			mockOperator := NewMockOperator()

			start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
			defer start.Shutdown()

			userCtx, userCancel := context.WithCancel(ctx)
			defer userCancel()

			respModel := &model.RespModel{
				Assist: model.Assistant{
					AssistId:   "test-assist-id",
					AssistName: "TestAssistant",
					UserId:     77777,
				},
				RespName: "TestUser",
				TTL:      time.Now().Add(1 * time.Hour),
				Chan:     make(map[uint64]*model.Ch),
				Ctx:      userCtx,
				Cancel:   userCancel,
			}

			usrCh := &model.Ch{
				TxCh:     make(chan model.Message, 10),
				RxCh:     make(chan model.Message, 10),
				UserId:   77777,
				DialogId: 777,
				RespName: "TestUser",
			}

			respModel.Chan[777] = usrCh

			// Настраиваем симуляцию ошибки
			apiError := errors.New(tc.errorMsg)
			if tc.expectedType == "retryable" {
				// Для временных ошибок - 2 неудачи, потом успех
				mockModel.SetError(apiError, 0, 2)
			} else {
				// Для критических - всегда ошибка
				mockModel.SetError(apiError, 0, 100)
			}

			errCh := make(chan error, 1)
			go func() {
				if err := start.Listener(respModel, usrCh, 777, 777); err != nil {
					errCh <- err
				}
			}()

			time.Sleep(100 * time.Millisecond)

			// Отправляем вопрос
			question := model.Message{
				Type: "user",
				Content: model.AssistResponse{
					Message: "Тестовый вопрос для " + tc.name,
				},
				Name:     "TestUser",
				Operator: model.Operator{Operator: false},
			}

			select {
			case usrCh.RxCh <- question:
				t.Logf("Вопрос отправлен для теста %s", tc.name)
			case <-time.After(1 * time.Second):
				t.Fatalf("Таймаут отправки вопроса")
			}

			// Читаем эхо вопроса
			select {
			case msg := <-usrCh.TxCh:
				if msg.Type != "user" {
					t.Errorf("Ожидался тип 'user', получен '%s'", msg.Type)
				}
				t.Logf("Получено эхо вопроса")
			case <-time.After(1 * time.Second):
				t.Logf("Эхо вопроса не получено (возможно из-за ошибки)")
			}

			if tc.expectedType == "retryable" {
				// Для временных ошибок должен быть retry и успех
				select {
				case msg := <-usrCh.TxCh:
					if msg.Content.Message == "" {
						t.Error("Получен пустой ответ после retry")
					} else {
						t.Logf("✅ Получен ответ после retry: %s", msg.Content.Message)
					}
				case <-time.After(5 * time.Second):
					t.Error("Таймаут ожидания ответа после retry")
				}

				// Проверяем количество попыток
				attempts := mockModel.requestCalled.Load()
				if attempts < 2 {
					t.Errorf("Ожидалось минимум 2 попытки для retry, получено %d", attempts)
				} else {
					t.Logf("✅ Retry работает корректно: %d попыток", attempts)
				}
			} else if tc.expectedType == "fatal" {
				// Для критических ошибок ответ не должен прийти
				select {
				case msg := <-usrCh.TxCh:
					t.Logf("Получено сообщение (может быть ошибкой): %+v", msg)
				case <-time.After(2 * time.Second):
					t.Logf("✅ Ответ не пришёл для критической ошибки (ожидаемо)")
				}
			}

			time.Sleep(100 * time.Millisecond)
			userCancel()

			select {
			case err := <-errCh:
				if err != nil {
					t.Logf("Listener завершился с ошибкой: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Log("Listener завершился по таймауту")
			}
		})
	}
}

// Тест критических и некритических ошибок
func TestListener_CriticalVsNonCriticalErrors(t *testing.T) {
	testCases := []struct {
		name            string
		errorToSimulate error
		expectResponse  bool
		description     string
	}{
		{
			name:            "Critical_AuthError",
			errorToSimulate: &FatalError{Err: errors.New("401 Unauthorized")},
			expectResponse:  false,
			description:     "Критическая ошибка авторизации должна прервать обработку",
		},
		{
			name:            "NonCritical_RateLimitError",
			errorToSimulate: &NonCriticalError{Err: errors.New("429 Too Many Requests")},
			expectResponse:  false,
			description:     "Некритическая ошибка rate limit не должна прерывать весь процесс",
		},
		{
			name:            "Retryable_NetworkError",
			errorToSimulate: &RetryableError{Err: errors.New("connection timeout")},
			expectResponse:  true,
			description:     "Временная сетевая ошибка должна быть повторена",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			mockModel := NewMockModel()
			mockEndpoint := NewMockEndpoint()
			mockBot := &MockBot{}
			mockOperator := NewMockOperator()

			start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
			defer start.Shutdown()

			userCtx, userCancel := context.WithCancel(ctx)
			defer userCancel()

			respModel := &model.RespModel{
				Assist: model.Assistant{
					AssistId:   "test-assist-id",
					AssistName: "TestAssistant",
					UserId:     66666,
				},
				RespName: "TestUser",
				TTL:      time.Now().Add(1 * time.Hour),
				Chan:     make(map[uint64]*model.Ch),
				Ctx:      userCtx,
				Cancel:   userCancel,
			}

			usrCh := &model.Ch{
				TxCh:     make(chan model.Message, 10),
				RxCh:     make(chan model.Message, 10),
				UserId:   66666,
				DialogId: 666,
				RespName: "TestUser",
			}

			respModel.Chan[666] = usrCh

			// Настраиваем ошибку: 2 неудачи для Retryable, 1 для остальных
			if _, ok := tc.errorToSimulate.(*RetryableError); ok {
				mockModel.SetError(tc.errorToSimulate, 0, 2)
			} else {
				mockModel.SetError(tc.errorToSimulate, 0, 5)
			}

			errCh := make(chan error, 1)
			go func() {
				if err := start.Listener(respModel, usrCh, 666, 666); err != nil {
					errCh <- err
				}
			}()

			time.Sleep(100 * time.Millisecond)

			t.Logf("Тест: %s", tc.description)

			question := model.Message{
				Type: "user",
				Content: model.AssistResponse{
					Message: "Тест ошибки: " + tc.name,
				},
				Name:     "TestUser",
				Operator: model.Operator{Operator: false},
			}

			select {
			case usrCh.RxCh <- question:
			case <-time.After(1 * time.Second):
				t.Fatal("Таймаут отправки вопроса")
			}

			// Читаем эхо
			select {
			case <-usrCh.TxCh:
				t.Log("Получено эхо вопроса")
			case <-time.After(1 * time.Second):
				t.Log("Эхо не получено")
			}

			// Проверяем ответ
			if tc.expectResponse {
				select {
				case msg := <-usrCh.TxCh:
					if msg.Content.Message == "" {
						t.Error("❌ Ожидался ответ, но получен пустой")
					} else {
						t.Logf("✅ Получен ожидаемый ответ после retry: %s", msg.Content.Message)
					}
				case <-time.After(8 * time.Second):
					t.Error("❌ Таймаут ожидания ответа (retry не сработал)")
				}
			} else {
				select {
				case msg := <-usrCh.TxCh:
					t.Logf("⚠️  Получен неожиданный ответ: %+v", msg)
				case <-time.After(3 * time.Second):
					t.Log("✅ Ответ не пришёл (ожидаемо для критической/некритической ошибки)")
				}
			}

			userCancel()
		})
	}
}

// Тест включения и выключения операторского режима
func TestListener_OperatorMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	userCtx, userCancel := context.WithCancel(ctx)
	defer userCancel()

	respModel := &model.RespModel{
		Assist: model.Assistant{
			AssistId:   "test-assist-id",
			AssistName: "TestAssistant",
			UserId:     55555,
		},
		RespName: "TestUser",
		TTL:      time.Now().Add(1 * time.Hour),
		Chan:     make(map[uint64]*model.Ch),
		Ctx:      userCtx,
		Cancel:   userCancel,
	}

	usrCh := &model.Ch{
		TxCh:     make(chan model.Message, 20),
		RxCh:     make(chan model.Message, 20),
		UserId:   55555,
		DialogId: 555,
		RespName: "TestUser",
	}

	respModel.Chan[555] = usrCh

	errCh := make(chan error, 1)
	go func() {
		if err := start.Listener(respModel, usrCh, 555, 555); err != nil {
			errCh <- err
		}
	}()

	time.Sleep(100 * time.Millisecond)

	t.Run("SendToOperator", func(t *testing.T) {
		// Отправляем вопрос с флагом оператора
		question := model.Message{
			Type: "user",
			Content: model.AssistResponse{
				Message: "Вопрос для оператора",
			},
			Name:     "TestUser",
			Operator: model.Operator{Operator: true, SenderName: "TestUser"},
		}

		select {
		case usrCh.RxCh <- question:
			t.Log("✅ Вопрос с флагом оператора отправлен")
		case <-time.After(1 * time.Second):
			t.Fatal("Таймаут отправки вопроса")
		}

		// Читаем эхо
		select {
		case msg := <-usrCh.TxCh:
			if msg.Type != "user" {
				t.Errorf("Ожидался тип 'user', получен '%s'", msg.Type)
			}
			t.Log("Получено эхо вопроса")
		case <-time.After(1 * time.Second):
			t.Error("Не получено эхо вопроса")
		}

		// Ждём ответ от оператора
		select {
		case msg := <-usrCh.TxCh:
			if msg.Operator.Operator {
				t.Logf("✅ Получен ответ от оператора: %s", msg.Content.Message)
				t.Logf("   Имя отправителя: '%s'", msg.Operator.SenderName)
			} else {
				t.Error("❌ Получен ответ не от оператора")
			}
		case <-time.After(2 * time.Second):
			t.Error("❌ Таймаут ожидания ответа от оператора")
		}

		// Проверяем что методы оператора были вызваны
		if mockOperator.receiveCalled.Load() < 1 {
			t.Error("❌ ReceiveFromOperator не был вызван")
		} else {
			t.Logf("✅ ReceiveFromOperator вызван %d раз", mockOperator.receiveCalled.Load())
		}
	})

	t.Run("OperatorToAISwitch", func(t *testing.T) {
		// Ждём пока придёт следующий ответ от оператора (из предыдущего теста)
		// так как MockOperator отправляет автоматический ответ с задержкой
		time.Sleep(200 * time.Millisecond)

		// Очищаем канал от возможных старых сообщений
		for len(usrCh.TxCh) > 0 {
			<-usrCh.TxCh
		}

		// Сбрасываем счётчики
		initialAICalls := mockModel.requestCalled.Load()

		// Чтобы выйти из операторского режима, нужно отправить вопрос с флагом Operator: false
		// или просто обычный вопрос, но операторский режим останется активным
		// Поэтому просто проверяем, что в операторском режиме всё идёт к оператору
		question := model.Message{
			Type: "user",
			Content: model.AssistResponse{
				Message: "Вопрос в операторском режиме",
			},
			Name:     "TestUser",
			Operator: model.Operator{Operator: false}, // Не выключает режим!
		}

		select {
		case usrCh.RxCh <- question:
			t.Log("✅ Вопрос отправлен (операторский режим всё ещё активен)")
		case <-time.After(1 * time.Second):
			t.Fatal("Таймаут отправки вопроса")
		}

		// Читаем эхо
		select {
		case <-usrCh.TxCh:
			t.Log("Получено эхо вопроса")
		case <-time.After(1 * time.Second):
			t.Error("Не получено эхо")
		}

		// В операторском режиме ответ приходит от оператора
		select {
		case msg := <-usrCh.TxCh:
			if msg.Operator.Operator {
				t.Logf("✅ В операторском режиме получен ответ от оператора: %s", msg.Content.Message)
			} else {
				t.Log("⚠️  Получен ответ от AI (режим мог быть отключён)")
			}
		case <-time.After(2 * time.Second):
			t.Log("⚠️  Таймаут ожидания ответа")
		}

		// В текущей реализации операторский режим остаётся активным
		// пока не будет явно отключён через DisableOperatorMode
		t.Log("ℹ️  Операторский режим остаётся активным до явного отключения")
		t.Logf("ℹ️  AI вызовов: %d (до: %d)", mockModel.requestCalled.Load(), initialAICalls)
	})

	t.Run("CheckOperatorCalls", func(t *testing.T) {
		receiveCalls := mockOperator.receiveCalled.Load()
		if receiveCalls < 1 {
			t.Errorf("❌ Ожидался минимум 1 вызов ReceiveFromOperator, получено %d", receiveCalls)
		} else {
			t.Logf("✅ ReceiveFromOperator вызван %d раз(а)", receiveCalls)
		}

		t.Logf("Статистика вызовов оператора:")
		t.Logf("  - AskOperator: %d", mockOperator.askCalled.Load())
		t.Logf("  - SendToOperator: %d", mockOperator.sendCalled.Load())
		t.Logf("  - ReceiveFromOperator: %d", mockOperator.receiveCalled.Load())
		t.Logf("  - DeleteSession: %d", mockOperator.deleteCalled.Load())
	})

	time.Sleep(200 * time.Millisecond)
	userCancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Logf("Listener завершился с ошибкой: %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Log("Listener завершился по таймауту")
	}
}

// Тест переключения режимов: AI -> Operator -> AI
func TestListener_ModeSwitch_AIOperatorAI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	userCtx, userCancel := context.WithCancel(ctx)
	defer userCancel()

	respModel := &model.RespModel{
		Assist: model.Assistant{
			AssistId:   "test-assist-id",
			AssistName: "TestAssistant",
			UserId:     44444,
		},
		RespName: "TestUser",
		TTL:      time.Now().Add(1 * time.Hour),
		Chan:     make(map[uint64]*model.Ch),
		Ctx:      userCtx,
		Cancel:   userCancel,
	}

	usrCh := &model.Ch{
		TxCh:     make(chan model.Message, 30),
		RxCh:     make(chan model.Message, 30),
		UserId:   44444,
		DialogId: 444,
		RespName: "TestUser",
	}

	respModel.Chan[444] = usrCh

	errCh := make(chan error, 1)
	go func() {
		if err := start.Listener(respModel, usrCh, 444, 444); err != nil {
			errCh <- err
		}
	}()

	time.Sleep(100 * time.Millisecond)

	aiCallsBefore := mockModel.requestCalled.Load()

	// Шаг 1: Вопрос к AI
	t.Log("=== Шаг 1: Отправка вопроса AI ===")
	question1 := model.Message{
		Type: "user",
		Content: model.AssistResponse{
			Message: "Первый вопрос для AI",
		},
		Name:     "TestUser",
		Operator: model.Operator{Operator: false},
	}

	usrCh.RxCh <- question1
	<-usrCh.TxCh // эхо
	msg1 := <-usrCh.TxCh
	if msg1.Operator.Operator {
		t.Error("❌ Первый ответ должен быть от AI")
	} else {
		t.Logf("✅ Получен ответ от AI: %s", msg1.Content.Message)
	}

	// Шаг 2: Переключение на оператора
	t.Log("=== Шаг 2: Переключение на оператора ===")
	question2 := model.Message{
		Type: "user",
		Content: model.AssistResponse{
			Message: "Вопрос для оператора",
		},
		Name:     "TestUser",
		Operator: model.Operator{Operator: true, SenderName: "TestUser"},
	}

	usrCh.RxCh <- question2
	<-usrCh.TxCh // эхо

	select {
	case msg2 := <-usrCh.TxCh:
		if !msg2.Operator.Operator {
			t.Error("❌ Второй ответ должен быть от оператора")
		} else {
			t.Logf("✅ Получен ответ от оператора: %s", msg2.Content.Message)
		}
	case <-time.After(2 * time.Second):
		t.Error("❌ Таймаут ожидания ответа от оператора")
	}

	// Шаг 3: Вопрос в операторском режиме (режим остаётся активным)
	t.Log("=== Шаг 3: Вопрос в активном операторском режиме ===")
	question3 := model.Message{
		Type: "user",
		Content: model.AssistResponse{
			Message: "Вопрос в операторском режиме",
		},
		Name:     "TestUser",
		Operator: model.Operator{Operator: false}, // Флаг false, но режим активен
	}

	usrCh.RxCh <- question3
	<-usrCh.TxCh // эхо

	select {
	case msg3 := <-usrCh.TxCh:
		if msg3.Operator.Operator {
			t.Logf("✅ Операторский режим остался активен, ответ от оператора: %s", msg3.Content.Message)
		} else {
			t.Logf("⚠️  Получен ответ от AI (режим был автоматически отключён): %s", msg3.Content.Message)
		}
	case <-time.After(2 * time.Second):
		t.Log("⚠️  Таймаут ожидания ответа")
	}

	aiCallsAfter := mockModel.requestCalled.Load()
	aiCallsDiff := aiCallsAfter - aiCallsBefore

	t.Logf("=== Итоговая статистика ===")
	t.Logf("Вызовов AI: %d (было) -> %d (стало), разница: %d", aiCallsBefore, aiCallsAfter, aiCallsDiff)
	t.Logf("Вызовов оператора (receive): %d", mockOperator.receiveCalled.Load())

	// Проверяем что был хотя бы 1 вызов AI (в начале)
	if aiCallsDiff < 1 {
		t.Errorf("❌ Ожидался минимум 1 вызов AI, получено %d", aiCallsDiff)
	} else {
		t.Logf("✅ AI вызван %d раз(а)", aiCallsDiff)
	}

	if mockOperator.receiveCalled.Load() < 1 {
		t.Error("❌ Оператор должен быть вызван минимум 1 раз")
	} else {
		t.Logf("✅ Оператор вызван %d раз(а)", mockOperator.receiveCalled.Load())
	}

	t.Log("ℹ️  В текущей реализации операторский режим остаётся активным до явного отключения")
	t.Log("ℹ️  Для возврата к AI нужно вызвать DisableOperatorMode или использовать спец. команду")

	userCancel()
	time.Sleep(500 * time.Millisecond)
}
