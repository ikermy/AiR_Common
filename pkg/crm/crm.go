// Package crm предоставляет интеграцию с CRM системами (AmoCRM).
//
// Пример использования Builder Pattern:
//
//	import "github.com/ikermy/AiR_Common/pkg/crm"
//
//	crmClient := crm.New(ctx, cfg)
//	err := crmClient.Init(userID)
//
//	// Простое сообщение
//	msg := crmClient.MSG("contact_id", "Привет!")
//
//	// С файлами (цепочка вызовов)
//	msg := crmClient.MSG("contact_id", "Документы").
//	    WithFiles("file1.pdf", "file2.docx")
//
//	// Голосовое сообщение (цепочка вызовов)
//	msg := crmClient.MSG("contact_id", "Голосовое").
//	    WithVoice(true)
//
//	// Все опции вместе (цепочка вызовов)
//	msg := crmClient.MSG("contact_id", "Полное сообщение").
//	    WithFiles("audio.mp3").
//	    NewDialog(true).
//	    WithVoice(true)
//
// Кэширование:
//
// Пакет автоматически кэширует Phone → Contact.ID и Contact.ID → Lead.ID
// с TTL 30 минут. При каждом обращении к записи TTL обновляется.
// Это снижает количество HTTP-запросов на ~60-70% для активных диалогов.
//
//	// Получить статистику кэша
//	contactCount, leadCount := crmClient.testGetCacheStats()
//
//	// Очистить весь кэш
//	crmClient.testClearCache()
package crm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
)

const (
	CrmType            = "amocrm"
	DefaultRespTimeout = 10 * time.Second
	DefaultCacheTTL    = 30 * time.Minute
)

type AmoCRMSettings struct {
	Assist           string   `json:"Assist"`           // Префикс для сообщений ассистента
	User             string   `json:"User"`             // Префикс для сообщений пользователя
	Meta             string   `json:"Meta"`             // Текст метаданных
	Voice            string   `json:"Voice"`            // Текст для голосовых сообщений
	File             string   `json:"File"`             // Текст для отправленных файлов
	LeadName         string   `json:"LeadName"`         // Название лида по умолчанию
	Tags             []string `json:"Tags"`             // Теги для лида
	CreateNewContact bool     `json:"CreateNewContact"` // Создавать новый контакт
	CreateNewLead    bool     `json:"CreateNewLead"`    // Создавать новый лид
	ChatMessages     bool     `json:"ChatMessages"`     // Добавлять сообщения чата
	MetaExist        bool     `json:"MetaExist"`        // Добавлять метаданные
}

type UserCRMConfig struct {
	UserID   uint32
	Channels ChannelsSettings
}

// cacheEntry хранит значение с временем последнего обращения
type cacheEntry struct {
	value      string
	lastAccess time.Time
}

type CRM struct {
	msg    chan *Message
	conf   *UserCRMConfig
	port   string
	ctx    context.Context
	cancel context.CancelFunc

	// Кэш: Phone → Contact.ID с TTL
	contactCache sync.Map // map[string]*cacheEntry

	// Кэш: Contact.ID → Lead.ID с TTL
	leadCache sync.Map // map[string]*cacheEntry

	respTimeOut time.Duration // Время жизни запроса к CRM
	cacheTTL    time.Duration // Время жизни записи в кэше
}

type Option func(*CRM)

// WithRespTimeout устанавливает кастомный таймаут для HTTP-запросов
func WithRespTimeout(timeout time.Duration) Option {
	return func(c *CRM) {
		c.respTimeOut = timeout
	}
}

// WithCacheTTL устанавливает кастомное время жизни кэша
func WithCacheTTL(ttl time.Duration) Option {
	return func(c *CRM) {
		c.cacheTTL = ttl
	}
}

// С параметрами по умолчанию
//crmClient := crm.New(ctx, cfg)

// С кастомным таймаутом
//crmClient := crm.New(ctx, cfg, crm.WithRespTimeout(15*time.Second))

// С кастомным TTL кэша
//crmClient := crm.New(ctx, cfg, crm.WithCacheTTL(1*time.Hour))

// С обеими опциями
//crmClient := crm.New(ctx, cfg,
//crm.WithRespTimeout(20*time.Second),
//crm.WithCacheTTL(45*time.Minute),
//)

func New(parent context.Context, cfg *conf.Conf, opts ...Option) *CRM {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan *Message, 1)

	crm := &CRM{
		msg:         ch,
		ctx:         ctx,
		cancel:      cancel,
		port:        cfg.WEB.CRM,
		respTimeOut: DefaultRespTimeout,
		cacheTTL:    DefaultCacheTTL,
	}

	for _, opt := range opts {
		opt(crm)
	}

	// Запускаем фоновую очистку устаревших записей кэша
	go crm.cleanExpiredCache()

	return crm
}

