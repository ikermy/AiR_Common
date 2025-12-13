package model

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
)

// DialogSaver интерфейс для сохранения диалогов с батчированием
type DialogSaver interface {
	// SaveDialog добавляет сообщение в буфер для сохранения
	SaveDialog(creator CreatorType, dialogId uint64, resp *AssistResponse)
	// FlushDialog принудительно сохраняет все накопленные сообщения для конкретного диалога
	FlushDialog(dialogId uint64)
	// FlushAllDialogs принудительно сохраняет все накопленные сообщения
	FlushAllDialogs()
	// Shutdown корректно завершает работу сервиса сохранения
	Shutdown()
}

// DialogMessageBatch для батчирования
type DialogMessageBatch struct {
	Creator   CreatorType
	Message   AssistResponse
	Timestamp time.Time
}

// DialogSaverImpl реализация DialogSaver с батчированием
type DialogSaverImpl struct {
	ctx          context.Context
	cancel       context.CancelFunc
	db           DialogDB
	messageBatch map[uint64][]DialogMessageBatch
	batchSize    int
	mu           sync.Mutex
}

// NewDialogSaver создает новый сервис сохранения диалогов
func NewDialogSaver(parent context.Context, db DialogDB, batchSize int) *DialogSaverImpl {
	if batchSize <= 0 {
		batchSize = mode.BatchSize
	}

	ctx, cancel := context.WithCancel(parent)
	ds := &DialogSaverImpl{
		ctx:          ctx,
		cancel:       cancel,
		db:           db,
		messageBatch: make(map[uint64][]DialogMessageBatch),
		batchSize:    batchSize,
	}

	// Запускаем периодическую очистку буфера
	go ds.periodicFlush()

	// Слушаем события для немедленного сохранения
	go ds.listenEvents()

	return ds
}

// SaveDialog добавляет сообщение в буфер
func (ds *DialogSaverImpl) SaveDialog(creator CreatorType, dialogId uint64, resp *AssistResponse) {
	message := DialogMessageBatch{
		Creator:   creator,
		Message:   *resp,
		Timestamp: time.Now(),
	}

	ds.mu.Lock()
	defer ds.mu.Unlock()

	ds.messageBatch[dialogId] = append(ds.messageBatch[dialogId], message)

	// Если размер буфера достиг порога, сохраняем
	if len(ds.messageBatch[dialogId]) >= ds.batchSize {
		ds.flushDialogBatchLocked(dialogId)
	}
}

// FlushDialog принудительно сохраняет сообщения для конкретного диалога
func (ds *DialogSaverImpl) FlushDialog(dialogId uint64) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	ds.flushDialogBatchLocked(dialogId)
}

// FlushAllDialogs принудительно сохраняет все накопленные сообщения
func (ds *DialogSaverImpl) FlushAllDialogs() {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	for dialogId := range ds.messageBatch {
		ds.flushDialogBatchLocked(dialogId)
	}
}

// flushDialogBatchLocked сохраняет батч для одного диалога (должен вызываться с залоченным mu)
func (ds *DialogSaverImpl) flushDialogBatchLocked(dialogId uint64) {
	batch := ds.messageBatch[dialogId]
	if len(batch) == 0 {
		return
	}

	// Очищаем буфер
	delete(ds.messageBatch, dialogId)

	// Сохраняем все сообщения
	for _, msg := range batch {
		dialogMsg := DialogMessage{
			Creator:   int(msg.Creator),
			Message:   msg.Message.Message, // Сохраняем только текст сообщения
			Timestamp: msg.Timestamp.Format(time.RFC3339),
		}

		jsonData, err := json.Marshal(dialogMsg)
		if err != nil {
			logger.Error("Ошибка сериализации сообщения: %v", err)
			continue
		}

		if err := ds.db.SaveDialog(dialogId, jsonData); err != nil {
			logger.Error("Ошибка сохранения диалога %d: %v", dialogId, err)
		}
	}
}

// periodicFlush периодически сохраняет накопленные сообщения
func (ds *DialogSaverImpl) periodicFlush() {
	ticker := time.NewTicker(time.Duration(mode.TimePeriodicFlush) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ds.FlushAllDialogs()
		case <-ds.ctx.Done():
			logger.Info("DialogSaver: остановка периодического сохранения")
			return
		}
	}
}

// listenEvents слушает события для немедленного сохранения
func (ds *DialogSaverImpl) listenEvents() {
	for {
		select {
		case dialogId, ok := <-mode.Event:
			if !ok {
				logger.Error("DialogSaver: канал Event был закрыт")
				return
			}
			logger.Info("DialogSaver: получен сигнал сохранения диалога %d", dialogId)
			ds.FlushDialog(dialogId)
		case <-ds.ctx.Done():
			logger.Info("DialogSaver: остановка слушателя событий")
			return
		}
	}
}

// Shutdown корректно завершает работу
func (ds *DialogSaverImpl) Shutdown() {
	logger.Info("DialogSaver: начало завершения работы")

	// Сохраняем все оставшиеся сообщения
	ds.FlushAllDialogs()

	// Отменяем контекст
	if ds.cancel != nil {
		ds.cancel()
	}

	logger.Info("DialogSaver: завершение работы завершено")
}
