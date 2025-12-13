package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/r3labs/sse/v2"
)

//type CallBack interface {
//	DisableOperatorMode(userId uint32, dialogId uint64) error
//}

type OperatorCh struct {
	Respondent any
	TxCh       chan model.Message
	RxCh       chan model.Message
	DialogId   uint64
	UserId     uint32
}

type Operator struct {
	port          string
	ctx           context.Context
	cancel        context.CancelFunc
	operatorChMap sync.Map
	//cb            CallBack
}

// ключ для сессии оператора
type opKey struct {
	userID   uint32
	dialogID uint64
}

// session хранит состояние каналов и таймер простоя
type session struct {
	ch         OperatorCh
	ctx        context.Context
	cancel     context.CancelFunc
	idleTimer  *time.Timer
	mu         sync.Mutex
	lastActive time.Time
	// session id, получаем из init-события SSE
	sid      int64
	sidReady chan struct{}
	sidOnce  sync.Once
	// канал для сигнализации об ошибках HTTP POST
	httpErrorCh chan error
	// Гарантирует однократное выполнение очистки
	cleanupOnce sync.Once
	// Канал для ошибок от операторского бэкенда
	connectionErrorCh chan string
}

func New(parent context.Context, cfg *conf.Conf) *Operator {
	ctx, cancel := context.WithCancel(parent)

	o := &Operator{
		ctx:           ctx,
		cancel:        cancel,
		operatorChMap: sync.Map{},
		port:          cfg.WEB.Oper,
	}

	return o
}

func (o *Operator) Close() {
	o.cancel()
}

// DeleteSession удаляет сессию оператора для заданного пользователя и диалога
func (o *Operator) DeleteSession(userID uint32, dialogID uint64) error {
	key := opKey{userID: userID, dialogID: dialogID}
	val, ok := o.operatorChMap.Load(key)
	if !ok {
		return fmt.Errorf("session not found for user=%d dialog=%d", userID, dialogID)
	}

	s := val.(*session)
	o.cleanup(key, s)
	logger.Debug("Session deleted for user=%d dialog=%d", userID, dialogID)

	return nil
}

// внутренняя функция: получить/создать сессию с таймером простоя
func (o *Operator) getOrCreateSession(userID uint32, dialogID uint64) (*session, error) {
	key := opKey{userID: userID, dialogID: dialogID}
	if val, ok := o.operatorChMap.Load(key); ok {
		logger.Debug("Найдена существующая сессия (user=%d, dialog=%d)", userID, dialogID)
		return val.(*session), nil
	}

	logger.Debug("Создаётся новая сессия (user=%d, dialog=%d)", userID, dialogID)
	// Создаём каналы напрямую (OperatorChannels удалён)
	ch := OperatorCh{
		TxCh:     make(chan model.Message, 1),
		RxCh:     make(chan model.Message, 1),
		UserId:   userID,
		DialogId: dialogID,
	}

	ctx, cancel := context.WithCancel(o.ctx)
	s := &session{
		ch:         ch,
		ctx:        ctx,
		cancel:     cancel,
		idleTimer:  time.NewTimer(mode.IdleDuration * time.Minute),
		lastActive: time.Now(),
		// Канал дла ожидания получения sid
		sidReady: make(chan struct{}),
		// канал для сигнализации об ошибках HTTP POST
		httpErrorCh:       make(chan error, 1),
		connectionErrorCh: make(chan string, 1),
	}

	o.operatorChMap.Store(key, s)

	// Запускаю слущателя
	go o.listenerSession(key, s)

	return s, nil
}

