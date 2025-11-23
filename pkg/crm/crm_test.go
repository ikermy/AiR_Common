package crm

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
)

func getConfig() *conf.Conf {
	// Переходим в корневую директорию проекта
	if err := os.Chdir("../.."); err != nil {
		log.Fatalf("Failed to change to root directory: %v", err)
	}

	cfg, err := conf.NewConf()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Инициализируем логгер для тестов
	const LogPatch = "/var/log/Marusia_TEST/common.log"
	logger.Set(LogPatch)

	return cfg
}

// testGetCacheStats возвращает статистику использования кэша
func (u *User) testGetCacheStats() (contactCount, leadCount int) {
	u.contactCache.Range(func(key, val interface{}) bool {
		contactCount++
		return true
	})

	u.leadCache.Range(func(key, val interface{}) bool {
		leadCount++
		return true
	})

	return contactCount, leadCount
}

// testClearCache очищает весь кэш
func (u *User) testClearCache() {
	u.contactCache.Range(func(key, val interface{}) bool {
		u.contactCache.Delete(key)
		return true
	})

	u.leadCache.Range(func(key, val interface{}) bool {
		u.leadCache.Delete(key)
		return true
	})

	if u.conf != nil {
		logger.Info("Кэш User полностью очищен", u.conf.UserID)
	} else {
		logger.Info("Кэш неинициализированного User очищен")
	}
}

func TestCRM(t *testing.T) {
	// Получаем конфигурацию
	cfg := getConfig()

	// Используем реальный сервер на порту 8092
	cfg.WEB.CRM = "8092"

	// Создаём контекст
	ctx := context.Background()

	// Создаём экземпляр CRM
	crm := New(ctx, cfg)

	// Инициализируем пользователя
	c, err := crm.Init(23)
	if err != nil {
		t.Fatalf("Failed to initialize User: %v", err)
	}

	msg := c.MSG("c", "+54111234567", "Leo Gilligan", "Тестовое сообщение").
		//WithFiles("audio.mp3", "video.mp4").
		//NewDialog(true).
		WithVoice(true)
	//SetMeta(true)

	// Существующий контакт
	//msg := c.MSG("c", "+79991234567", "Иван Иванов", "Тестовое сообщение")

	time.Sleep(10 * time.Millisecond)

	if err := c.SendMessage(msg); err != nil {
		t.Fatalf("Failed to send message: %v", err)
	}

	time.Sleep(5 * time.Second)
}

func TestCRM_Init(t *testing.T) {
	// Получаем конфигурацию
	cfg := getConfig()

	// Используем реальный сервер на порту 8092
	cfg.WEB.CRM = "8092"

	// Создаём контекст
	ctx := context.Background()

	// Создаём экземпляр CRM
	crm := New(ctx, cfg)

	// Инициализируем пользователя
	user, err := crm.Init(23)
	if err != nil {
		t.Fatalf("Failed to initialize User: %v", err)
	}

	logger.Infoln(user.conf)

	// Проверяем статистику кэша
	contactCount, leadCount := user.testGetCacheStats()
	logger.Infoln("Cache stats - Contacts:", contactCount, "Leads:", leadCount)
}