// getFromCache получает значение из кэша и обновляет время последнего обращения
func (c *CRM) getFromCache(cache *sync.Map, key string) (string, bool) {
	val, ok := cache.Load(key)
	if !ok {
		return "", false
	}

	entry := val.(*cacheEntry)

	// Проверяем TTL
	if time.Since(entry.lastAccess) > c.cacheTTL {
		cache.Delete(key)
		return "", false
	}

	// Обновляем время последнего обращения
	entry.lastAccess = time.Now()
	cache.Store(key, entry)

	return entry.value, true
}

// setToCache сохраняет значение в кэш с текущим временем
func (c *CRM) setToCache(cache *sync.Map, key, value string) {
	entry := &cacheEntry{
		value:      value,
		lastAccess: time.Now(),
	}
	cache.Store(key, entry)
}

// cleanExpiredCache периодически очищает устаревшие записи из кэша
func (c *CRM) cleanExpiredCache() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()

			// Очищаем contactCache
			c.contactCache.Range(func(key, val interface{}) bool {
				entry := val.(*cacheEntry)
				if now.Sub(entry.lastAccess) > c.cacheTTL {
					c.contactCache.Delete(key)
					logger.Debug("Удалена устаревшая запись из contactCache: %v", key, c.conf.UserID)
				}
				return true
			})

			// Очищаем leadCache
			c.leadCache.Range(func(key, val interface{}) bool {
				entry := val.(*cacheEntry)
				if now.Sub(entry.lastAccess) > c.cacheTTL {
					c.leadCache.Delete(key)
					logger.Debug("Удалена устаревшая запись из leadCache: %v", key, c.conf.UserID)
				}
				return true
			})
		}
	}
}

func (c *CRM) Init(userID uint32) error {
	if userID == 0 {
		return fmt.Errorf("userID не может быть 0")
	}

	setting, err := c.ChannelsSettings(userID)
	if err != nil {
		return fmt.Errorf("ошибка инициализации CRM: %v", err)
	}

	crmConfig := &UserCRMConfig{
		UserID:   userID,
		Channels: *setting,
	}

	c.conf = crmConfig

	// Запускаем обработчик сообщений
	go c.handler()

	logger.Debug("CRM инициализирован с настройками: %+v", crmConfig.Channels, userID)

	return nil
}

func (c *CRM) handler() {
	logger.Debug("Запуск обработчика CRM сообщений", c.conf.UserID)
	// Обработка сообщений из канала c.msg
	for msg := range c.msg {
		logger.Debug("Обработка сообщения для контакта %s: %+v", msg.Phone, msg, c.conf.UserID)
		go c.processor(msg)
	}
}