// GetConnectionErrors возвращает канал для получения ошибок подключения
// Если сессия ещё не создана, возвращает пустой канал (не создаёт сессию)
func (o *Operator) GetConnectionErrors(ctx context.Context, userID uint32, dialogID uint64) <-chan string {
	key := opKey{userID: userID, dialogID: dialogID}

	// Проверяем, существует ли уже сессия (БЕЗ создания новой)
	if val, ok := o.operatorChMap.Load(key); ok {
		s := val.(*session)
		return s.connectionErrorCh
	}

	// Если сессии нет, возвращаем пустой канал, который никогда не отправит данные
	// Это предотвращает автоматическое создание операторской сессии для всех пользователей
	ch := make(chan string)
	close(ch) // Закрываем сразу, чтобы select не блокировался
	return ch
}

// touch продлевает TTL сессии и сбрасывает таймер простоя
func (s *session) touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActive = time.Now()
	if !s.idleTimer.Stop() {
		select {
		case <-s.idleTimer.C:
		default:
		}
	}
	s.idleTimer.Reset(mode.IdleDuration * time.Minute)
}

// setSID сохраняет sid и сигнализирует готовность
func (s *session) setSID(id int64) {
	s.mu.Lock()
	s.sid = id
	s.mu.Unlock()
	s.sidOnce.Do(func() { close(s.sidReady) })
}

// waitSID блокируется до получения sid или таймаута
func (s *session) waitSID(timeout time.Duration) (int64, error) {
	select {
	case <-s.sidReady:
		s.mu.Lock()
		id := s.sid
		s.mu.Unlock()
		return id, nil
	case <-s.ctx.Done():
		return 0, fmt.Errorf("session cancelled while waiting for sid")
	case <-time.After(timeout):
		return 0, fmt.Errorf("timeout while waiting for sid")
	}
}

// cleanup выполняет очистку сессии ровно один раз.
func (o *Operator) cleanup(key opKey, s *session) {
	s.cleanupOnce.Do(func() {
		// Уведомляем сервер о закрытии сессии
		//if s.sid > 0 {
		//	if err := o.notifyServerSessionClose(key.userID, key.dialogID, s.sid); err != nil {
		//		logger.Error("Failed to notify server about session close: %v", err)
		//	}
		//}

		// Отменяем контекст сессии, чтобы остановить все связанные горутины
		s.cancel()

		// Удаляем сессию из карты
		o.operatorChMap.Delete(key)

		logger.Debug("Session cleanup complete (user=%d, dialog=%d)", key.userID, key.dialogID)
	})
}

