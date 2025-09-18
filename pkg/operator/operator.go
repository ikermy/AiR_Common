package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
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

const (
	idleDuration = 30 * time.Minute // длительность простоя для закрытия SSE
	idleOperator = 5 * time.Minute  // длительность простоя для закрытия оператора
)

// session хранит состояние каналов и таймер простоя
type session struct {
	ch         OperatorCh
	ctx        context.Context
	cancel     context.CancelFunc
	idleTimer  *time.Timer
	mu         sync.Mutex
	lastActive time.Time
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
		idleTimer:  time.NewTimer(idleDuration),
		lastActive: time.Now(),
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
	s.idleTimer.Reset(idleDuration)
}

// listenerSession — основной слушатель с учётом простоя
func (o *Operator) listenerSession(key opKey, s *session) {
	// Создаем SSE клиент
	url := fmt.Sprintf("http://%s:8093/op", o.url)
	client := sse.NewClient(url)

	// Подписываемся на события
	events := make(chan *sse.Event)
	err := client.SubscribeChan("messages", events)
	if err != nil {
		logger.Error("Failed to subscribe to SSE: %v", err)
		// Ошибка подписки — очищаем сессию
		o.operatorChMap.Delete(key)
		close(s.ch.RxCh)
		close(s.ch.TxCh)
		s.cancel()
		return
	}
	logger.Info("SSE connection established to %s (user=%d, dialog=%d)", url, s.ch.UserId, s.ch.DialogId)

	// cleanup при выходе
	defer func() {
		client.Unsubscribe(events)
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
			logger.Debug("Sending message via SSE: %+v", msg)
			// Отправляем сообщение два раза с интервалом в 1 секунду
			go func(message model.Message) {
				operMsg := model.Message{
					Operator: true,
					Type:     message.Type,
					Content: model.AssistResponse{
						Message: message.Content.Message + " (отправлено оператором)",
						Action:  message.Content.Action,
						Meta:    message.Content.Meta,
					},
					Name:      message.Name,
					Timestamp: time.Now(),
				}
				for i := 1; i <= 2; i++ {
					if err := o.sendMessage(url, operMsg); err != nil {
						logger.Error("Failed to send message (attempt %d): %v", i, err)
					} else {
						logger.Debug("Message sent successfully (attempt %d)", i)
					}
					if i < 2 {
						time.Sleep(1 * time.Second)
					}
				}
			}(msg)

		case event := <-events:
			if event == nil {
				logger.Warn("SSE connection closed by server (user=%d, dialog=%d)", s.ch.UserId, s.ch.DialogId)
				s.cancel()
				return
			}
			s.touch()
			logger.Debug("Received SSE event: %s", string(event.Data))
			var receivedMsg model.Message
			if err := json.Unmarshal(event.Data, &receivedMsg); err != nil {
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

// sendMessage отправляет сообщение на сервер (эмуляция отправки через SSE)
func (o *Operator) sendMessage(baseURL string, msg model.Message) error {
	// Эмуляция отправки сообщения через HTTP POST
	jsonData, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// В реальной реализации это был бы POST запрос к серверу
	// Для тестирования просто логируем
	logger.Debug("Would send POST to %s/send with data: %s", baseURL, string(jsonData))

	// Эмуляция задержки сети
	time.Sleep(100 * time.Millisecond)

	return nil
}

// StartTestSSEEmulation запускает тестовую эмуляцию SSE сервера
func (o *Operator) StartTestSSEEmulation(ch OperatorCh) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
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
	case <-time.After(10 * idleOperator):
		return model.Message{}, fmt.Errorf("timeout while sending question to operator")
	}

	// Ожидаем ответ от оператора (локальный таймаут ожидания ответа)
	select {
	case response := <-s.ch.RxCh:
		logger.Debug("Received response from operator: %+v", response)
		return response, nil
	case <-s.ctx.Done():
		return model.Message{}, fmt.Errorf("operator session context cancelled while waiting for response")
	case <-time.After(30 * idleOperator):
		return model.Message{}, fmt.Errorf("timeout while waiting for operator response")
	}
}
