package endpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/model"
)

// MockDB мок базы данных для тестирования
type MockDB struct {
	saveDialogCalls             atomic.Int64
	updateDialogsMetaCalls      atomic.Int64
	getNotificationChannelCalls atomic.Int64
	saveDialogLatency           time.Duration
	mu                          sync.Mutex
	errors                      []error
}

func NewMockDB() *MockDB {
	return &MockDB{
		saveDialogLatency: 1 * time.Millisecond, // Эмуляция задержки БД
	}
}

func (m *MockDB) SaveDialog(threadId uint64, message json.RawMessage) error {
	m.saveDialogCalls.Add(1)
	if m.saveDialogLatency > 0 {
		time.Sleep(m.saveDialogLatency)
	}
	return nil
}

func (m *MockDB) UpdateDialogsMeta(dialogId uint64, meta string) error {
	m.updateDialogsMetaCalls.Add(1)
	return nil
}

func (m *MockDB) GetNotificationChannel(userId uint32) (json.RawMessage, error) {
	m.getNotificationChannelCalls.Add(1)
	return json.RawMessage(`{}`), nil
}

func (m *MockDB) GetSaveDialogCalls() int64 {
	return m.saveDialogCalls.Load()
}

func (m *MockDB) GetUpdateDialogsMetaCalls() int64 {
	return m.updateDialogsMetaCalls.Load()
}

// LoadTestMetrics метрики нагрузочного теста
type LoadTestMetrics struct {
	TotalOperations   atomic.Int64
	SuccessfulOps     atomic.Int64
	FailedOps         atomic.Int64
	TotalResponseTime atomic.Int64 // в микросекундах
	MinResponseTime   atomic.Int64
	MaxResponseTime   atomic.Int64
	SaveDialogOps     atomic.Int64
	SetUserAskOps     atomic.Int64
	GetUserAskOps     atomic.Int64
	FlushOps          atomic.Int64
	StartTime         time.Time
	EndTime           time.Time
}

func NewLoadTestMetrics() *LoadTestMetrics {
	m := &LoadTestMetrics{
		StartTime: time.Now(),
	}
	m.MinResponseTime.Store(int64(^uint64(0) >> 1)) // Максимальное значение int64
	return m
}

func (m *LoadTestMetrics) UpdateResponseTime(duration time.Duration) {
	us := duration.Microseconds()
	m.TotalResponseTime.Add(us)

	// Обновляем минимум
	for {
		oldMin := m.MinResponseTime.Load()
		if us >= oldMin || m.MinResponseTime.CompareAndSwap(oldMin, us) {
			break
		}
	}

	// Обновляем максимум
	for {
		oldMax := m.MaxResponseTime.Load()
		if us <= oldMax || m.MaxResponseTime.CompareAndSwap(oldMax, us) {
			break
		}
	}
}

func (m *LoadTestMetrics) PrintReport(t *testing.T) {
	m.EndTime = time.Now()
	duration := m.EndTime.Sub(m.StartTime)

	t.Log("\n" + "═════════════════════════════════════════════════════════")
	t.Log("           ОТЧЁТ НАГРУЗОЧНОГО ТЕСТИРОВАНИЯ")
	t.Log("═════════════════════════════════════════════════════════")
	t.Logf("Время выполнения: %v", duration)
	t.Logf("Всего операций: %d", m.TotalOperations.Load())
	t.Logf("Успешных операций: %d", m.SuccessfulOps.Load())
	t.Logf("Неудачных операций: %d", m.FailedOps.Load())

	if m.SuccessfulOps.Load() > 0 {
		avgTime := float64(m.TotalResponseTime.Load()) / float64(m.SuccessfulOps.Load())
		t.Logf("Среднее время ответа: %.2f мкс (%.3f мс)", avgTime, avgTime/1000.0)
		t.Logf("Минимальное время ответа: %d мкс", m.MinResponseTime.Load())
		t.Logf("Максимальное время ответа: %d мкс", m.MaxResponseTime.Load())

		opsPerSec := float64(m.SuccessfulOps.Load()) / duration.Seconds()
		t.Logf("Операций в секунду: %.2f", opsPerSec)
	}

	t.Log("\nДетализация операций:")
	t.Logf("  - SaveDialog: %d", m.SaveDialogOps.Load())
	t.Logf("  - SetUserAsk: %d", m.SetUserAskOps.Load())
	t.Logf("  - GetUserAsk: %d", m.GetUserAskOps.Load())
	t.Logf("  - Flush: %d", m.FlushOps.Load())
	t.Log("═════════════════════════════════════════════════════════")
}