// listenerSession — основной слушатель с учётом простоя
func (o *Operator) listenerSession(key opKey, s *session) {
	logger.Debug("Starting listener session (user=%d, dialog=%d)", s.ch.UserId, s.ch.DialogId)

	base := fmt.Sprintf("http://localhost:%s/op", o.port)
	sseURL := fmt.Sprintf("%s?user_id=%d&dialog_id=%d", base, s.ch.UserId, s.ch.DialogId)
	client := sse.NewClient(sseURL)

	client.Connection.Transport = &http.Transport{
		IdleConnTimeout: 30 * time.Second,
	}

	events := make(chan *sse.Event)
	err := client.SubscribeChan("", events)
	if err != nil {
		logger.Error("Failed to subscribe to SSE: %v", err)
		// Не закрываем каналы здесь — просто отменяем сессию и удаляем её
		o.operatorChMap.Delete(key)
		s.cancel()
		return
	}

	//defer func() {
	//	// И в любом случае выключаю операторский режим
	//	if err := o.cb.DisableOperatorMode(s.ch.UserId, s.ch.DialogId); err != nil {
	//		logger.Error("Failed to disable operator mode: %v", err)
	//	}
	//}()

	// Ожидаем отмены контекста сессии и выполняем единоразовую очистку
	go func() {
		for {
			<-s.ctx.Done()
			logger.Debug("Closing operator session context cancelled")
			o.cleanup(key, s)
			return
		}
	}()

	for {
		select {
		case msg, ok := <-s.ch.TxCh:
			if !ok {
				logger.Info("TxCh channel closed (user=%d, dialog=%d)", s.ch.UserId, s.ch.DialogId)
				o.cleanup(key, s)
				return
			}
			s.touch()
			logger.Debug("Sending message via HTTP POST: %+v", msg)

			go func(message model.Message) {
				if err := o.sendMessage(base, s, message); err != nil {
					logger.Error("Failed to send message: %v", err)
					// Отправляем ошибку в канал для обработки
					select {
					case s.httpErrorCh <- err:
					case <-s.ctx.Done():
					}
				}
			}(msg)

		case httpErr := <-s.httpErrorCh:
			// Обработка ошибок HTTP POST - если сервер недоступен, закрываем сессию
			logger.Warn("HTTP POST error detected, closing session (user=%d, dialog=%d): %v", s.ch.UserId, s.ch.DialogId, httpErr)
			o.cleanup(key, s)
			return

		case event, ok := <-events:
			// Корректно завершаем при закрытии канала событий SSE
			if !ok {
				logger.Warn("SSE events channel closed by server (user=%d, dialog=%d)", s.ch.UserId, s.ch.DialogId)
				o.cleanup(key, s)
				return
			}

			s.touch()
			etype := string(event.Event)
			edata := event.Data
			logger.Debug("Received SSE event '%s': %s", etype, string(edata))

			// Обработка ошибок
			if etype == "error" {
				logger.Error("Received error event: %s", string(edata))

				// Пока всего один тип обрабатываемой ошибки, поэтому сразу шлю её
				select {
				case s.connectionErrorCh <- "no_tg_id":
				case <-s.ctx.Done():
					return
				default:
				}
				o.cleanup(key, s)
				return
			}

			if etype == "init" {
				var initPayload struct {
					SID int64 `json:"sid"`
				}
				if err := json.Unmarshal(edata, &initPayload); err != nil {
					logger.Error("Failed to unmarshal init event: %v", err)
					continue
				}
				s.setSID(initPayload.SID)
				continue
			}

			var receivedMsg model.Message
			if len(edata) == 0 {
				// пустой пакет от сервера — игнорируем
				continue
			}
			if err := json.Unmarshal(edata, &receivedMsg); err != nil {
				logger.Error("Failed to unmarshal received message: %v", err)
				continue
			}

			select {
			case s.ch.RxCh <- receivedMsg:
				logger.Debug("Message sent to RxCh: %+v", receivedMsg)
			case <-s.ctx.Done():
				return
			}

		case <-s.idleTimer.C:
			logger.Info("Operator session idle timeout (30m). Closing (user=%d, dialog=%d)", s.ch.UserId, s.ch.DialogId)
			//s.cancel()
			return

		case <-s.ctx.Done():
			logger.Debug("Listener session context done (user=%d, dialog=%d)", s.ch.UserId, s.ch.DialogId)
			return
		}
	}
}

// notifyServerSessionClose уведомляет сервер о закрытии сессии оператора
//func (o *Operator) notifyServerSessionClose(userID uint32, dialogID uint64, sid int64) error {
//	closeURL := fmt.Sprintf("http://localhost:%s/close?user_id=%d&dialog_id=%d&sid=%d",
//		o.port, userID, dialogID, sid)
//
//	req, err := http.NewRequest("POST", closeURL, nil)
//	if err != nil {
//		return err
//	}
//
//	resp, err := http.DefaultClient.Do(req)
//	if err != nil {
//		return err
//	}
//	defer resp.Body.Close()
//
//	if resp.StatusCode != http.StatusOK {
//		return fmt.Errorf("server returned status %d", resp.StatusCode)
//	}
//
//	return nil
//}

