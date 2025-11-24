package crm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ikermy/AiR_Common/pkg/logger"
)

// ChannelsSettings AmoCRMSettings структура настроек AmoCRM канала
type ChannelsSettings struct {
	AmoCRM AmoCRMSettings `json:"amocrm"`
}

// Contact структура контакта
type Contact struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ContactResponse структура ответа при поиске контакта
type ContactResponse struct {
	Contact Contact `json:"contact"`
	Success bool    `json:"success"`
}

// CreateContact структура запроса для создания контакта
type CreateContact struct {
	Name         string        `json:"name"`
	Phone        string        `json:"phone,omitempty"`
	Email        string        `json:"email,omitempty"`
	AltContact   string        `json:"alt_contact,omitempty"`
	Tags         []string      `json:"tags,omitempty"`
	CustomFields []CustomField `json:"custom_fields,omitempty"`
}

type CustomField struct {
	ID int64 `json:"id,omitempty"`
}

// CreateContactResponse структура ответа при создании контакта
type CreateContactResponse struct {
	Contact Contact `json:"contact"`
	Message string  `json:"message"`
	Success bool    `json:"success"`
}

// Lead структура лида
type Lead struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ContactID string `json:"contact_id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// LeadResponse структура ответа при поиске лида
type LeadResponse struct {
	Leads   []Lead `json:"leads"`
	Success bool   `json:"success"`
}

// CreateLead структура запроса для создания лида
type CreateLead struct {
	ContactID string   `json:"contact_id"`
	LeadName  string   `json:"lead_name"`
	Tags      []string `json:"tags,omitempty"`
}

// CreateLeadResponse структура ответа при создании лида
type CreateLeadResponse struct {
	Lead    Lead   `json:"lead"`
	Message string `json:"message"`
	Success bool   `json:"success"`
}

// AddNote структура запроса для добавления заметки
type AddNote struct {
	LeadID   string `json:"lead_id"`
	NoteType string `json:"note_type"`
	Text     string `json:"text"`
}

// AddNoteResponse структура ответа при добавлении заметки
type AddNoteResponse struct {
	NoteID  string `json:"note_id"`
	Message string `json:"message"`
	Success bool   `json:"success"`
}

// UpdateLeadState структура запроса для обновления статуса лида
type UpdateLeadState struct {
	StatusID string `json:"status_id"`
}

// UpdateLeadStateResponse структура ответа при обновлении статуса лида
type UpdateLeadStateResponse struct {
	Message string `json:"message"`
	Success bool   `json:"success"`
}

func (u *User) sendRESP(method, url string, userID uint32, data ...[]byte) (*http.Response, error) {
	var bodyData io.Reader
	if len(data) > 0 {
		bodyData = bytes.NewBuffer(data[0])
	} else {
		bodyData = nil
	}

	reqCtx, cancel := context.WithTimeout(u.ctx, u.respTimeOut)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, url, bodyData)
	if err != nil {
		return nil, fmt.Errorf("ошибка при создании HTTP-запроса: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", fmt.Sprintf("%d", userID))

	// Проверка на инициализированный HTTP клиент
	if u.httpClient == nil {
		return nil, fmt.Errorf("HTTP клиент не инициализирован")
	}

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка при выполнении HTTP-запроса: %v", err)
	}

	return resp, nil
}

// ChannelsSettings Получение настроек каналов пользователя
func (u *User) ChannelsSettings(userID uint32) (*ChannelsSettings, error) {
	if userID == 0 {
		return nil, fmt.Errorf("userID не может быть 0")
	}

	url := fmt.Sprintf("http://localhost:%s/configs/%s/channels", u.port, Type)

	resp, err := u.sendRESP(http.MethodGet, url, userID, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения настроек каналов: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error("ошибка закрытия тела ответа: %v", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	// Парсим ответ согласно серверной структуре: {"success": true, "settings": {...}}
	var response struct {
		Success  bool           `json:"success"`
		Settings AmoCRMSettings `json:"settings"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	if !response.Success {
		return nil, fmt.Errorf("сервер вернул ошибку")
	}

	// Возвращаем настройки в обёртке ChannelsSettings
	channels := &ChannelsSettings{
		AmoCRM: response.Settings,
	}

	return channels, nil
}