// TestEndpointConcurrentSaveDialog тестирует параллельное сохранение диалогов
func TestEndpointConcurrentSaveDialog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockDB := NewMockDB()
	endpoint := New(ctx, mockDB)
	defer endpoint.Shutdown()

	const (
		numGoroutines        = 100
		messagesPerGoroutine = 100
	)

	metrics := NewLoadTestMetrics()
	var wg sync.WaitGroup

	t.Logf("Запуск теста: %d горутин по %d сообщений каждая", numGoroutines, messagesPerGoroutine)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineId int) {
			defer wg.Done()

			threadId := uint64(goroutineId % 10) // 10 разных тредов
			for j := 0; j < messagesPerGoroutine; j++ {
				start := time.Now()

				resp := model.AssistResponse{
					Message: fmt.Sprintf("Сообщение %d от горутины %d", j, goroutineId),
				}

				endpoint.SaveDialog(comdb.AI, threadId, &resp)

				duration := time.Since(start)
				metrics.UpdateResponseTime(duration)
				metrics.TotalOperations.Add(1)
				metrics.SuccessfulOps.Add(1)
				metrics.SaveDialogOps.Add(1)
			}
		}(i)
	}

	wg.Wait()
	endpoint.FlushAllBatches()

	time.Sleep(100 * time.Millisecond) // Даём время на завершение операций

	metrics.PrintReport(t)

	totalExpected := int64(numGoroutines * messagesPerGoroutine)
	dbCalls := mockDB.GetSaveDialogCalls()

	t.Logf("\nВызовов SaveDialog в БД: %d из %d операций", dbCalls, totalExpected)
	t.Logf("Эффективность батчинга: %.1f%% (меньше - лучше)", float64(dbCalls)/float64(totalExpected)*100)

	if dbCalls == 0 {
		t.Error("Не было вызовов SaveDialog в БД")
	}
}

// TestEndpointSetGetUserAsk тестирует операции SetUserAsk и GetUserAsk
func TestEndpointSetGetUserAsk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockDB := NewMockDB()
	endpoint := New(ctx, mockDB)
	defer endpoint.Shutdown()

	const (
		numGoroutines          = 50
		operationsPerGoroutine = 200
	)

	metrics := NewLoadTestMetrics()
	var wg sync.WaitGroup

	t.Logf("Запуск теста SetUserAsk/GetUserAsk: %d горутин по %d операций", numGoroutines, operationsPerGoroutine)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineId int) {
			defer wg.Done()

			dialogId := uint64(goroutineId % 10)
			respId := uint64(goroutineId)

			for j := 0; j < operationsPerGoroutine; j++ {
				start := time.Now()

				// Set операция
				ask := fmt.Sprintf("Вопрос %d от горутины %d", j, goroutineId)
				success := endpoint.SetUserAsk(dialogId, respId, ask, 10000)

				if success {
					metrics.SuccessfulOps.Add(1)
					metrics.SetUserAskOps.Add(1)
				} else {
					metrics.FailedOps.Add(1)
				}

				duration := time.Since(start)
				metrics.UpdateResponseTime(duration)
				metrics.TotalOperations.Add(1)

				// Периодически делаем Get
				if j%10 == 9 {
					start = time.Now()
					asks := endpoint.GetUserAsk(dialogId, respId)

					if asks != nil {
						metrics.SuccessfulOps.Add(1)
						metrics.GetUserAskOps.Add(1)
					}

					duration = time.Since(start)
					metrics.UpdateResponseTime(duration)
					metrics.TotalOperations.Add(1)
				}
			}
		}(i)
	}

	wg.Wait()
	metrics.PrintReport(t)
}

