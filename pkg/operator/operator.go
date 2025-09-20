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

type OperatorCh struct {
	TxCh     chan model.Message
	RxCh     chan model.Message
	UserId   uint32
	DialogId uint64
}

type Operator struct {
	ctx           context.Context
	cancel        context.CancelFunc
	operatorChMap sync.Map
	url           string
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
}

func New(cfg *conf.Conf) *Operator {
	ctx, cancel := context.WithCancel(context.Background())
	return &Operator{
		ctx:           ctx,
		cancel:        cancel,
		operatorChMap: sync.Map{},
		url:           cfg.WEB.RealUrl,
	}
}

func (o *Operator) Close() {
	o.cancel()
}

// внутренняя функция: получить/создать сессию с таймером простоя
func (o *Operator) getOrCreateSession(userID uint32, dialogID uint64) (*session, opKey, bool, error) {
	key := opKey{userID: userID, dialogID: dialogID}
	if val, ok := o.operatorChMap.Load(key); ok {
		return val.(*session), key, false, nil
	}

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
		// init sid
		sidReady: make(chan struct{}),
	}

	o.operatorChMap.Store(key, s)
	return s, key, true, nil
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

// listenerSession — основной слушатель с учётом простоя
func (o *Operator) listenerSession(key opKey, s *session) {
	logger.Debug("Starting listener session (user=%d, dialog=%d)", s.ch.UserId, s.ch.DialogId)

	// РЕАЛЬНЫЙ SSE клиент
	base := fmt.Sprintf("http://%s:8093/op", o.url)
	logger.Debug(base)
	sseURL := fmt.Sprintf("%s?user_id=%d&dialog_id=%d", base, s.ch.UserId, s.ch.DialogId)
	client := sse.NewClient(sseURL)

	// Подписываемся на события базового потока
	events := make(chan *sse.Event)
	err := client.SubscribeChan("", events)
	if err != nil {
		logger.Error("Failed to subscribe to SSE: %v", err)
		// Ошибка подписки — очищаем сессию
		o.operatorChMap.Delete(key)
		close(s.ch.RxCh)
		close(s.ch.TxCh)
		s.cancel()
		return
	}
	logger.Info("SSE connection established to %s (user=%d, dialog=%d)", sseURL, s.ch.UserId, s.ch.DialogId)

	// cleanup при выходе
	defer func() {
		//client.Unsubscribe(events)
		o.operatorChMap.Delete(key)
		close(s.ch.RxCh)
		close(s.ch.TxCh)
		logger.Info("SSE connection closed (user=%d, dialog=%d)", s.ch.UserId, s.ch.DialogId)
	}()

	for {
		select {
		case msg, ok := <-s.ch.TxCh:
			if !ok {
				logger.Warn("TxCh channel closed (user=%d, dialog=%d)", s.ch.UserId, s.ch.DialogId)
				s.cancel()
				return
			}
			s.touch()
			logger.Debug("Sending message via HTTP POST: %+v", msg)
			// Отправляем сообщение (ждём sid при необходимости)
			go func(message model.Message) {
				if err := o.sendMessage(base, s, message); err != nil {
					logger.Error("Failed to send message: %v", err)
				}
			}(msg)

		case event := <-events:
			if event == nil {
				logger.Warn("SSE connection closed by server (user=%d, dialog=%d)", s.ch.UserId, s.ch.DialogId)
				s.cancel()
				return
			}
			s.touch()
			// Обработка типов событий: init (sid) и сообщения
			etype := string(event.Event)
			edata := event.Data
			logger.Debug("Received SSE event '%s': %s", etype, string(edata))
			if etype == "init" {
				var initPayload struct {
					SID int64 `json:"sid"`
				}
				if err := json.Unmarshal(edata, &initPayload); err != nil {
					logger.Error("Failed to unmarshal init event: %v", err)
					continue
				}
				s.setSID(initPayload.SID)
				logger.Info("sid initialized: %d (user=%d, dialog=%d)", initPayload.SID, s.ch.UserId, s.ch.DialogId)
				continue
			}

			// Остальные события считаем сообщениями
			var receivedMsg model.Message
			if err := json.Unmarshal(edata, &receivedMsg); err != nil {
				logger.Error("Failed to unmarshal received message: %v", err)
				continue
			}
			// Отправляем полученное сообщение в RxCh
			select {
			case s.ch.RxCh <- receivedMsg:
				logger.Debug("Message sent to RxCh: %+v", receivedMsg)
			case <-s.ctx.Done():
				return
			}

		case <-s.idleTimer.C:
			// Таймаут простоя — закрываем сессию
			logger.Info("Operator session idle timeout (30m). Closing (user=%d, dialog=%d)", s.ch.UserId, s.ch.DialogId)
			s.cancel()
			return

		case <-s.ctx.Done():
			logger.Info("Listener session context done (user=%d, dialog=%d)", s.ch.UserId, s.ch.DialogId)
			return
		}
	}
}

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

// StartTestSSEEmulation запускает тестовую эмуляцию SSE сервера
func (o *Operator) StartTestSSEEmulation(ch OperatorCh) {
	logger.Debug("Starting SSE emulation")
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		counter := 1
		for {
			select {
			case <-ticker.C:
				// Эмулируем получение сообщения от сервера
				testMsg := model.Message{
					Operator: false,
					Type:     "test_response",
					Content: model.AssistResponse{
						Message: fmt.Sprintf("Test message #%d from SSE server", counter),
					},
					Name:      "SSE_Server",
					Timestamp: time.Now(),
				}

				// Отправляем тестовое сообщение в RxCh
				select {
				case ch.RxCh <- testMsg:
					logger.Debug("Test SSE message sent: %+v", testMsg)
					counter++
				case <-o.ctx.Done():
					logger.Info("Test SSE emulation stopped")
					return
				}

			case <-o.ctx.Done():
				logger.Info("Test SSE emulation context done")
				return
			}
		}
	}()
}

// AskOperator отправляет вопрос оператору и ожидает ответ, удерживая SSE-сессию активной
func (o *Operator) AskOperator(userID uint32, dialogID uint64, question model.Message) (model.Message, error) {
	// Получаем или создаём долгоживущую сессию
	s, key, created, err := o.getOrCreateSession(userID, dialogID)
	if err != nil {
		return model.Message{}, fmt.Errorf("failed to create/get operator session: %w", err)
	}

	// Если новая — запускаем listenerSession
	if created {
		go o.listenerSession(key, s)
	}

	// Отправляем вопрос оператору
	select {
	case s.ch.TxCh <- question:
		logger.Debug("Question sent to operator: %+v", question)
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
	case <-s.ctx.Done():
		return model.Message{}, fmt.Errorf("operator session context cancelled while waiting for response")
	case <-time.After(mode.IdleDuration * time.Minute):
		return model.Message{}, fmt.Errorf("timeout while waiting for operator response")
	}
}