// sendMessage отправляет сообщение на сервер через HTTP POST с sid
func (o *Operator) sendMessage(baseURL string, s *session, msg model.Message) error {
	logger.Debug("Preparing to send message: %+v", msg)
	// Ждём sid из init события
	sid, err := s.waitSID(mode.IdleOperator * time.Minute)
	if err != nil {
		return err
	}

	// Формируем конверт
	type envelope struct {
		UserID   uint32         `json:"user_id"`
		DialogID uint64         `json:"dialog_id"`
		SID      int64          `json:"sid"`
		Msg      *model.Message `json:"msg,omitempty"`
	}
	env := envelope{
		UserID:   s.ch.UserId,
		DialogID: s.ch.DialogId,
		SID:      sid,
		Msg:      &msg,
	}
	jsonData, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("failed to marshal envelope: %w", err)
	}

	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, baseURL, bytes.NewReader(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to POST message: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status from POST /op: %s", resp.Status)
	}

	return nil
}

// AskOperator отправляет вопрос оператору и ожидает ответ, удерживая SSE-сессию активной
func (o *Operator) AskOperator(ctx context.Context, userID uint32, dialogID uint64, question model.Message) (model.Message, error) {
	// Получаем или создаём долгоживущую сессию
	s, err := o.getOrCreateSession(userID, dialogID)
	if err != nil {
		return model.Message{}, fmt.Errorf("failed to create/get operator session: %w", err)
	}

	// Отправляем вопрос оператору
	select {
	case s.ch.TxCh <- question:
		logger.Debug("Question sent to operator: %+v", question)
	case <-ctx.Done():
		return model.Message{}, ctx.Err()
	case <-s.ctx.Done():
		return model.Message{}, fmt.Errorf("operator session context cancelled while sending question")
	case <-time.After(mode.IdleOperator * time.Minute):
		return model.Message{}, fmt.Errorf("timeout while sending question to operator")
	}

	// Ожидаем ответ от оператора (локальный таймаут ожидания ответа)
	select {
	case response := <-s.ch.RxCh:
		logger.Debug("Received response from operator: %+v", response)
		return response, nil
	case <-ctx.Done():
		return model.Message{}, ctx.Err()
	case <-s.ctx.Done():
		return model.Message{}, fmt.Errorf("operator session context cancelled while waiting for response")
	case <-time.After(mode.IdleDuration * time.Minute):
		return model.Message{}, fmt.Errorf("timeout while waiting for operator response")
	}
}

// SendToOperator отправляет сообщение оператору без ожидания ответа
func (o *Operator) SendToOperator(ctx context.Context, userID uint32, dialogID uint64, question model.Message) error {
	s, err := o.getOrCreateSession(userID, dialogID)
	if err != nil {
		return fmt.Errorf("failed to create/get operator session: %w", err)
	}

	select {
	case s.ch.TxCh <- question:
		logger.Debug("Question sent to operator: %+v", question)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return fmt.Errorf("operator session context cancelled")
	case <-time.After(5 * time.Second): // Короткий таймаут
		return fmt.Errorf("timeout while sending question to operator")
	}
}

// ReceiveFromOperator возвращает канал для получения ответов от оператора
func (o *Operator) ReceiveFromOperator(ctx context.Context, userID uint32, dialogID uint64) <-chan model.Message {
	s, err := o.getOrCreateSession(userID, dialogID)
	if err != nil {
		// Возвращаем закрытый канал в случае ошибки
		ch := make(chan model.Message)
		close(ch)
		return ch
	}

	// Если внешний контекст завершится раньше, можно запустить горутину для закрытия сессии (опционально)
	go func() {
		select {
		case <-ctx.Done():
		case <-s.ctx.Done():
		}
	}()
	return s.ch.RxCh
}

// CloseOperatorSSE закрывает SSE-сессию оператора после получения сообщения что оператор отключился
func (o *Operator) CloseOperatorSSE(ctx context.Context, userID uint32, dialogID uint64) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	key := opKey{userID: userID, dialogID: dialogID}
	val, ok := o.operatorChMap.Load(key)
	if !ok {
		return fmt.Errorf("session not found for user=%d dialog=%d", userID, dialogID)
	}
	s := val.(*session)
	o.cleanup(key, s) // Централизованная функция очистки

	return nil
}