// TestEndpointMixedLoad тестирует смешанную нагрузку всех операций
func TestEndpointMixedLoad(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockDB := NewMockDB()
	endpoint := New(ctx, mockDB)
	defer endpoint.Shutdown()

	const (
		numGoroutines = 100
		duration      = 5 * time.Second
	)

	metrics := NewLoadTestMetrics()
	var wg sync.WaitGroup
	stopChan := make(chan struct{})

	t.Logf("Запуск смешанного теста: %d горутин, длительность %v", numGoroutines, duration)

	// Запускаем таймер
	time.AfterFunc(duration, func() {
		close(stopChan)
	})

	// Запускаем воркеры
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(workerId int) {
			defer wg.Done()

			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerId)))
			dialogId := uint64(workerId % 20)
			respId := uint64(workerId)
			threadId := uint64(workerId % 10)

			for {
				select {
				case <-stopChan:
					return
				default:
					operation := rng.Intn(100)
					start := time.Now()

					switch {
					case operation < 40: // 40% - SaveDialog
						resp := model.AssistResponse{
							Message: fmt.Sprintf("Сообщение от воркера %d", workerId),
						}
						endpoint.SaveDialog(comdb.AI, threadId, &resp)
						metrics.SaveDialogOps.Add(1)
						metrics.SuccessfulOps.Add(1)

					case operation < 70: // 30% - SetUserAsk
						ask := fmt.Sprintf("Вопрос от воркера %d", workerId)
						if endpoint.SetUserAsk(dialogId, respId, ask, 50000) {
							metrics.SetUserAskOps.Add(1)
							metrics.SuccessfulOps.Add(1)
						} else {
							metrics.FailedOps.Add(1)
						}

					case operation < 90: // 20% - GetUserAsk
						endpoint.GetUserAsk(dialogId, respId)
						metrics.GetUserAskOps.Add(1)
						metrics.SuccessfulOps.Add(1)

					default: // 10% - FlushAllBatches
						endpoint.FlushAllBatches()
						metrics.FlushOps.Add(1)
						metrics.SuccessfulOps.Add(1)
					}

					opDuration := time.Since(start)
					metrics.UpdateResponseTime(opDuration)
					metrics.TotalOperations.Add(1)

					// Небольшая случайная задержка для реалистичности
					time.Sleep(time.Duration(rng.Intn(5)) * time.Millisecond)
				}
			}
		}(i)
	}

	wg.Wait()
	endpoint.FlushAllBatches()

	metrics.PrintReport(t)

	dbCalls := mockDB.GetSaveDialogCalls()
	t.Logf("\nВызовов SaveDialog в БД: %d", dbCalls)
	t.Logf("SaveDialog операций: %d", metrics.SaveDialogOps.Load())
	if metrics.SaveDialogOps.Load() > 0 {
		t.Logf("Коэффициент батчинга: %.2fx", float64(metrics.SaveDialogOps.Load())/float64(dbCalls))
	}
}

// TestEndpointBatchingEfficiency тестирует эффективность буферизации
func TestEndpointBatchingEfficiency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockDB := NewMockDB()
	endpoint := New(ctx, mockDB)
	endpoint.batchSize = 10 // Устанавливаем размер батча для теста
	defer endpoint.Shutdown()

	threadId := uint64(1)
	numMessages := 100

	t.Logf("Тест буферизации сообщений: размер батча=%d, сообщений=%d", endpoint.batchSize, numMessages)

	for i := 0; i < numMessages; i++ {
		resp := model.AssistResponse{
			Message: fmt.Sprintf("Сообщение %d", i),
		}
		endpoint.SaveDialog(comdb.AI, threadId, &resp)
	}

	time.Sleep(100 * time.Millisecond)
	endpoint.FlushAllBatches()
	time.Sleep(100 * time.Millisecond)

	dbCalls := mockDB.GetSaveDialogCalls()
	expectedFlushes := (numMessages + endpoint.batchSize - 1) / endpoint.batchSize

	t.Logf("Сообщений отправлено: %d", numMessages)
	t.Logf("Вызовов БД: %d", dbCalls)
	t.Logf("Ожидаемых flush операций: %d", expectedFlushes)

	// Примечание: текущая реализация endpoint сохраняет каждое сообщение отдельно,
	// но группирует их в батчи для отложенного сохранения
	if dbCalls == int64(numMessages) {
		t.Logf("✓ Все сообщения сохранены: %d вызовов БД для %d сообщений", dbCalls, numMessages)
	} else {
		t.Errorf("✗ Ожидалось %d вызовов БД, получено %d", numMessages, dbCalls)
	}
}

