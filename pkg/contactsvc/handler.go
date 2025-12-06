package contactsvc

import (
	"encoding/json"
	"sync"
)

// ContactsServiceHandler - обобщённый обработчик для любых контактных данных
// Не зависит от конкретного proto-пакета
type ContactsServiceHandler struct {
	mu   sync.Mutex
	db   DB
	data map[string]json.RawMessage // Буфер для хранения полученных контактов (JSON)
}

// NewContactsServiceHandler создаёт новый обработчик
func NewContactsServiceHandler(db DB) *ContactsServiceHandler {
	return &ContactsServiceHandler{
		db:   db,
		data: make(map[string]json.RawMessage),
	}
}

// HandleContactsData получает данные контактов и сохраняет их
// Работает с любыми структурами контактов благодаря JSON
func (h *ContactsServiceHandler) HandleContactsData(contactsData json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Сохраняем в буфер
	h.data["contacts"] = contactsData

	return nil
}

// GetData возвращает полученные контакты в виде JSON
func (h *ContactsServiceHandler) GetData() map[string]json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()

	result := make(map[string]json.RawMessage)
	for k, v := range h.data {
		result[k] = v
	}
	return result
}

// ClearData очищает буфер полученных контактов
func (h *ContactsServiceHandler) ClearData() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.data = make(map[string]json.RawMessage)
}
