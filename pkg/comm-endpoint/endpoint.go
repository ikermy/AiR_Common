package comm_endpoint

//import (
//	"encoding/json"
//	"fmt"
//	"github.com/ikermy/AiR_Common/pkg/comm-db"
//	"github.com/ikermy/AiR_Common/pkg/common"
//	"github.com/ikermy/AiR_Common/pkg/mode"
//	"log"
//	"strings"
//	"sync"
//	"time"
//	"unicode/utf8"
//)
//
//type DB interface {
//	SaveDialog(treadId uint64, message json.RawMessage) error
//	//UpdateDialogsMeta(dialogId uint64, meta string) error
//}
//
//type Endpoint struct {
//	Db           DB
//	answers      []string
//	arrMsg       map[uint64]map[uint64][]string
//	MessageBatch map[uint64][]comm_db.Message // Буфер сообщений для каждого треда
//	BatchSize    int                          // Размер батча
//	batchMutex   sync.Mutex                   // Мьютекс для защиты буфера
//}
//
//func New(d DB) *Endpoint {
//	e := &Endpoint{
//		Db:           d,
//		MessageBatch: make(map[uint64][]comm_db.Message),
//		BatchSize:    mode.BatchSize, // Размер батча по умолчанию
//	}
//
//	// Запускаем горутину для периодической очистки буфера
//	go e.PeriodicFlush()
//
//	////////// ИСПОЛЬЗУЕТСЯ В КАНАЛАХ ДОПУСКАЮЩИЙ ОТСЛЕЖИВАНИЕ ЗАВЕРШЕНИЯ ДИАЛОГА !!!!!!!!!
//	// Добавляем обработку событий для немедленного сохранения диалога
//	go func() {
//		for threadId := range common.Event {
//			log.Printf("Endpoint: получен сигнал сохранения диалога %d", threadId)
//			e.FlushThreadBatch(threadId)
//		}
//		log.Println("НЕВОЗМОЖНОЕСООБЩЕНИЕ: канал global.Event был закрыт, сохранение диалогов по событиям остановлено")
//	}()
//
//	return e
//}
//
//// FlushThreadBatch принудительно сохраняет все накопленные сообщения для указанного треда
//func (e *Endpoint) FlushThreadBatch(threadId uint64) {
//	e.batchMutex.Lock()
//	defer e.batchMutex.Unlock()
//
//	if len(e.MessageBatch[threadId]) > 0 {
//		e.flushThreadBatch(threadId)
//	}
//}
//
//func (e *Endpoint) PeriodicFlush() {
//	ticker := time.NewTicker(mode.TimePeriodicFlush * time.Second)
//	defer ticker.Stop()
//
//	for range ticker.C {
//		e.FlushAllBatches()
//	}
//}
//
//func (e *Endpoint) flushThreadBatch(threadId uint64) {
//	// Должен вызываться с заблокированным e.batchMutex
//	batch := e.MessageBatch[threadId]
//	if len(batch) == 0 {
//		return
//	}
//
//	// Очищаем буфер
//	delete(e.MessageBatch, threadId)
//
//	// Разблокируем мьютекс на время операций с БД
//	e.batchMutex.Unlock()
//	defer e.batchMutex.Lock()
//
//	// Сохраняем все сообщения
//	for _, msg := range batch {
//		jsonData, err := json.Marshal(msg)
//		if err != nil {
//			log.Printf("Ошибка сериализации: %v", err)
//			continue
//		}
//
//		if err := e.Db.SaveDialog(threadId, jsonData); err != nil {
//			log.Printf("Ошибка сохранения диалога: %v", err)
//		}
//	}
//}
//
//// FlushAllBatches принудительно сохраняет все накопленные сообщения
//func (e *Endpoint) FlushAllBatches() {
//	e.batchMutex.Lock()
//	defer e.batchMutex.Unlock()
//
//	for threadId := range e.MessageBatch {
//		if len(e.MessageBatch[threadId]) > 0 {
//			e.flushThreadBatch(threadId)
//		}
//	}
//}
//
//func (e *Endpoint) GetUserAsk(dialogId uint64, respId uint64) []string {
//	if e.arrMsg == nil {
//		return nil
//	}
//	if userMsgs, ok := e.arrMsg[dialogId]; ok {
//		res := userMsgs[respId]
//		delete(e.arrMsg[dialogId], respId)
//		return res
//	}
//	return nil
//}
//
//func (e *Endpoint) SetUserAsk(dialogId uint64, respId uint64, ask string, askLimit uint32) bool {
//	ask = strings.TrimSpace(ask)
//	if ask == "" || ask == "[]" { // Этого не может быть?! Но на всякий случай
//		return true
//	}
//
//	if e.arrMsg == nil {
//		e.arrMsg = make(map[uint64]map[uint64][]string)
//	}
//	if e.arrMsg[dialogId] == nil {
//		e.arrMsg[dialogId] = make(map[uint64][]string)
//	}
//
//	// Считаю общее количество символов в сообщениях
//	totalChars := 0
//	for _, msg := range e.arrMsg[dialogId][respId] {
//		totalChars += utf8.RuneCountInString(msg)
//	}
//
//	askChars := utf8.RuneCountInString(ask)
//	if totalChars+askChars > int(askLimit) {
//		fmt.Println("Превышен лимит символов", totalChars, askChars, askLimit)
//		return false
//	}
//
//	e.arrMsg[dialogId][respId] = append(e.arrMsg[dialogId][respId], ask)
//	return true
//}
//
//func (e *Endpoint) SaveDialog(creator comm_db.CreatorType, treadId uint64, resp *string) {
//	ask := strings.TrimSpace(*resp)
//	if ask == "" || ask == "[]" { // Этого не может быть?! Но на всякий случай
//		return
//	}
//
//	message := comm_db.Message{
//		Creator:   creator,
//		Message:   *resp,
//		Timestamp: time.Now(),
//	}
//
//	e.batchMutex.Lock()
//	defer e.batchMutex.Unlock()
//
//	// Добавляем сообщение в буфер
//	e.MessageBatch[treadId] = append(e.MessageBatch[treadId], message)
//
//	// Если размер буфера достиг порога, сохраняем
//	if len(e.MessageBatch[treadId]) >= e.BatchSize {
//		e.flushThreadBatch(treadId)
//	}
//}
