package crm

import (
	"fmt"

	"github.com/ikermy/AiR_Common/pkg/logger"
)

type Message struct {
	Files []string // Файлы сообщения
	Type  string   //  Тип сообщения (user / assist)
	Phone string   // Идентификатор контакта
	Name  string   // Имя контакта (в одну строку разберётся в CRM)
	Text  string   // Текст сообщения
	New   bool     // True - если новый диалог (пользователь не известен)
	Voice bool     // True - если голосовое сообщение
	Meta  bool     // True - если нужно отправить сообщение о достижении цели
}

// MSG создает новый Message с обязательными полями type, contact и message
// Используйте методы WithFiles, NewDialog, WithVoice для настройки опциональных полей
func (c *CRM) MSG(tip, contact, name, text string) *Message {
	return &Message{
		Type:  tip,
		Phone: contact,
		Name:  name,
		Text:  text,
	}
}

// SetMeta устанавливает флаг достижения цели (цепочка вызовов)
func (m *Message) SetMeta(meta bool) *Message {
	m.Meta = meta
	return m
}

// WithFiles добавляет файлы к сообщению (цепочка вызовов)
func (m *Message) WithFiles(files ...string) *Message {
	m.Files = files
	return m
}

// NewDialog устанавливает флаг нового пользователя (цепочка вызовов)
func (m *Message) NewDialog(know bool) *Message {
	m.New = know
	return m
}

// WithVoice устанавливает флаг голосового сообщения (цепочка вызовов)
func (m *Message) WithVoice(voice bool) *Message {
	m.Voice = voice
	return m
}

// SendMessage безопасно отправляет сообщение в канал CRM
// Возвращает ошибку, если контекст отменён или канал закрыт
func (c *CRM) SendMessage(msg *Message) error {
	if msg == nil {
		return fmt.Errorf("сообщение не может быть nil")
	}

	if c.conf == nil {
		return fmt.Errorf("CRM не инициализирован, вызовите Init() перед отправкой")
	}

	select {
	case c.msg <- msg:
		logger.Debug("Сообщение отправлено в CRM канал для контакта %s", msg.Phone)
		return nil
	case <-c.ctx.Done():
		return fmt.Errorf("отправка отменена: %w", c.ctx.Err())
	}
}
