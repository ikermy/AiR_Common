package crm

import (
	"fmt"

	"github.com/ikermy/AiR_Common/pkg/logger"
)

type Message struct {
	Files      []string // Файлы сообщения
	Type       string   //  Тип сообщения (user / assist)
	Phone      string   // Идентификатор контакта
	AltContact string   // Альтернативный идентификатор контакта для (Telegram, Instagram и т.д.)
	Name       string   // Имя контакта (в одну строку разберётся в User)
	Text       string   // Текст сообщения
	New        bool     // True - если новый диалог (пользователь не известен)
	Voice      bool     // True - если голосовое сообщение
	Meta       bool     // True - если нужно отправить сообщение о достижении цели
}

// MSG создает новый Message с обязательными полями type, name и text
// Используйте методы WithPhone, WithAltContact, WithFiles, NewDialog, WithVoice для настройки опциональных полей
// Работает даже если User не инициализирован (для удобства цепочки вызовов)
func (u *User) MSG(tip, name, text string) *Message {
	if u == nil {
		return &Message{} // Возвращаем пустое сообщение, которое будет проигнорировано в SendMessage
	}
	return &Message{
		Type: tip,
		Name: name,
		Text: text,
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

// WithPhone устанавливает номер телефона контакта (цепочка вызовов)
func (m *Message) WithPhone(phone string) *Message {
	m.Phone = phone
	return m
}

// WithAltContact устанавливает альтернативный идентификатор контакта (цепочка вызовов)
// Используется для контактов без номера телефона (@telegram, @instagram и т.д.)
func (m *Message) WithAltContact(altContact string) *Message {
	m.AltContact = altContact
	return m
}

// SendMessage безопасно отправляет сообщение в канал User
// Возвращает ошибку, если контекст отменён или канал закрыт
func (u *User) SendMessage(msg *Message) error {
	// User не инициализирован - молча выходим
	if u == nil || u.conf == nil {
		return nil
	}

	if msg == nil {
		return fmt.Errorf("сообщение не может быть nil")
	}

	select {
	case u.msg <- msg:
		logger.Debug("Сообщение отправлено в User канал для контакта %s", msg.Phone, u.conf.UserID)
		return nil
	case <-u.ctx.Done():
		return fmt.Errorf("отправка отменена: %w", u.ctx.Err())
	default:
		// очередь u.msg переполнена например модуль вообще не запущен
		return nil
	}
}