// TestEndpointLimitExceeded тестирует превышение лимита символов
func TestEndpointLimitExceeded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockDB := NewMockDB()
	endpoint := New(ctx, mockDB)
	defer endpoint.Shutdown()

	dialogId := uint64(1)
	respId := uint64(1)
	limit := uint32(100)

	// Добавляем сообщения до лимита
	longMessage := string(make([]byte, 90)) // 90 символов
	success := endpoint.SetUserAsk(dialogId, respId, longMessage, limit)
	if !success {
		t.Error("Первое сообщение должно быть принято")
	}

	// Пытаемся добавить сообщение, которое превысит лимит
	anotherMessage := string(make([]byte, 20)) // 20 символов
	success = endpoint.SetUserAsk(dialogId, respId, anotherMessage, limit)
	if success {
		t.Error("Второе сообщение должно быть отклонено из-за превышения лимита")
	}

	// Проверяем, что первое сообщение сохранилось
	asks := endpoint.GetUserAsk(dialogId, respId)
	if len(asks) != 1 {
		t.Errorf("Ожидалось 1 сообщение, получено %d", len(asks))
	}
}

// BenchmarkSaveDialog бенчмарк для SaveDialog
func BenchmarkSaveDialog(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockDB := NewMockDB()
	mockDB.saveDialogLatency = 0 // Отключаем задержку для чистого бенчмарка
	endpoint := New(ctx, mockDB)
	defer endpoint.Shutdown()

	resp := model.AssistResponse{
		Message: "Тестовое сообщение",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		endpoint.SaveDialog(comdb.AI, uint64(i%10), &resp)
	}
}

// BenchmarkSetUserAsk бенчмарк для SetUserAsk
func BenchmarkSetUserAsk(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockDB := NewMockDB()
	endpoint := New(ctx, mockDB)
	defer endpoint.Shutdown()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		endpoint.SetUserAsk(uint64(i%10), uint64(i%100), "Тестовый вопрос", 10000)
	}
}

// BenchmarkGetUserAsk бенчмарк для GetUserAsk
func BenchmarkGetUserAsk(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockDB := NewMockDB()
	endpoint := New(ctx, mockDB)
	defer endpoint.Shutdown()

	// Предварительно заполняем данные
	for i := 0; i < 100; i++ {
		endpoint.SetUserAsk(uint64(i%10), uint64(i%100), "Тестовый вопрос", 10000)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		endpoint.GetUserAsk(uint64(i%10), uint64(i%100))
	}
}

// BenchmarkConcurrentMixedOperations бенчмарк параллельных операций
func BenchmarkConcurrentMixedOperations(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockDB := NewMockDB()
	mockDB.saveDialogLatency = 0
	endpoint := New(ctx, mockDB)
	defer endpoint.Shutdown()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			switch i % 3 {
			case 0:
				resp := model.AssistResponse{Message: "Сообщение"}
				endpoint.SaveDialog(comdb.AI, uint64(i%10), &resp)
			case 1:
				endpoint.SetUserAsk(uint64(i%10), uint64(i%100), "?", 1000000)
			case 2:
				endpoint.GetUserAsk(uint64(i%10), uint64(i%100))
			}
			i++
		}
	})
}