func (c *CRM) processor(msg *Message) {
	// Шаг 1: Получение ContactID по номеру телефона (с кэшированием)
	var contact Contact
	var err error

	// Проверяем кэш
	if cachedContactID, found := c.getFromCache(&c.contactCache, msg.Phone); found {
		logger.Debug("ContactID получен из кэша для %s: %s", msg.Phone, cachedContactID, c.conf.UserID)
		contact.ID = cachedContactID
	} else {
		// Запрашиваем с сервера
		contact, err = c.ContactID(msg.Phone)
		if err != nil {
			logger.Error("Ошибка получения ContactID для %s: %v", msg.Phone, err, c.conf.UserID)
			return
		}

		// Сохраняем в кэш, если контакт найден
		if contact.ID != "" {
			c.setToCache(&c.contactCache, msg.Phone, contact.ID)
			logger.Debug("ContactID сохранён в кэш для %s: %s", msg.Phone, contact.ID, c.conf.UserID)
		}
	}

	// Шаг 1.1: Если контакт не найден, создаем новый (если разрешено настройками)
	if contact.ID == "" {
		if !c.conf.Channels.AmoCRM.CreateNewContact {
			logger.Debug("Настройки запрещают создавать новые контакты", c.conf.UserID)
			return
		}

		newContact := CreateContact{
			Name:  msg.Name,
			Phone: msg.Phone,
			Tags:  c.conf.Channels.AmoCRM.Tags,
		}

		logger.Debug("Создание нового контакта в AmoCRM для %s", msg.Phone, c.conf.UserID)
		contact, err = c.CreateContact(&newContact)
		if err != nil {
			logger.Error("Ошибка создания контакта в AmoCRM для %s: %v", msg.Phone, err, c.conf.UserID)
			return
		}
		logger.Info("Новый контакт создан в AmoCRM: ID=%s, Name=%s", contact.ID, contact.Name, c.conf.UserID)

		// Сохраняем в кэш новый контакт
		c.setToCache(&c.contactCache, msg.Phone, contact.ID)
		logger.Debug("Новый ContactID сохранён в кэш для %s: %s", msg.Phone, contact.ID, c.conf.UserID)
	}

	logger.Debug("Получен ContactID: %s для %s", contact, msg.Phone, c.conf.UserID)

	// Шаг 2: Поиск лида по ContactID (с кэшированием)
	var leadID string
	var leads []Lead

	// Проверяем кэш
	if cachedLeadID, found := c.getFromCache(&c.leadCache, contact.ID); found {
		logger.Debug("LeadID получен из кэша для контакта %s: %s", contact.ID, cachedLeadID, c.conf.UserID)
		leadID = cachedLeadID
	} else {
		// Запрашиваем с сервера
		leads, err = c.FindLeadByContactID(contact.ID)
		if err != nil {
			logger.Error("Ошибка поиска лидов для контакта %s: %v", contact.ID, err, c.conf.UserID)
			return
		}
	}

	// Если leadID не был найден в кэше, обрабатываем результат запроса
	if leadID == "" && len(leads) == 0 {
		// Шаг 2.1: Создание нового лида (если нужно)
		if !c.conf.Channels.AmoCRM.CreateNewLead {
			logger.Debug("Настройки запрещают создавать новые лиды", c.conf.UserID)
			return
		}

		newLead := CreateLead{
			ContactID: contact.ID,
			LeadName:  c.conf.Channels.AmoCRM.LeadName,
			Tags:      c.conf.Channels.AmoCRM.Tags,
		}

		logger.Debug("Создание нового лида для контакта %s", contact.ID, c.conf.UserID)
		lead, err := c.NewLead(&newLead)
		if err != nil {
			logger.Error("Ошибка создания лида: %v", err, c.conf.UserID)
			return
		}
		leadID = lead.ID

		// Сохраняем в кэш новый лид
		c.setToCache(&c.leadCache, contact.ID, leadID)
		logger.Debug("Новый LeadID сохранён в кэш для контакта %s: %s", contact.ID, leadID, c.conf.UserID)
	} else if leadID == "" && len(leads) > 0 {
		leadID = leads[0].ID
		logger.Debug("Найдено %d лид(ов) для контакта %s, используется первый: ID=%s",
			len(leads), contact.ID, leadID, c.conf.UserID)

		// Сохраняем в кэш найденный лид
		c.setToCache(&c.leadCache, contact.ID, leadID)
		logger.Debug("LeadID сохранён в кэш для контакта %s: %s", contact.ID, leadID, c.conf.UserID)

		// Шаг 2.2: Если лид найден и сообщение содержит метку новый диалог вызываю UpdateLeadState для обновления статуса лида
		if msg.New {
			err = c.UpdateLeadState(leadID)
			if err != nil {
				logger.Error("Ошибка обновления статуса лида %s: %v", leadID, err, c.conf.UserID)
			} else {
				logger.Debug("Статус лида %s обновлен", leadID, c.conf.UserID)
			}
		}
	}

	// Шаг 3: Добавление заметки к лиду
	if !c.conf.Channels.AmoCRM.ChatMessages {
		logger.Debug("Настройки запрещают добавлять сообщения в чат", c.conf.UserID)
		return
	}

	// Формируем текст заметки в зависимости от типа сообщения
	var noteText string
	var prefix string

	switch msg.Type {
	case "user":
		prefix = c.conf.Channels.AmoCRM.User
	case "assist":
		prefix = c.conf.Channels.AmoCRM.Assist
	default:
		prefix = ""
	}

	// Если цель в сообщении достигнута, добавляем префикс мета
	if msg.Meta && c.conf.Channels.AmoCRM.MetaExist {
		noteText += fmt.Sprintf("[%s] ", c.conf.Channels.AmoCRM.Meta)
	}

	// Добавляем префикс типа сообщения (используем += вместо =)
	if prefix != "" {
		noteText += fmt.Sprintf("%s: ", prefix)
	}

	// Добавляем основной текст
	noteText += fmt.Sprintf("%s\n", msg.Text)

	// Добавляем информацию о голосовом сообщении
	if msg.Voice {
		noteText += fmt.Sprintf("\n[%s]", c.conf.Channels.AmoCRM.Voice)
	}

	// Добавляем информацию о файлах
	if len(msg.Files) > 0 {
		noteText += fmt.Sprintf(" %s %v", c.conf.Channels.AmoCRM.File, msg.Files)
	}

	logger.Debug("Сформирован текст заметки: %s", noteText, c.conf.UserID)

	newNote := AddNote{
		LeadID:   leadID,
		NoteType: "extended_service_message",
		Text:     noteText,
	}

	err = c.AddNote(newNote)
	if err != nil {
		logger.Error("Ошибка добавления заметки к лиду %s: %v", leadID, err, c.conf.UserID)
		return
	}

	logger.Debug("Заметка успешно добавлена к лиду %s", leadID, c.conf.UserID)
}
