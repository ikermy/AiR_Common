// Package crm предоставляет интеграцию с User системами (AmoCRM).
//
// Пример использования Builder Pattern:
//
//	import "github.com/ikermy/AiR_Common/pkg/crm"
//
//	crmClient := crm.New(ctx, cfg)
//	user, err := crmClient.Init(userID)
//	// err проверяется только для логирования, user всегда возвращается
//	if err != nil {
//	    log.Warn("CRM не инициализирован: %v", err)
//	}
//
//	// Безопасное использование без дополнительных проверок
//	// Если user не инициализирован, методы молча вернут nil
//	msg := user.MSG("assist", "John Doe", "Привет!").
//	    WithPhone("+1234567890").
//	    WithFiles("file1.pdf", "file2.docx").
//	    SetMeta(true)
//
//	// Отправка сообщения (безопасна даже если user не инициализирован)
//	if err := user.SendMessage(msg); err != nil {
//	    log.Error("Ошибка отправки: %v", err)
//	}
//
//	// Простой пример с телефоном
//	msg := user.MSG("user", "Jane", "Голосовое").
//	    WithPhone("+1234567890").
//	    WithVoice(true)
//	user.SendMessage(msg)
//
//	// Пример с альтернативным контактом
//	msg := user.MSG("user", "Telegram User", "Сообщение").
//	    WithAltContact("@telegram_username")
//	user.SendMessage(msg)
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
	"crypto/tls"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
)

const (
	Type               = "amocrm"
	DefaultRespTimeout = 10 * time.Second
	DefaultCacheTTL    = 30 * time.Minute
	DefaultNumWorkers  = 10 // Количество воркеров в пуле
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
	AltContact       bool     `json:"AltContact"`       // Создавать контакты без номера телефона
	Telegram         int64    `json:"Telegram"`         // ID канала Telegram
	Instagram        int64    `json:"Instagram"`        // ID канала Instagram
	Widget           int64    `json:"Widget"`           // ID канала Widget
}

type UserCRMConfig struct {
	UserID       uint32
	Channels     ChannelsSettings
	AltContactID int64 // ID канала для создания контактов без номера телефона
}

// cacheEntry хранит значение с временем последнего обращения
type cacheEntry struct {
	value      string
	lastAccess time.Time
}

type CRM struct {
	ctx    context.Context
	cancel context.CancelFunc
	port   string

	user sync.Map // key: uint32 (userID), value: *User

	respTimeOut       time.Duration      // Время жизни запроса к User
	cacheTTL          time.Duration      // Время жизни записи в кэше
	numWorkers        uint8              // Количество воркеров в пуле
	altContactChannel *altContactChannel // Настройка канала для альтернативных контактов

}

type User struct {
	ctx    context.Context
	cancel context.CancelFunc
	port   string

	msg  chan *Message
	conf *UserCRMConfig

	// Многоразовый HTTP клиент
	httpClient *http.Client

	// Кэш: Phone → Contact.ID с TTL
	contactCache sync.Map // map[string]*cacheEntry

	// Кэш: AltContact → Contact.ID с TTL
	altContactCache sync.Map // map[string]*cacheEntry

	// Кэш: Contact.ID → Lead.ID с TTL
	leadCache sync.Map // map[string]*cacheEntry

	// Пул воркеров
	workerChannels []chan *Message

	respTimeOut time.Duration // Время жизни запроса к User
	cacheTTL    time.Duration // Время жизни записи в кэше
	numWorkers  uint8         // Количество воркеров в пуле

	// Контроль жизненного цикла горутин
	wg sync.WaitGroup
}

// AltContactChannelType тип канала для создания альтернативных контактов
type AltContactChannelType string

const (
	ChannelTelegram  AltContactChannelType = "telegram"
	ChannelInstagram AltContactChannelType = "instagram"
	ChannelWidget    AltContactChannelType = "widget"
)

type Option func(*CRM)

// altContactChannel хранит настройку канала для альтернативных контактов
type altContactChannel struct {
	channelType AltContactChannelType
}

