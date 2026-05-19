package model

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// ============================================================================
// СТРУКТУРЫ ДАННЫХ
// ============================================================================

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

// Assistant информация об ассистенте
type Assistant struct {
	AssistId   string
	AssistName string
	Metas      Target
	Events     Notifications
	UserID     uint32
	Limit      uint32
	Provider   create.ProviderType
	Espero     uint8
	Ignore     bool
}

// RespModel универсальная структура респондента для всех провайдеров
type RespModel struct {
	Ctx      context.Context
	Cancel   context.CancelFunc
	Chan     map[uint64]*Ch
	TTL      time.Time
	Assist   Assistant
	RespName string
	Services Services
}

// Services структура для отслеживания активных сервисов
type Services struct {
	Listener   *atomic.Bool
	Respondent *atomic.Bool
}

// Action действия для выполнения
type Action struct {
	SendFiles []File `json:"send_files,omitempty"`
}

// FileType тип файла
type FileType string

const (
	Photo FileType = "photo"
	Video FileType = "video"
	Audio FileType = "audio"
	Doc   FileType = "doc"
)

// File информация о файле
type File struct {
	Type     FileType `json:"type,omitempty"`
	URL      string   `json:"url,omitempty"`
	FileName string   `json:"file_name,omitempty"`
	Caption  string   `json:"caption,omitempty"`
}

// AssistResponse представляет ответ от AI-ассистента
type AssistResponse struct {
	Message  string `json:"message,omitempty"`
	Action   Action `json:"action,omitempty"`
	Meta     bool   `json:"target,omitempty"`
	Operator bool   `json:"operator,omitempty"`
}

// Ch канал для обмена сообщениями
type Ch struct {
	TxCh     chan Message
	RxCh     chan Message
	UserID   uint32
	DialogID uint64
	RespName string
	txClosed atomic.Bool
	rxClosed atomic.Bool
}

// IsTxOpen проверяет, открыт ли канал TxCh для записи
func (ch *Ch) IsTxOpen() bool {
	return !ch.txClosed.Load()
}

// IsRxOpen проверяет, открыт ли канал RxCh для записи
func (ch *Ch) IsRxOpen() bool {
	return !ch.rxClosed.Load()
}

// SendToTx безопасно отправляет сообщение в TxCh
func (ch *Ch) SendToTx(msg Message) error {
	if !ch.IsTxOpen() {
		return fmt.Errorf("канал TxCh закрыт для DialogID %d", ch.DialogID)
	}
	defer func() {
		if r := recover(); r != nil {
			// канал закрыт в race-condition — паника проигнорирована
		}
	}()
	select {
	case ch.TxCh <- msg:
		return nil
	case <-time.After(1 * time.Second):
		return fmt.Errorf("таймаут отправки в TxCh для DialogID %d", ch.DialogID)
	}
}

// SendToRx безопасно отправляет сообщение в RxCh
func (ch *Ch) SendToRx(msg Message) error {
	if !ch.IsRxOpen() {
		return fmt.Errorf("канал RxCh закрыт для DialogID %d", ch.DialogID)
	}
	defer func() {
		if r := recover(); r != nil {
			// канал закрыт в race-condition — паника проигнорирована
		}
	}()
	select {
	case ch.RxCh <- msg:
		return nil
	default:
		return fmt.Errorf("канал RxCh переполнен для DialogID %d", ch.DialogID)
	}
}

// Close безопасно закрывает оба канала Ch
func (ch *Ch) Close() error {
	ch.CloseTx()
	ch.CloseRx()
	return nil
}

// CloseTx безопасно закрывает TxCh
func (ch *Ch) CloseTx() {
	if !ch.IsTxOpen() {
		return
	}
	ch.txClosed.Store(true)
	time.Sleep(10 * time.Millisecond)
	safeCloseMessage(ch.TxCh)
}

// CloseRx безопасно закрывает RxCh
func (ch *Ch) CloseRx() {
	if !ch.IsRxOpen() {
		return
	}
	ch.rxClosed.Store(true)
	time.Sleep(10 * time.Millisecond)
	safeCloseMessage(ch.RxCh)
}

// safeCloseMessage закрывает канал, перехватывая панику при повторном закрытии
func safeCloseMessage(ch chan Message) {
	defer func() {
		if r := recover(); r != nil {
			// канал уже закрыт — паника проигнорирована
		}
	}()
	close(ch)
}

// StartCh структура для передачи данных для запуска слушателя
type StartCh struct {
	Ctx      context.Context
	Provider string // "telegram", "whatsapp", "instagram" — для логирования
	Model    *RespModel
	Chanel   *Ch
	TreadId  uint64
	RespId   uint64
}

// Operator информация об операторе
type Operator struct {
	SenderName  string
	SetOperator bool
	Operator    bool
}

// Message представляет сообщение в системе
type Message struct {
	Operator  Operator
	Type      string
	Content   AssistResponse
	Name      string
	Timestamp time.Time
	Files     []FileUpload `json:"files,omitempty"`
}

// FileUpload представляет файл для отправки (code interpreter, изображения и т.д.)
type FileUpload struct {
	Name     string    `json:"name"`
	Content  io.Reader `json:"-"`
	MimeType string    `json:"mime_type"`
	URL      string    `json:"url,omitempty"`
}

// IsImageMimeType проверяет, является ли MIME-тип изображением
func (f *FileUpload) IsImageMimeType() bool {
	switch f.MimeType {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/jpg":
		return true
	default:
		return false
	}
}

// HasURL проверяет, содержит ли FileUpload валидный HTTP(S) URL
func (f *FileUpload) HasURL() bool {
	return f.URL != "" && (strings.HasPrefix(f.URL, "http://") || strings.HasPrefix(f.URL, "https://"))
}

// ============================================================================
// HELPER ФУНКЦИИ ДЛЯ СОЗДАНИЯ РЕСПОНДЕНТОВ
// ============================================================================

// CreateBaseResponder создаёт базовые компоненты для респондента.
// Используется всеми провайдерами для устранения дублирования кода.
func CreateBaseResponder(parentCtx context.Context, ttl time.Duration,
	assist Assistant, dialogID uint64, respName string) (context.Context, context.CancelFunc, *Ch, time.Time) {

	userCtx, cancel := context.WithCancel(parentCtx)

	ch := &Ch{
		TxCh:     make(chan Message, create.TxChanBuffer),
		RxCh:     make(chan Message, create.RxChanBuffer),
		UserID:   assist.UserID,
		DialogID: dialogID,
		RespName: respName,
	}

	return userCtx, cancel, ch, time.Now().Add(ttl)
}

// NotifyWaitChannels уведомляет ожидающие горутины о создании респондента
func NotifyWaitChannels(waitChannels *sync.Map, respId uint64) {
	if waitChIface, exists := waitChannels.Load(respId); exists {
		waitCh := waitChIface.(chan struct{})
		close(waitCh)
		waitChannels.Delete(respId)
	}
}
