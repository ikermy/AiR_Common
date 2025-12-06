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
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
)

type DB interface {
	SaveDialog(treadId uint64, message json.RawMessage) error
	UpdateDialogsMeta(dialogId uint64, meta string) error
	GetNotificationChannel(userId uint32) (json.RawMessage, error)
}

type Endpoint struct {
	ctx    context.Context
	cancel context.CancelFunc

	Db           DB
	arrMsg       map[uint64]map[uint64][]string
	messageBatch map[uint64][]comdb.Message // Буфер сообщений для каждого треда
	batchSize    int                        // Размер батча
	mu           sync.Mutex                 // Мьютекс для защиты буфера
}

func New(parent context.Context, d DB) *Endpoint {
	ctx, cancel := context.WithCancel(parent)
	e := &Endpoint{
		ctx:    ctx,
		cancel: cancel,

		Db:           d,
		messageBatch: make(map[uint64][]comdb.Message),
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
		if err := e.Db.SaveDialog(threadId, jsonData); err != nil {
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
		fmt.Println("Превышен лимит символов", totalChars, askChars, askLimit)
		return false
	}

	e.arrMsg[dialogId][respId] = append(e.arrMsg[dialogId][respId], ask)
	return true
}

func (e *Endpoint) SaveDialog(creator comdb.CreatorType, treadId uint64, resp *model.AssistResponse) {
	//ask := strings.TrimSpace(*resp)
	//if ask == "" || ask == "[]" { // Этого не может быть?! Но на всякий случай
	//	return
	//}

	message := comdb.Message{
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
	err := e.Db.UpdateDialogsMeta(dialogId, meta)
	if err != nil {
		logger.Error("ошибка обновления метаданных для диалога %d: %v", dialogId, err, userId)
	}
	e.SendEvent(userId, meta, respName, assistName, metaAction)
}