// WithAltContactChannel устанавливает канал для создания контактов без номера телефона
// Используется совместно с настройкой AltContact = true
// Возможные значения: ChannelTelegram, ChannelInstagram, ChannelWidget
//
//nolint:unused // Экспортируемая функция для использования в других пакетах
func WithAltContactChannel(channelType AltContactChannelType) Option {
	return func(c *CRM) {
		if c.altContactChannel == nil {
			c.altContactChannel = &altContactChannel{}
		}
		c.altContactChannel.channelType = channelType
	}
}

// WithRespTimeout устанавливает кастомный таймаут для HTTP-запросов
//
//nolint:unused // Экспортируемая функция для использования в других пакетах
func WithRespTimeout(timeout time.Duration) Option {
	return func(c *CRM) {
		c.respTimeOut = timeout
	}
}

// WithCacheTTL устанавливает кастомное время жизни кэша
//
//nolint:unused // Экспортируемая функция для использования в других пакетах
func WithCacheTTL(ttl time.Duration) Option {
	return func(c *CRM) {
		c.cacheTTL = ttl
	}
}

// WithNumWorkers устанавливает количество воркеров в пуле
//
//nolint:unused // Экспортируемая функция для использования в других пакетах
func WithNumWorkers(n uint8) Option {
	return func(c *CRM) {
		if n > 0 {
			c.numWorkers = n
		}
	}
}

// С параметрами по умолчанию
//crmClient := crm.New(ctx, cfg)

// С кастомным таймаутом
//crmClient := crm.New(ctx, cfg, crm.WithRespTimeout(15*time.Second))

// С кастомным TTL кэша
//crmClient := crm.New(ctx, cfg, crm.WithCacheTTL(1*time.Hour))

// С каналом для альтернативных контактов
//crmClient := crm.New(ctx, cfg, crm.WithAltContactChannel(crm.ChannelTelegram))

// Со всеми опциями
//crmClient := crm.New(ctx, cfg,
//crm.WithRespTimeout(20*time.Second),
//crm.WithCacheTTL(45*time.Minute),
//crm.WithNumWorkers(20),
//crm.WithAltContactChannel(crm.ChannelTelegram),
//)

func New(parent context.Context, cfg *conf.Conf, opts ...Option) *CRM {
	ctx, cancel := context.WithCancel(parent)

	crm := &CRM{
		port:        cfg.WEB.CRM,
		ctx:         ctx,
		cancel:      cancel,
		respTimeOut: DefaultRespTimeout,
		cacheTTL:    DefaultCacheTTL,
		numWorkers:  DefaultNumWorkers,
	}

	for _, opt := range opts {
		opt(crm)
	}

	return crm
}

