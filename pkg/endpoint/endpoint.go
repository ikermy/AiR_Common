package endpoint

import (
	"encoding/json"
	"fmt"
	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/common"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type DB interface {
	SaveDialog(treadId uint64, message json.RawMessage) error
	UpdateDialogsMeta(dialogId uint64, meta string) error
}

type Endpoint struct {
	Db DB
	//answers      []string
	arrMsg       map[uint64]map[uint64][]string
	messageBatch map[uint64][]comdb.Message // Буфер сообщений для каждого треда
	batchSize    int                        // Размер батча
	mu           sync.Mutex                 // Мьютекс для защиты буфера
}

func New(d DB) *Endpoint {
	e := &Endpoint{
		Db:           d,
		messageBatch: make(map[uint64][]comdb.Message),
		batchSize:    mode.BatchSize, // Размер батча по умолчанию
	}

	// Запускаем горутину для периодической очистки буфера
	go e.periodicFlush()

	// ТОЛЬКО ДЛЯ КАНАЛОВ С ДОПУСКАЮЩИМ ОТСЛЕЖИВАНИЕМ ЗАВЕРШЕНИЯ ДИАЛОГА !!!!!
	// Добавляем обработку событий для немедленного сохранения диалога
	go func() {
		for threadId := range common.Event {
			log.Printf("Endpoint: получен сигнал сохранения диалога %d", threadId)
			e.mu.Lock()
			e.flushThreadBatch(threadId)
			e.mu.Unlock()
		}
		log.Println("НЕВОЗМОЖНОЕСООБЩЕНИЕ: канал Event был закрыт, сохранение диалогов по событиям остановлено")
	}()

	return e
}

func (e *Endpoint) periodicFlush() {
	ticker := time.NewTicker(mode.TimePeriodicFlush * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		e.FlushAllBatches()
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
			log.Printf("Ошибка сериализации: %v", err)
			continue
		}
		if err := e.Db.SaveDialog(threadId, jsonData); err != nil {
			log.Printf("Ошибка сохранения диалога: %v", err)
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

func (e *Endpoint) SetUserAsk(dialogId uint64, respId uint64, ask string, askLimit uint32) bool {
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
	if totalChars+askChars > int(askLimit) {
		fmt.Println("Превышен лимит символов", totalChars, askChars, askLimit)
		return false
	}

	e.arrMsg[dialogId][respId] = append(e.arrMsg[dialogId][respId], ask)
	return true
}

func (e *Endpoint) SaveDialog(creator comdb.CreatorType, treadId uint64, resp *string) {
	ask := strings.TrimSpace(*resp)
	if ask == "" || ask == "[]" { // Этого не может быть?! Но на всякий случай
		return
	}

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
		log.Printf("ошибка обновления метаданных для диалога %d: %v", dialogId, err)
	}
	common.SendEvent(userId, meta, respName, assistName, metaAction)
}
