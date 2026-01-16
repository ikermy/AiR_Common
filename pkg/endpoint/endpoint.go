package endpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/common"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
)

// Inter - интерфейс для работы с диалогами
type Inter interface {
	GetUserAsk(dialogId uint64, respId uint64) []string
	SetUserAsk(dialogId, respId uint64, ask string, askLimit ...uint32) bool
	SaveDialog(creator comdb.CreatorType, treadId uint64, resp *model.AssistResponse)
	GetDialogHistory(dialogId uint64, limit int) ([]Message, error)
	Meta(userId uint32, dialogId uint64, meta string, respName string, assistName string, metaAction string)
	SendEvent(userId uint32, event, userName, assistName, target string)
	SendNotification(msg common.CarpCh) error
}

type DB comdb.Exterior

type Message struct {
	Creator   comdb.CreatorType    `json:"creator"`
	Message   model.AssistResponse `json:"message"`
	Timestamp time.Time            `json:"timestamp"`
}

type Endpoint struct {
	ctx          context.Context
	cancel       context.CancelFunc
	db           DB
	arrMsg       map[uint64]map[uint64][]string
	messageBatch map[uint64][]Message // Буфер сообщений для каждого треда
	batchSize    int                  // Размер батча
	mu           sync.Mutex           // Мьютекс для защиты буфера
	optionalFunc func(any) error      // Дополнительный опциональный метод которого нет в Inter (с типом any для гибкости)
}

func New(parent context.Context, d DB) *Endpoint {
	ctx, cancel := context.WithCancel(parent)
	e := &Endpoint{
		ctx:    ctx,
		cancel: cancel,

		db:           d,
		messageBatch: make(map[uint64][]Message),
		batchSize:    mode.BatchSize, // Размер батча по умолчанию
	}

	// Запускаем горутину для периодической очистки буфера
	go e.periodicFlush()

	// ТОЛЬКО ДЛЯ КАНАЛОВ С ДОПУСКАЮЩИМ ОТСЛЕЖИВАНИЕМ ЗАВЕРШЕНИЯ ДИАЛОГА !!!!!
	// Добавляем обработку событий для немедленного сохранения диалога
	go func() {
		for {
			select {
			case threadId, ok := <-mode.Event:
				if !ok {
					logger.Error("НЕВОЗМОЖНОЕСООБЩЕНИЕ: канал Event был закрыт, сохранение диалогов по событиям остановлено")
					return
				}
				logger.Info("Endpoint: получен сигнал сохранения диалога %d", threadId)
				e.mu.Lock()
				e.flushThreadBatch(threadId)
				e.mu.Unlock()
			case <-e.ctx.Done():
				logger.Info("Endpoint: остановка слушателя событий по контексту")
				return
			}
		}
	}()

	return e
}

// SetOptional устанавливает callback функцию для вызова при достижении meta-цели
func (e *Endpoint) SetOptional(fn func(any) error) {
	e.optionalFunc = fn
}

// CallOptional безопасно вызывает опциональный метод если он установлен (принимает any)
func (e *Endpoint) CallOptional(val any) error {
	if e.optionalFunc != nil {
		return e.optionalFunc(val)
	}
	return nil
}

// CallOptionalTyped[T any] — generic версия CallOptional для типобезопасного вызова
// Пример: e.CallOptionalTyped[int64](int64(respId))
func (e *Endpoint) CallOptionalTyped(val any) error {
	return e.CallOptional(val)
}

// WrapOptional[T any] — helper-функция с дженериком для адаптирования функции типа T
// к типу func(any) error. Используется вместе с SetOptional.
//
// Пример использования:
//
//	e.SetOptional(WrapOptional[int64](telegramProvider.Meta))
func WrapOptional[T any](fn func(T) error) func(any) error {
	if fn == nil {
		return nil
	}
	return func(v any) error {
		t, ok := v.(T)
		if !ok {
			return fmt.Errorf("WrapOptional: unexpected type %T, expected %T", v, *new(T))
		}
		return fn(t)
	}
}

// Shutdown останавливает фоновые задачи и принудительно сохраняет буферы
func (e *Endpoint) Shutdown() {
	// Отменяем контекст, чтобы остановить горутины
	if e.cancel != nil {
		e.cancel()
	}
	// Небольшая пауза для корректной остановки горутин
	time.Sleep(100 * time.Millisecond)
	// Финальный flush
	e.FlushAllBatches()
}

func (e *Endpoint) periodicFlush() {
	ticker := time.NewTicker(mode.TimePeriodicFlush * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.FlushAllBatches()
		case <-e.ctx.Done():
			logger.Info("Endpoint: periodicFlush остановлен по контексту")
			return
		}
	}
}