func (c *CRM) Init(userID uint32) (*User, error) {
	if userID == 0 {
		// Возвращаем пустой User вместо nil
		logger.Warn("userID не может быть 0, возвращаем неинициализированный User")
		return &User{}, fmt.Errorf("userID не может быть 0")
	}

	// Проверяем, существует ли уже пользователь
	if existingUser, exists := c.user.Load(userID); exists {
		logger.Debug("Пользователь с userID %d уже инициализирован, возвращаем существующий", userID)
		return existingUser.(*User), nil
	}

	// Создаем контекст для пользователя
	ctx, cancel := context.WithCancel(c.ctx)

	// Создаем новую структуру User
	u := &User{
		ctx:         ctx,
		cancel:      cancel,
		port:        c.port,
		respTimeOut: c.respTimeOut,
		cacheTTL:    c.cacheTTL,
		numWorkers:  c.numWorkers,
	}

	// Создаем многоразовый HTTP клиент с таймаутом ДО вызова ChannelsSettings
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	u.httpClient = &http.Client{
		Transport: tr,
	}

	// Получаем настройки каналов для пользователя
	setting, err := u.ChannelsSettings(userID)
	if err != nil {
		// Закрываем контекст
		cancel()
		// Закрываем HTTP клиент для предотвращения утечки ресурсов
		if transport, ok := u.httpClient.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
		logger.Warn("Не удалось получить настройки для userID %d: %v. Возвращаем неинициализированный User", userID, err)
		// Возвращаем пустой User (без ctx, httpClient и т.д.) для безопасного использования
		return &User{}, fmt.Errorf("ошибка получения настроек для userID %d: %v", userID, err)
	}

	crmConfig := &UserCRMConfig{
		UserID:   userID,
		Channels: *setting,
	}

	// Устанавливаем AltContactID если выполнены условия
	if setting.AmoCRM.AltContact && c.altContactChannel != nil {
		switch c.altContactChannel.channelType {
		case ChannelTelegram:
			crmConfig.AltContactID = setting.AmoCRM.Telegram
		case ChannelInstagram:
			crmConfig.AltContactID = setting.AmoCRM.Instagram
		case ChannelWidget:
			crmConfig.AltContactID = setting.AmoCRM.Widget
		}
	}

	u.conf = crmConfig

	u.msg = make(chan *Message, 10*DefaultNumWorkers)

	// Инициализируем каналы для воркеров
	u.workerChannels = make([]chan *Message, u.numWorkers)
	for i := uint8(0); i < u.numWorkers; i++ {
		u.workerChannels[i] = make(chan *Message, 10) // Буферизированный канал
	}

	// Регистрируем все горутины в WaitGroup
	u.wg.Add(int(u.numWorkers) + 3) // воркеры + waitParentCTX + dispatcher + cleanExpiredCache

	// Запускаем ожидание завершения родительского контекста
	go func() {
		defer u.wg.Done()
		u.waitParentCTX()
	}()

	// Запускаем воркеров
	for i := uint8(0); i < u.numWorkers; i++ {
		workerID := i // Создаём локальную копию для замыкания
		go func() {
			defer u.wg.Done()
			u.worker(workerID, u.workerChannels[workerID])
		}()
	}

	// Запускаем главный обработчик (диспетчер)
	go func() {
		defer u.wg.Done()
		u.dispatcher()
	}()

	// Запускаем фоновую очистку устаревших записей кэша
	go func() {
		defer u.wg.Done()
		u.cleanExpiredCache()
	}()

	// Добавляем пользователя в sync.Map
	c.user.Store(userID, u)

	logger.Debug("User инициализирован с настройками: %+v", crmConfig.Channels, userID)

	return u, nil
}

func (c *CRM) Shutdown() {
	c.cancel() // Отменяем контекст

	// Завершаем всех пользователей
	c.user.Range(func(key, value interface{}) bool {
		userID := key.(uint32)
		u := value.(*User)

		logger.Debug("Завершение пользователя %d", userID)

		// Отменяем контекст пользователя
		u.cancel()

		// Ждем завершения горутин пользователя
		done := make(chan struct{})
		go func() {
			u.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			logger.Debug("Пользователь %d успешно завершил работу", userID)
		case <-time.After(10 * time.Second):
			logger.Warn("Таймаут ожидания завершения пользователя %d", userID)
		}

		// Закрываем HTTP-клиент
		if transport, ok := u.httpClient.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}

		return true
	})

	logger.Info("CRM успешно завершил работу")
}

// getFromCache получает значение из кэша и обновляет время последнего обращения
func (u *User) getFromCache(cache *sync.Map, key string) (string, bool) {
	val, ok := cache.Load(key)
	if !ok {
		return "", false
	}

	entry := val.(*cacheEntry)

	// Проверяем TTL
	if time.Since(entry.lastAccess) > u.cacheTTL {
		cache.Delete(key)
		return "", false
	}

	// Создаем новую запись с обновленным временем доступа
	newEntry := &cacheEntry{
		value:      entry.value,
		lastAccess: time.Now(),
	}

	// Сохраняем обновленную запись в кэш
	cache.Store(key, newEntry)

	return newEntry.value, true
}