func TestCRM_Cache(t *testing.T) {
	t.Skip("Manual test - requires running server")

	cfg := getConfig()
	cfg.WEB.CRM = "8092"

	ctx := context.Background()
	crm := New(ctx, cfg)

	// Инициализируем пользователя
	user, err := crm.Init(23)
	if err != nil {
		t.Fatalf("Failed to initialize User: %v", err)
	}

	// Создаём тестовые сообщения
	msg1 := user.MSG("user", "+54111234567", "Leo Gilligan", "Первое сообщение").
		NewDialog(true)
	msg2 := user.MSG("assist", "+54111234567", "Leo Gilligan", "Второе сообщение").
		SetMeta(true)
	msg3 := user.MSG("user", "+79991234567", "John Smith", "Другой пользователь").
		WithVoice(true)

	// Отправляем первое сообщение (должно создать записи в кэше)
	if err := user.SendMessage(msg1); err != nil {
		t.Errorf("Failed to send message 1: %v", err)
	}

	// Проверяем статистику после первого сообщения
	contactCount, leadCount := user.testGetCacheStats()
	t.Logf("After msg1 - Contacts in cache: %d, Leads in cache: %d", contactCount, leadCount)

	// Отправляем второе сообщение (должно использовать кэш)
	if err := user.SendMessage(msg2); err != nil {
		t.Errorf("Failed to send message 2: %v", err)
	}

	// Проверяем, что количество записей не изменилось
	contactCount2, leadCount2 := user.testGetCacheStats()
	t.Logf("After msg2 - Contacts in cache: %d, Leads in cache: %d", contactCount2, leadCount2)

	if contactCount2 != contactCount {
		t.Errorf("Expected same contact count, got %d, want %d", contactCount2, contactCount)
	}

	// Отправляем сообщение от другого пользователя
	if err := user.SendMessage(msg3); err != nil {
		t.Errorf("Failed to send message 3: %v", err)
	}

	// Проверяем, что добавилась новая запись
	contactCount3, leadCount3 := user.testGetCacheStats()
	t.Logf("After msg3 - Contacts in cache: %d, Leads in cache: %d", contactCount3, leadCount3)

	if contactCount3 <= contactCount2 {
		t.Errorf("Expected more contacts in cache, got %d, want > %d", contactCount3, contactCount2)
	}

	// Очищаем кэш
	user.testClearCache()

	// Проверяем, что кэш пуст
	contactCount4, leadCount4 := user.testGetCacheStats()
	t.Logf("After testClearCache - Contacts in cache: %d, Leads in cache: %d", contactCount4, leadCount4)

	if contactCount4 != 0 || leadCount4 != 0 {
		t.Errorf("Expected empty cache, got contacts=%d, leads=%d", contactCount4, leadCount4)
	}

	time.Sleep(10 * time.Second)
}

// TestUninitializedUser проверяет безопасность работы с неинициализированным User
func TestUninitializedUser(t *testing.T) {
	cfg := getConfig()
	cfg.WEB.CRM = "8092"

	ctx := context.Background()
	crm := New(ctx, cfg)

	// Инициализируем с несуществующим userID (должна вернуться ошибка)
	user, err := crm.Init(99999)
	if err == nil {
		t.Log("Ожидалась ошибка инициализации, но получили nil")
	}

	// Проверяем, что user не nil (даже при ошибке)
	if user == nil {
		t.Fatal("User не должен быть nil даже при ошибке инициализации")
	}

	// Проверяем безопасность вызова методов на неинициализированном user
	msg := user.MSG("assist", "+1234567890", "Test User", "Тестовое сообщение").
		WithFiles("file1.pdf").
		SetMeta(true)

	// Отправка должна пройти без паники, просто вернуть nil а не err
	err = user.SendMessage(msg)
	if err != nil {
		t.Fatalf("SendMessage вернул ошибку (значит что то не так, не должен возвращать ошибку для неинициализированного User): %v", err)
	}

	t.Log("Тест безопасности неинициализированного User пройден успешно")
}

// TestCRM_Options демонстрирует использование опциональных параметров
func TestCRM_Options(t *testing.T) {
	t.Skip("Example test - demonstrates usage of optional parameters")

	cfg := getConfig()
	cfg.WEB.CRM = "8092"
	ctx := context.Background()

	// Пример 1: Все опции вместе
	crm1 := New(ctx, cfg,
		WithRespTimeout(20*time.Second),
		WithCacheTTL(1*time.Hour),
		WithNumWorkers(20),
	)
	t.Logf("CRM создан с кастомными параметрами: %+v", crm1)

	// Пример 2: Только таймаут
	crm2 := New(ctx, cfg, WithRespTimeout(15*time.Second))
	t.Logf("CRM создан с кастомным таймаутом: %+v", crm2)

	// Пример 3: Только кэш TTL
	crm3 := New(ctx, cfg, WithCacheTTL(45*time.Minute))
	t.Logf("CRM создан с кастомным TTL кэша: %+v", crm3)

	// Пример 4: Только количество воркеров
	crm4 := New(ctx, cfg, WithNumWorkers(5))
	t.Logf("CRM создан с 5 воркерами: %+v", crm4)
}