func (e *Endpoint) flushThreadBatch(threadId uint64) {
	// Должен вызываться с заблокированным e.mu
	batch := e.messageBatch[threadId]
	if len(batch) == 0 {
		return
	}

	// Очищаем буфер
	delete(e.messageBatch, threadId)

	// Разблокируем мьютекс на время операций с БД
	//e.mu.Unlock()
	//defer e.mu.Lock()

	// Сохраняем все сообщения
	for _, msg := range batch {
		jsonData, err := json.Marshal(msg)
		if err != nil {
			logger.Error("Ошибка сериализации: %v", err)
			continue
		}
		if err := e.db.SaveDialog(threadId, jsonData); err != nil {
			logger.Error("Ошибка сохранения диалога: %v", err)
		}
	}
}

// FlushAllBatches принудительно сохраняет все накопленные сообщения
func (e *Endpoint) FlushAllBatches() {
	e.mu.Lock()
	defer e.mu.Unlock()

	for threadId := range e.messageBatch {
		if len(e.messageBatch[threadId]) > 0 {
			e.flushThreadBatch(threadId)
		}
	}
}

func (e *Endpoint) GetUserAsk(dialogId uint64, respId uint64) []string {
	if e.arrMsg == nil {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if userMsgs, ok := e.arrMsg[dialogId]; ok {
		res := userMsgs[respId]
		delete(e.arrMsg[dialogId], respId)
		return res
	}
	return nil
}

func (e *Endpoint) SetUserAsk(dialogId, respId uint64, ask string, askLimit ...uint32) bool {
	// По умолчанию askLimit максимальный для uint32
	var limit uint32 = 4294967295
	if len(askLimit) > 0 {
		limit = askLimit[0]
	}

	ask = strings.TrimSpace(ask)
	if ask == "" || ask == "[]" { // Этого не может быть?! Но на всякий случай
		return true
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.arrMsg == nil {
		e.arrMsg = make(map[uint64]map[uint64][]string)
	}
	if e.arrMsg[dialogId] == nil {
		e.arrMsg[dialogId] = make(map[uint64][]string)
	}

	// Считаю общее количество символов в сообщениях
	totalChars := 0
	for _, msg := range e.arrMsg[dialogId][respId] {
		totalChars += utf8.RuneCountInString(msg)
	}
	askChars := utf8.RuneCountInString(ask)
	if totalChars+askChars > int(limit) {
		logger.Warn("Превышен лимит [%d] символов %d, символов в сообщении %d", limit, askChars, totalChars)
		return false
	}

	e.arrMsg[dialogId][respId] = append(e.arrMsg[dialogId][respId], ask)
	return true
}

// GetDialogHistory получает историю диалога из памяти (messageBatch) или из БД
// Возвращает последние N сообщений (limit) в хронологическом порядке
func (e *Endpoint) GetDialogHistory(dialogId uint64, limit int) ([]Message, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Сначала проверяем буфер в памяти
	batch := e.messageBatch[dialogId]

	if len(batch) > 0 {
		// Есть сообщения в памяти
		logger.Debug("Endpoint: найдено %d сообщений в памяти для диалога %d", len(batch), dialogId)

		// Ограничиваем количество сообщений
		startIdx := 0
		if len(batch) > limit {
			startIdx = len(batch) - limit
		}

		result := make([]Message, len(batch)-startIdx)
		copy(result, batch[startIdx:])
		return result, nil
	}

	// Если в памяти нет, читаем из БД
	logger.Debug("Endpoint: сообщения для диалога %d не найдены в памяти, читаем из БД", dialogId)

	// Разблокируем мьютекс на время операции с БД
	e.mu.Unlock()
	defer e.mu.Lock()

	jsonData, err := e.db.ReadDialog(dialogId, uint8(limit))
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения диалога из БД: %w", err)
	}

	if jsonData == nil || len(jsonData) == 0 {
		logger.Debug("Endpoint: диалог %d не найден в БД", dialogId)
		return nil, nil
	}

	// Парсим JSON данные
	var messages []Message
	if err := json.Unmarshal(jsonData, &messages); err != nil {
		return nil, fmt.Errorf("ошибка парсинга данных диалога из БД: %w", err)
	}

	logger.Debug("Endpoint: прочитано %d сообщений из БД для диалога %d", len(messages), dialogId)
	return messages, nil
}

func (e *Endpoint) SaveDialog(creator comdb.CreatorType, treadId uint64, resp *model.AssistResponse) {
	message := Message{
		Creator:   creator,
		Message:   *resp,
		Timestamp: time.Now(),
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// Добавляем сообщение в буфер
	e.messageBatch[treadId] = append(e.messageBatch[treadId], message)

	// Если размер буфера достиг порога, сохраняем
	if len(e.messageBatch[treadId]) >= e.batchSize {
		e.flushThreadBatch(treadId)
	}
}

// Meta Метод вызывается из common.startpoint
func (e *Endpoint) Meta(userId uint32, dialogId uint64, meta string, respName string, assistName string, metaAction string) {
	err := e.db.UpdateDialogsMeta(dialogId, meta)
	if err != nil {
		logger.Error("ошибка обновления метаданных для диалога %d: %v", dialogId, err, userId)
	}
	e.SendEvent(userId, meta, respName, assistName, metaAction)
}