// setToCache сохраняет значение в кэш с текущим временем
func (u *User) setToCache(cache *sync.Map, key, value string) {
	entry := &cacheEntry{
		value:      value,
		lastAccess: time.Now(),
	}
	cache.Store(key, entry)
}

// cleanExpiredCache периодически очищает устаревшие записи из кэша
func (u *User) cleanExpiredCache() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-u.ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()

			// Очищаем contactCache
			u.contactCache.Range(func(key, val interface{}) bool {
				entry := val.(*cacheEntry)
				if now.Sub(entry.lastAccess) > u.cacheTTL {
					u.contactCache.Delete(key)
				}
				return true
			})

			// Очищаем altContactCache
			u.altContactCache.Range(func(key, val interface{}) bool {
				entry := val.(*cacheEntry)
				if now.Sub(entry.lastAccess) > u.cacheTTL {
					u.altContactCache.Delete(key)
				}
				return true
			})

			// Очищаем leadCache
			u.leadCache.Range(func(key, val interface{}) bool {
				entry := val.(*cacheEntry)
				if now.Sub(entry.lastAccess) > u.cacheTTL {
					u.leadCache.Delete(key)
				}
				return true
			})
		}
	}
}

// waitParentCTX ожидает завершения родительского контекста и закрывает канал сообщений
func (u *User) waitParentCTX() {
	<-u.ctx.Done()
	close(u.msg)
	// Не логируем userID, т.к. u.conf может быть nil для неинициализированного user
}

// dispatcher читает из главного канала и распределяет сообщения по воркерам
func (u *User) dispatcher() {
	logger.Debug("Запуск диспетчера User сообщений", u.conf.UserID)
	for {
		select {
		case msg, ok := <-u.msg:
			if !ok {
				// Канал закрыт, завершаем работу
				for _, ch := range u.workerChannels {
					close(ch)
				}
				logger.Debug("Диспетчер User сообщений завершил работу", u.conf.UserID)
				return
			}

			hasher := fnv.New32a()
			_, _ = hasher.Write([]byte(msg.Phone)) // ошибки не может быть, просто игнорирую IDE
			workerIndex := uint8(hasher.Sum32()) % u.numWorkers

			select {
			case u.workerChannels[workerIndex] <- msg:
			case <-u.ctx.Done():
				return
			}
		case <-u.ctx.Done():
			return
		}
	}
}

// worker - это горутина, которая последовательно обрабатывает сообщения из своего канала
func (u *User) worker(id uint8, messages <-chan *Message) {
	logger.Debug("Запуск воркера User %d", id, u.conf.UserID)
	for msg := range messages {
		logger.Debug("Воркер %d получил сообщение для %s", id, msg.Phone, u.conf.UserID)
		u.processor(msg)
	}
	logger.Debug("Воркер User %d завершил работу", id, u.conf.UserID)
}