// ContactID ищет контакт по номеру телефона и возвращает его
func (u *User) ContactID(contact string) (Contact, error) {
	url := fmt.Sprintf("http://localhost:%s/contacts/%s/search?phone=%s", u.port, Type, contact)

	resp, err := u.sendRESP(http.MethodGet, url, u.conf.UserID, nil)
	if err != nil {
		return Contact{}, fmt.Errorf("ошибка получения id контакта: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error("ошибка закрытия тела ответа: %v", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Contact{}, fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	var contactResp ContactResponse
	if err := json.Unmarshal(body, &contactResp); err != nil {
		return Contact{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	// контакт не найден возвращаем пустой контакт
	if !contactResp.Success {
		return Contact{}, nil
	}

	return contactResp.Contact, nil
}

// FindContactByAltContact ищет контакт по альтернативному имени (например, @telegram_username)
func (u *User) FindContactByAltContact(altContact string) (Contact, error) {
	url := fmt.Sprintf("http://localhost:%s/contacts/%s/search-by-alt?alt_contact=%s", u.port, Type, altContact)

	resp, err := u.sendRESP(http.MethodGet, url, u.conf.UserID, nil)
	if err != nil {
		return Contact{}, fmt.Errorf("ошибка поиска контакта по альтернативному имени: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error("ошибка закрытия тела ответа: %v", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Contact{}, fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	var contactResp ContactResponse
	if err := json.Unmarshal(body, &contactResp); err != nil {
		return Contact{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	// контакт не найден возвращаем пустой контакт
	if !contactResp.Success {
		return Contact{}, nil
	}

	return contactResp.Contact, nil
}

// CreateContact создает новый контакт
func (u *User) CreateContact(contact *CreateContact) (Contact, error) {
	if contact.Name == "" {
		return Contact{}, fmt.Errorf("имя контакта не может быть пустым")
	}

	url := fmt.Sprintf("http://localhost:%s/contacts/%s", u.port, Type)

	jsonData, err := json.Marshal(contact)
	if err != nil {
		return Contact{}, fmt.Errorf("ошибка кодирования JSON: %v", err)
	}

	resp, err := u.sendRESP(http.MethodPost, url, u.conf.UserID, jsonData)
	if err != nil {
		return Contact{}, fmt.Errorf("ошибка создания контакта: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error("ошибка закрытия тела ответа: %v", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Contact{}, fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	var createResp CreateContactResponse
	if err := json.Unmarshal(body, &createResp); err != nil {
		return Contact{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	if !createResp.Success {
		return Contact{}, fmt.Errorf("сервер вернул ошибку при создании контакта: %s", createResp.Message)
	}

	return createResp.Contact, nil
}

// FindLeadByContactID ищет лиды по ID контакта
func (u *User) FindLeadByContactID(contactID string) ([]Lead, error) {
	if contactID == "" {
		return nil, fmt.Errorf("contactID не может быть пустым")
	}

	url := fmt.Sprintf("http://localhost:%s/leads/%s/by-contact/%s", u.port, Type, contactID)

	resp, err := u.sendRESP(http.MethodGet, url, u.conf.UserID, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения лидов для контакта %s: %v", contactID, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error("ошибка закрытия тела ответа: %v", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	var leadResp LeadResponse
	if err := json.Unmarshal(body, &leadResp); err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	// лиды не найдены возвращаем пустой слайс
	if !leadResp.Success {
		return []Lead{}, nil
	}

	if len(leadResp.Leads) == 0 {
		logger.Debug("Лиды не найдены для контакта %s", contactID, u.conf.UserID)
		return []Lead{}, nil
	}

	return leadResp.Leads, nil
}

// NewLead создает новый лид в AmoCRM
func (u *User) NewLead(lead *CreateLead) (Lead, error) {
	url := fmt.Sprintf("http://localhost:%s/leads/%s/ai-dialog/%s", u.port, Type, lead.ContactID)

	jsonData, err := json.Marshal(lead)
	if err != nil {
		return Lead{}, fmt.Errorf("ошибка кодирования JSON: %v", err)
	}

	resp, err := u.sendRESP(http.MethodPost, url, u.conf.UserID, jsonData)
	if err != nil {
		return Lead{}, fmt.Errorf("ошибка создания лида: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error("ошибка закрытия тела ответа: %v", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Lead{}, fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	var createResp CreateLeadResponse
	if err := json.Unmarshal(body, &createResp); err != nil {
		return Lead{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	if !createResp.Success {
		return Lead{}, fmt.Errorf("сервер вернул ошибку при создании лида: %s", createResp.Message)
	}

	logger.Info("Лид успешно создан: ID=%s, Name=%s, ContactID=%s",
		createResp.Lead.ID, createResp.Lead.Name, createResp.Lead.ContactID, u.conf.UserID)

	return createResp.Lead, nil
}

// AddNote добавляет заметку к лиду
func (u *User) AddNote(note AddNote) error {
	if note.LeadID == "" {
		return fmt.Errorf("leadID не может быть пустым")
	}
	if note.Text == "" {
		return fmt.Errorf("текст заметки не может быть пустым")
	}

	url := fmt.Sprintf("http://localhost:%s/leads/%s/%s/notes", u.port, Type, note.LeadID)

	jsonData, err := json.Marshal(note)
	if err != nil {
		return fmt.Errorf("ошибка кодирования JSON: %v", err)
	}

	resp, err := u.sendRESP(http.MethodPost, url, u.conf.UserID, jsonData)
	if err != nil {
		return fmt.Errorf("ошибка добавления заметки: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error("ошибка закрытия тела ответа: %v", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	var noteResp AddNoteResponse
	if err := json.Unmarshal(body, &noteResp); err != nil {
		return fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	if !noteResp.Success {
		return fmt.Errorf("сервер вернул ошибку при добавлении заметки: %s", noteResp.Message)
	}

	return nil
}

// UpdateLeadState обновляет лид (перемещает в другой pipeline/status согласно настройкам БД)
func (u *User) UpdateLeadState(leadID string) error {
	if leadID == "" {
		return fmt.Errorf("leadID не может быть пустым")
	}

	url := fmt.Sprintf("http://localhost:%s/leads/%s/%s", u.port, Type, leadID)

	resp, err := u.sendRESP(http.MethodPatch, url, u.conf.UserID)
	if err != nil {
		return fmt.Errorf("ошибка обновления лида: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			logger.Error("ошибка закрытия тела ответа: %v", closeErr)
		}
	}()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("лид не найден: %s", leadID)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	var updateResp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &updateResp); err != nil {
		return fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	if !updateResp.Success {
		return fmt.Errorf("сервер вернул ошибку при обновлении лида: %s", updateResp.Message)
	}

	logger.Info("Лид успешно обновлён: LeadID=%s", leadID, u.conf.UserID)

	return nil
}
