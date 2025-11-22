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
func (c *CRM) testGetCacheStats() (contactCount, leadCount int) {
	c.contactCache.Range(func(key, val interface{}) bool {
		contactCount++
		return true
	})

	c.leadCache.Range(func(key, val interface{}) bool {
		leadCount++
		return true
	})

	return contactCount, leadCount
}

// testClearCache очищает весь кэш
func (c *CRM) testClearCache() {
	c.contactCache.Range(func(key, val interface{}) bool {
		c.contactCache.Delete(key)
		return true
	})

	c.leadCache.Range(func(key, val interface{}) bool {
		c.leadCache.Delete(key)
		return true
	})

	logger.Info("Кэш CRM полностью очищен", c.conf.UserID)
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

	if err := crm.Init(23); err != nil {
		t.Fatalf("Failed to initialize CRM: %v", err)
	}

	msg := crm.MSG("user", "+54111234567", "Leo Gilligan", "Тестовое сообщение").
		//WithFiles("audio.mp3", "video.mp4").
		//NewDialog(true).
		WithVoice(true)
	//SetMeta(true)

	// Существующий контакт
	//msg := crm.MSG("user", "+79991234567", "Иван Иванов", "Тестовое сообщение")

	time.Sleep(10 * time.Millisecond)

	if err := crm.SendMessage(msg); err != nil {
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
	crmInstance := New(ctx, cfg)

	if err := crmInstance.Init(23); err != nil {
		t.Fatalf("Failed to initialize CRM: %v", err)
	}

	logger.Infoln(crmInstance.conf)

	// Проверяем статистику кэша
	contactCount, leadCount := crmInstance.testGetCacheStats()
	logger.Infoln("Cache stats - Contacts:", contactCount, "Leads:", leadCount)
}

func TestCRM_Cache(t *testing.T) {
	t.Skip("Manual test - requires running server")

	cfg := getConfig()
	cfg.WEB.CRM = "8092"

	ctx := context.Background()
	crmInstance := New(ctx, cfg)

	if err := crmInstance.Init(23); err != nil {
		t.Fatalf("Failed to initialize CRM: %v", err)
	}

	// Создаём тестовые сообщения
	msg1 := crmInstance.MSG("user", "+54111234567", "Leo Gilligan", "Первое сообщение").
		NewDialog(true)
	msg2 := crmInstance.MSG("assist", "+54111234567", "Leo Gilligan", "Второе сообщение").
		SetMeta(true)
	msg3 := crmInstance.MSG("user", "+79991234567", "John Smith", "Другой пользователь").
		WithVoice(true)

	// Отправляем первое сообщение (должно создать записи в кэше)
	if err := crmInstance.SendMessage(msg1); err != nil {
		t.Errorf("Failed to send message 1: %v", err)
	}

	// Проверяем статистику после первого сообщения
	contactCount, leadCount := crmInstance.testGetCacheStats()
	t.Logf("After msg1 - Contacts in cache: %d, Leads in cache: %d", contactCount, leadCount)

	// Отправляем второе сообщение (должно использовать кэш)
	if err := crmInstance.SendMessage(msg2); err != nil {
		t.Errorf("Failed to send message 2: %v", err)
	}

	// Проверяем, что количество записей не изменилось
	contactCount2, leadCount2 := crmInstance.testGetCacheStats()
	t.Logf("After msg2 - Contacts in cache: %d, Leads in cache: %d", contactCount2, leadCount2)

	if contactCount2 != contactCount {
		t.Errorf("Expected same contact count, got %d, want %d", contactCount2, contactCount)
	}

	// Отправляем сообщение от другого пользователя
	if err := crmInstance.SendMessage(msg3); err != nil {
		t.Errorf("Failed to send message 3: %v", err)
	}

	// Проверяем, что добавилась новая запись
	contactCount3, leadCount3 := crmInstance.testGetCacheStats()
	t.Logf("After msg3 - Contacts in cache: %d, Leads in cache: %d", contactCount3, leadCount3)

	if contactCount3 <= contactCount2 {
		t.Errorf("Expected more contacts in cache, got %d, want > %d", contactCount3, contactCount2)
	}

	// Очищаем кэш
	crmInstance.testClearCache()

	// Проверяем, что кэш пуст
	contactCount4, leadCount4 := crmInstance.testGetCacheStats()
	t.Logf("After testClearCache - Contacts in cache: %d, Leads in cache: %d", contactCount4, leadCount4)

	if contactCount4 != 0 || leadCount4 != 0 {
		t.Errorf("Expected empty cache, got contacts=%d, leads=%d", contactCount4, leadCount4)
	}

	time.Sleep(10 * time.Second)
}