func (u *User) processor(msg *Message) {
	// Определяем приоритет: телефон имеет приоритет над альтернативным контактом
	var contactID string
	var contactIdentifier string
	var useAltContact bool

	// Приоритет: если есть телефон, используем его, иначе альтернативный контакт
	if msg.Phone != "" {
		contactIdentifier = msg.Phone
		useAltContact = false
		logger.Debug("Использование телефона как идентификатора: %s", msg.Phone, u.conf.UserID)
	} else if msg.AltContact != "" && u.conf.Channels.AmoCRM.AltContact {
		contactIdentifier = msg.AltContact
		useAltContact = true
		logger.Debug("Использование альтернативного контакта как идентификатора: %s", msg.AltContact, u.conf.UserID)
	} else {
		logger.Warn("Не указан ни телефон, ни альтернативный контакт для сообщения", u.conf.UserID)
		return
	}

	// Шаг 1: Получение или создание ContactID
	// Проверяем соответствующий кэш
	if useAltContact {
		// Работаем с кэшем альтернативных контактов
		if cachedContactID, found := u.getFromCache(&u.altContactCache, contactIdentifier); found {
			logger.Debug("ContactID получен из кэша altContact для %s: %s", contactIdentifier, cachedContactID, u.conf.UserID)
			contactID = cachedContactID
		} else {
			// Запрашиваем с сервера по альтернативному контакту
			contact, err := u.FindContactByAltContact(msg.AltContact)
			if err != nil {
				logger.Error("Ошибка поиска ContactID по альтернативному контакту %s: %v", msg.AltContact, err, u.conf.UserID)
				return
			}

			// Если контакт не найден, создаем новый
			if contact.ID == "" {
				if !u.conf.Channels.AmoCRM.CreateNewContact {
					logger.Warn("Создание новых контактов запрещено настройками для %s", msg.AltContact, u.conf.UserID)
					return
				}

				newContactData := CreateContact{
					Name:       msg.Name,
					AltContact: msg.AltContact,
					Tags:       u.conf.Channels.AmoCRM.Tags,
				}

				// Добавляем CustomField если настроен AltContactID
				if u.conf.AltContactID != 0 {
					newContactData.CustomFields = []CustomField{{ID: u.conf.AltContactID}}
				}

				logger.Debug("Создание нового контакта в AmoCRM для альтернативного контакта %s", msg.AltContact, u.conf.UserID)
				contact, err = u.CreateContact(&newContactData)
				if err != nil {
					logger.Error("Ошибка создания контакта в AmoCRM для %s: %v", msg.AltContact, err, u.conf.UserID)
					return
				}
				logger.Info("Новый контакт создан в AmoCRM: ID=%s, Name=%s, AltContact=%s", contact.ID, contact.Name, msg.AltContact, u.conf.UserID)
			}

			// Сохраняем в кэш альтернативных контактов
			u.setToCache(&u.altContactCache, contactIdentifier, contact.ID)
			logger.Debug("ContactID сохранён в кэш altContact для %s: %s", contactIdentifier, contact.ID, u.conf.UserID)
			contactID = contact.ID
		}
	} else {
		// Работаем с кэшем телефонов
		if cachedContactID, found := u.getFromCache(&u.contactCache, contactIdentifier); found {
			logger.Debug("ContactID получен из кэша phone для %s: %s", contactIdentifier, cachedContactID, u.conf.UserID)
			contactID = cachedContactID
		} else {
			// Запрашиваем с сервера по телефону
			contact, err := u.ContactID(msg.Phone)
			if err != nil {
				logger.Error("Ошибка получения ContactID для %s: %v", msg.Phone, err, u.conf.UserID)
				return
			}

			// Если контакт не найден, создаем новый
			if contact.ID == "" {
				if !u.conf.Channels.AmoCRM.CreateNewContact {
					logger.Warn("Создание новых контактов запрещено настройками для %s", msg.Phone, u.conf.UserID)
					return
				}

				newContactData := CreateContact{
					Name:  msg.Name,
					Phone: msg.Phone,
					Tags:  u.conf.Channels.AmoCRM.Tags,
				}

				logger.Debug("Создание нового контакта в AmoCRM для %s", msg.Phone, u.conf.UserID)
				contact, err = u.CreateContact(&newContactData)
				if err != nil {
					logger.Error("Ошибка создания контакта в AmoCRM для %s: %v", msg.Phone, err, u.conf.UserID)
					return
				}
				logger.Info("Новый контакт создан в AmoCRM: ID=%s, Name=%s, Phone=%s", contact.ID, contact.Name, msg.Phone, u.conf.UserID)
			}

			// Сохраняем в кэш телефонов
			u.setToCache(&u.contactCache, contactIdentifier, contact.ID)
			logger.Debug("ContactID сохранён в кэш phone для %s: %s", contactIdentifier, contact.ID, u.conf.UserID)
			contactID = contact.ID
		}
	}

	logger.Debug("Получен ContactID: %s для %s", contactID, contactIdentifier, u.conf.UserID)

	// Шаг 2: Получение или создание LeadID
	var leadID string
	// Проверяем кэш
	if cachedLeadID, found := u.getFromCache(&u.leadCache, contactID); found {
		logger.Debug("LeadID получен из кэша для контакта %s: %s", contactID, cachedLeadID, u.conf.UserID)
		leadID = cachedLeadID
	} else {
		// Запрашиваем с сервера
		leads, err := u.FindLeadByContactID(contactID)
		if err != nil {
			logger.Error("Ошибка поиска лидов для контакта %s: %v", contactID, err, u.conf.UserID)
			return
		}

		if len(leads) > 0 {
			leadID = leads[0].ID
			logger.Debug("Найдено %d лид(ов) для контакта %s, используется первый: ID=%s", len(leads), contactID, leadID, u.conf.UserID)
			// Если лид найден и сообщение содержит метку "новый диалог", обновляем статус
			if msg.New {
				if err := u.UpdateLeadState(leadID); err != nil {
					logger.Error("Ошибка обновления статуса лида %s: %v", leadID, err, u.conf.UserID)
				} else {
					logger.Debug("Статус лида %s обновлен", leadID, u.conf.UserID)
				}
			}
		} else {
			// Создание нового лида
			if !u.conf.Channels.AmoCRM.CreateNewLead {
				logger.Warn("Создание новых лидов запрещено настройками для контакта %s", contactID, u.conf.UserID)
				return
			}
			newLeadData := CreateLead{ContactID: contactID, LeadName: u.conf.Channels.AmoCRM.LeadName, Tags: u.conf.Channels.AmoCRM.Tags}
			logger.Debug("Создание нового лида для контакта %s", contactID, u.conf.UserID)
			lead, err := u.NewLead(&newLeadData)
			if err != nil {
				logger.Error("Ошибка создания лида: %v", err, u.conf.UserID)
				return
			}
			leadID = lead.ID
		}

		// Сохраняем в кэш
		u.setToCache(&u.leadCache, contactID, leadID)
		logger.Debug("LeadID сохранён в кэш для контакта %s: %s", contactID, leadID, u.conf.UserID)
	}
	logger.Debug("Получен LeadID: %s для контакта %s", leadID, contactID, u.conf.UserID)

	// Шаг 3: Добавление заметки к лиду
	// ... (остальная часть функции без изменений) ...
	if !u.conf.Channels.AmoCRM.ChatMessages {
		logger.Debug("Настройки запрещают добавлять сообщения в чат", u.conf.UserID)
		return
	}

	var sb strings.Builder

	// Префикс по типу сообщения
	var prefix string
	switch msg.Type {
	case "user":
		prefix = u.conf.Channels.AmoCRM.User
	case "assist":
		prefix = u.conf.Channels.AmoCRM.Assist
	}

	// Если цель достигнута
	if msg.Meta && u.conf.Channels.AmoCRM.MetaExist {
		sb.WriteString("[" + u.conf.Channels.AmoCRM.Meta + "] ")
	}

	// Префикс типа сообщения
	if prefix != "" {
		sb.WriteString(prefix)
		sb.WriteString(": ")
	}

	// Основной текст
	sb.WriteString(msg.Text)
	sb.WriteByte('\n')

	// Голосовое сообщение
	if msg.Voice {
		sb.WriteString("\n[" + u.conf.Channels.AmoCRM.Voice + "]")
	}

	// Файлы
	if len(msg.Files) > 0 {
		sb.WriteString(" " + u.conf.Channels.AmoCRM.File + " ")
		sb.WriteString(strings.Join(msg.Files, ", "))
	}

	logger.Debug("Сформирован текст заметки: %s", sb.String(), u.conf.UserID)

	newNote := AddNote{
		LeadID:   leadID,
		NoteType: "extended_service_message",
		Text:     sb.String(),
	}

	err := u.AddNote(newNote)
	if err != nil {
		logger.Error("Ошибка добавления заметки к лиду %s: %v", leadID, err, u.conf.UserID)
		return
	}

	logger.Debug("Заметка успешно добавлена к лиду %s", leadID, u.conf.UserID)
}
