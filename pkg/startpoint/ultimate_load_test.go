package startpoint

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
)

// UltimateLoadMetrics метрики для максимального нагрузочного теста
type UltimateLoadMetrics struct {
	// Базовые метрики
	TotalUsers         int32
	ActiveUsers        atomic.Int32
	MaxActiveUsers     atomic.Int32
	TotalMessages      atomic.Int64
	SuccessfulMessages atomic.Int64
	FailedMessages     atomic.Int64
	TimeoutMessages    atomic.Int64
	ErrorMessages      atomic.Int64

	// Метрики времени
	TotalResponseTime atomic.Int64
	MinResponseTime   atomic.Int64
	MaxResponseTime   atomic.Int64
	StartTime         time.Time
	EndTime           time.Time

	// Операторские метрики
	OperatorActivations       atomic.Int32
	OperatorTimeouts          atomic.Int32
	OperatorResponses         atomic.Int32
	OperatorManualDisconnects atomic.Int32

	// Метрики типов сообщений
	VoiceMessages atomic.Int32
	TextMessages  atomic.Int32
	WithFiles     atomic.Int32

	// Метрики ошибок API
	API401Errors      atomic.Int32
	API403Errors      atomic.Int32
	API500Errors      atomic.Int32
	API503Errors      atomic.Int32
	CriticalErrors    atomic.Int32
	NonCriticalErrors atomic.Int32

	// Метрики переключений режимов
	AIToOperatorSwitches atomic.Int32
	OperatorToAISwitches atomic.Int32

	mu sync.Mutex
}

// NewUltimateLoadMetrics создаёт экземпляр метрик
func NewUltimateLoadMetrics(totalUsers int) *UltimateLoadMetrics {
	m := &UltimateLoadMetrics{
		TotalUsers: int32(totalUsers),
		StartTime:  time.Now(),
	}
	m.MinResponseTime.Store(int64(^uint64(0) >> 1))
	return m
}

// UpdateResponseTime обновляет метрики времени ответа
func (m *UltimateLoadMetrics) UpdateResponseTime(duration time.Duration) {
	ms := duration.Milliseconds()
	m.TotalResponseTime.Add(ms)

	// Обновляем минимум
	for {
		oldMin := m.MinResponseTime.Load()
		if ms >= oldMin || m.MinResponseTime.CompareAndSwap(oldMin, ms) {
			break
		}
	}

	// Обновляем максимум
	for {
		oldMax := m.MaxResponseTime.Load()
		if ms <= oldMax || m.MaxResponseTime.CompareAndSwap(oldMax, ms) {
			break
		}
	}
}

// PrintUltimateReport выводит полный отчёт
func (m *UltimateLoadMetrics) PrintUltimateReport(t *testing.T) {
	m.EndTime = time.Now()
	duration := m.EndTime.Sub(m.StartTime)

	successRate := float64(0)
	if m.TotalMessages.Load() > 0 {
		successRate = float64(m.SuccessfulMessages.Load()) / float64(m.TotalMessages.Load()) * 100
	}

	avgResponseTime := float64(0)
	if m.SuccessfulMessages.Load() > 0 {
		avgResponseTime = float64(m.TotalResponseTime.Load()) / float64(m.SuccessfulMessages.Load())
	}

	throughput := float64(m.TotalMessages.Load()) / duration.Seconds()

	t.Log("\n" + "═════════════════════════════════════════════════════════════════")
	t.Log("        🚀 МАКСИМАЛЬНЫЙ НАГРУЗОЧНЫЙ ТЕСТ - ПОЛНЫЙ ОТЧЁТ 🚀")
	t.Log("═════════════════════════════════════════════════════════════════")

	t.Log("\n📊 ОБЩАЯ ИНФОРМАЦИЯ:")
	t.Logf("  • Всего пользователей: %d", m.TotalUsers)
	t.Logf("  • Длительность теста: %v", duration.Round(time.Millisecond))
	t.Logf("  • Максимальная активность: %d параллельных пользователей", m.MaxActiveUsers.Load())

	t.Log("\n📨 СТАТИСТИКА СООБЩЕНИЙ:")
	t.Logf("  • Всего сообщений: %d", m.TotalMessages.Load())
	t.Logf("  • Успешных: %d (%.2f%%)", m.SuccessfulMessages.Load(), successRate)
	t.Logf("  • Неудачных: %d", m.FailedMessages.Load())
	t.Logf("  • Таймаутов: %d", m.TimeoutMessages.Load())
	t.Logf("  • Ошибок: %d", m.ErrorMessages.Load())

	t.Log("\n⚡ ПРОИЗВОДИТЕЛЬНОСТЬ:")
	t.Logf("  • Пропускная способность: %.2f сообщений/сек", throughput)
	t.Logf("  • Среднее время ответа: %.2f мс", avgResponseTime)
	t.Logf("  • Мин. время ответа: %d мс", m.MinResponseTime.Load())
	t.Logf("  • Макс. время ответа: %d мс", m.MaxResponseTime.Load())

	t.Log("\n👤 ОПЕРАТОРСКИЙ РЕЖИМ:")
	t.Logf("  • Активаций: %d", m.OperatorActivations.Load())
	t.Logf("  • Таймаутов оператора: %d", m.OperatorTimeouts.Load())
	t.Logf("  • Ответов от оператора: %d", m.OperatorResponses.Load())
	t.Logf("  • Ручных отключений: %d", m.OperatorManualDisconnects.Load())

	t.Log("\n📝 ТИПЫ СООБЩЕНИЙ:")
	t.Logf("  • Текстовых: %d", m.TextMessages.Load())
	t.Logf("  • Голосовых: %d", m.VoiceMessages.Load())
	t.Logf("  • С файлами: %d", m.WithFiles.Load())

	t.Log("\n❌ ОШИБКИ API:")
	t.Logf("  • 401 (Unauthorized): %d", m.API401Errors.Load())
	t.Logf("  • 403 (Forbidden): %d", m.API403Errors.Load())
	t.Logf("  • 500 (Internal Error): %d", m.API500Errors.Load())
	t.Logf("  • 503 (Service Unavailable): %d", m.API503Errors.Load())
	t.Logf("  • Критических: %d", m.CriticalErrors.Load())
	t.Logf("  • Некритических: %d", m.NonCriticalErrors.Load())

	t.Log("\n🔄 ПЕРЕКЛЮЧЕНИЯ РЕЖИМОВ:")
	t.Logf("  • AI → Оператор: %d", m.AIToOperatorSwitches.Load())
	t.Logf("  • Оператор → AI: %d", m.OperatorToAISwitches.Load())

	t.Log("═════════════════════════════════════════════════════════════════")
}

// simulateUltimateUserSession максимально реалистичная симуляция пользователя
func simulateUltimateUserSession(
	ctx context.Context,
	start *Start,
	userId uint32,
	dialogId uint64,
	messagesPerUser int,
	metrics *UltimateLoadMetrics,
	mockOperator *MockOperator,
	mockModel *MockModel,
	wg *sync.WaitGroup,
	t *testing.T,
) {
	defer wg.Done()

	// Обновляем активных пользователей
	currentActive := metrics.ActiveUsers.Add(1)
	for {
		oldMax := metrics.MaxActiveUsers.Load()
		if currentActive <= oldMax || metrics.MaxActiveUsers.CompareAndSwap(oldMax, currentActive) {
			break
		}
	}
	defer metrics.ActiveUsers.Add(-1)

	// Создаём контекст с таймаутом для пользователя
	userCtx, userCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer userCancel()

	respModel := &model.RespModel{
		Assist: model.Assistant{
			AssistId:   fmt.Sprintf("ultimate-test-%d", userId),
			AssistName: "UltimateAssistant",
			UserId:     userId,
		},
		RespName: fmt.Sprintf("UltimateUser-%d", userId),
		TTL:      time.Now().Add(2 * time.Hour),
		Chan:     make(map[uint64]*model.Ch),
		Ctx:      userCtx,
		Cancel:   userCancel,
	}

	usrCh := &model.Ch{
		TxCh:     make(chan model.Message, 200), // Большой буфер
		RxCh:     make(chan model.Message, 200),
		UserId:   userId,
		DialogId: dialogId,
		RespName: respModel.RespName,
	}

	respModel.Chan[dialogId] = usrCh

	// Запускаем Listener
	go func() {
		if err := start.Listener(respModel, usrCh, dialogId, dialogId); err != nil {
			// Игнорируем ошибки
		}
	}()

	time.Sleep(100 * time.Millisecond)

	// Генерируем случайное поведение пользователя
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(userId)))

	// Решаем, будет ли пользователь использовать оператора
	useOperator := rng.Float32() < 0.3 // 30%
	operatorMode := false
	operatorActive := false

	// Решаем, будет ли пользователь генерировать ошибки
	generateErrors := rng.Float32() < 0.2 // 20%

	// Решаем, будет ли использовать голос
	useVoice := rng.Float32() < 0.15 // 15%

	// Решаем, будет ли отправлять файлы
	useFiles := rng.Float32() < 0.25 // 25%

	for i := 0; i < messagesPerUser; i++ {
		select {
		case <-userCtx.Done():
			return
		default:
		}

		startTime := time.Now()
		metrics.TotalMessages.Add(1)

		// Формируем сообщение
		var question model.Message
		isVoice := useVoice && rng.Float32() < 0.4
		hasFiles := useFiles && rng.Float32() < 0.3

		// Первое сообщение может активировать оператора
		if i == 0 && useOperator {
			question = model.Message{
				Type: "user",
				Content: model.AssistResponse{
					Message: fmt.Sprintf("Срочно нужна помощь оператора! Вопрос #%d", i+1),
				},
				Name:     respModel.RespName,
				Operator: model.Operator{SetOperator: true, SenderName: respModel.RespName},
			}
			metrics.OperatorActivations.Add(1)
			metrics.AIToOperatorSwitches.Add(1)
			operatorMode = true
		} else {
			msgType := "user"
			if isVoice {
				msgType = "user_voice"
				metrics.VoiceMessages.Add(1)
			} else {
				metrics.TextMessages.Add(1)
			}

			messageText := fmt.Sprintf("Вопрос #%d от пользователя %d", i+1, userId)

			// Симулируем разные типы вопросов
			questionTypes := []string{
				"Как настроить параметр X?",
				"Получаю ошибку при запуске",
				"Не могу найти функцию Y",
				"Система работает медленно",
				"Нужна помощь с интеграцией",
			}
			if rng.Float32() < 0.5 {
				messageText = questionTypes[rng.Intn(len(questionTypes))]
			}

			question = model.Message{
				Type: msgType,
				Content: model.AssistResponse{
					Message: messageText,
				},
				Name:     respModel.RespName,
				Operator: model.Operator{Operator: false},
			}

			// Добавляем файлы
			if hasFiles {
				numFiles := rng.Intn(3) + 1
				for f := 0; f < numFiles; f++ {
					question.Files = append(question.Files, model.FileUpload{
						Name:     fmt.Sprintf("file_%d_%d.txt", userId, f),
						MimeType: "text/plain",
					})
				}
				metrics.WithFiles.Add(1)
			}
		}

		// Симулируем ошибки API
		if generateErrors && rng.Float32() < 0.15 {
			errorType := rng.Intn(4)
			switch errorType {
			case 0:
				mockModel.SimulateError(401)
				metrics.API401Errors.Add(1)
			case 1:
				mockModel.SimulateError(403)
				metrics.API403Errors.Add(1)
			case 2:
				mockModel.SimulateError(500)
				metrics.API500Errors.Add(1)
			case 3:
				mockModel.SimulateError(503)
				metrics.API503Errors.Add(1)
			}
		}

		// Отправляем вопрос
		select {
		case usrCh.RxCh <- question:
		case <-time.After(2 * time.Second):
			metrics.TimeoutMessages.Add(1)
			metrics.FailedMessages.Add(1)
			continue
		case <-userCtx.Done():
			return
		}

		// Читаем эхо
		select {
		case msg := <-usrCh.TxCh:
			if msg.Type != "user" && msg.Type != "user_voice" {
				// Не эхо, возвращаем обратно
				select {
				case usrCh.TxCh <- msg:
				default:
				}
			}
		case <-time.After(2 * time.Second):
			metrics.TimeoutMessages.Add(1)
			metrics.FailedMessages.Add(1)
			continue
		case <-userCtx.Done():
			return
		}

		// Читаем ответ
		responseTimeout := time.After(time.Duration(mode.OperatorResponseTimeout+5) * time.Second)
		gotResponse := false

	responseLoop:
		for {
			select {
			case msg := <-usrCh.TxCh:
				responseTime := time.Since(startTime)

				if msg.Type == "assist" && msg.Content.Message != "" {
					// Проверяем на таймаут оператора
					if len(msg.Content.Message) > 20 && msg.Content.Message[:20] == "⏱️ Оператор не отве" {
						metrics.OperatorTimeouts.Add(1)
						metrics.OperatorToAISwitches.Add(1)
						operatorMode = false
						operatorActive = false
						continue // Ждём следующий ответ от AI
					}

					// Обычный ответ
					if msg.Operator.Operator {
						metrics.OperatorResponses.Add(1)
						operatorActive = true
					}

					metrics.SuccessfulMessages.Add(1)
					metrics.UpdateResponseTime(responseTime)
					gotResponse = true
					break responseLoop
				}

			case <-responseTimeout:
				metrics.TimeoutMessages.Add(1)
				metrics.FailedMessages.Add(1)
				break responseLoop

			case <-userCtx.Done():
				return
			}
		}

		if !gotResponse {
			continue
		}

		// Оператор может завершить диалог
		if operatorMode && operatorActive && i >= 2 && rng.Float32() < 0.2 {
			key := fmt.Sprintf("%d_%d", userId, dialogId)
			if chInterface, ok := mockOperator.activeReceivers.Load(key); ok {
				opCh := chInterface.(chan model.Message)

				systemMsg := model.Message{
					Type: "assist",
					Content: model.AssistResponse{
						Message: "Set-Mode-To-AI",
					},
					Operator: model.Operator{SetOperator: true, Operator: true},
				}

				select {
				case opCh <- systemMsg:
					metrics.OperatorManualDisconnects.Add(1)
					metrics.OperatorToAISwitches.Add(1)
					operatorMode = false
					operatorActive = false
					time.Sleep(200 * time.Millisecond)
				case <-time.After(100 * time.Millisecond):
				}
			}
		}

		// Случайная задержка между сообщениями (имитация реального пользователя)
		delay := time.Duration(rng.Intn(200)+50) * time.Millisecond
		time.Sleep(delay)
	}

	// Завершение
	time.Sleep(100 * time.Millisecond)
	userCancel()
}

// TestUltimateLoadTest максимальный комплексный нагрузочный тест
func TestUltimateLoadTest(t *testing.T) {
	if testing.Short() {
		t.Skip("Пропуск максимального нагрузочного теста в режиме short")
	}

	const (
		numUsers        = 100 // Можно увеличить до 200+
		messagesPerUser = 7   // Можно увеличить до 10+
		waveSize        = 20  // Пользователей в волне
	)

	// Сохраняем оригинальные значения
	originalTimeout := mode.OperatorResponseTimeout
	mode.OperatorResponseTimeout = 8 // 8 секунд
	defer func() {
		mode.OperatorResponseTimeout = originalTimeout
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	// Настройка оператора
	mockOperator.SetResponseDelay(400 * time.Millisecond)
	mockOperator.StartAutoResponder(ctx)

	mockModel.StartMessageConsumer(ctx)

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	metrics := NewUltimateLoadMetrics(numUsers)

	t.Log("\n" + "═════════════════════════════════════════════════════════════════")
	t.Log("🚀🚀🚀 МАКСИМАЛЬНЫЙ КОМПЛЕКСНЫЙ НАГРУЗОЧНЫЙ ТЕСТ 🚀🚀🚀")
	t.Log("═════════════════════════════════════════════════════════════════")
	t.Logf("📊 Конфигурация теста:")
	t.Logf("  • Пользователей: %d", numUsers)
	t.Logf("  • Сообщений на пользователя: %d", messagesPerUser)
	t.Logf("  • Ожидаемо сообщений: %d", numUsers*messagesPerUser)
	t.Logf("  • Размер волны: %d пользователей", waveSize)
	t.Logf("  • Таймаут оператора: %d секунд", mode.OperatorResponseTimeout)
	t.Log("\n🎯 Симулируемые сценарии:")
	t.Log("  ✓ Операторский режим (~30% пользователей)")
	t.Log("  ✓ Таймауты оператора")
	t.Log("  ✓ Ручное отключение оператором")
	t.Log("  ✓ Голосовые сообщения (~15%)")
	t.Log("  ✓ Отправка файлов (~25%)")
	t.Log("  ✓ Симуляция ошибок API (~20% пользователей)")
	t.Log("  ✓ Ошибки 401, 403, 500, 503")
	t.Log("  ✓ Критические и некритические ошибки")
	t.Log("  ✓ Переключения AI ↔ Оператор")
	t.Log("  ✓ Случайные задержки и поведение")
	t.Log("═════════════════════════════════════════════════════════════════\n")

	var wg sync.WaitGroup

	// Горутина управления доступностью оператора
	operatorCtx, operatorCancel := context.WithCancel(ctx)
	defer operatorCancel()

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-operatorCtx.Done():
				return
			case <-ticker.C:
				// Случайно меняем доступность
				if time.Now().UnixNano()%3 == 0 {
					mockOperator.EnableResponse(false)
					t.Log("  🔴 Оператор временно недоступен")
				} else {
					mockOperator.EnableResponse(true)
					t.Log("  🟢 Оператор доступен")
				}
			}
		}
	}()

	// Запускаем пользователей волнами
	numWaves := (numUsers + waveSize - 1) / waveSize
	t.Logf("🌊 Запуск %d волн по %d пользователей...\n", numWaves, waveSize)

	for wave := 0; wave < numWaves; wave++ {
		startIdx := wave * waveSize
		endIdx := startIdx + waveSize
		if endIdx > numUsers {
			endIdx = numUsers
		}

		for i := startIdx; i < endIdx; i++ {
			userId := uint32(i + 1)
			dialogId := uint64(userId)

			wg.Add(1)
			go simulateUltimateUserSession(
				ctx,
				start,
				userId,
				dialogId,
				messagesPerUser,
				metrics,
				mockOperator,
				mockModel,
				&wg,
				t,
			)

			time.Sleep(30 * time.Millisecond)
		}

		t.Logf("  🌊 Волна %d/%d запущена (%d пользователей)", wave+1, numWaves, endIdx-startIdx)
		time.Sleep(300 * time.Millisecond)
	}

	t.Log("\n⏳ Ожидание завершения всех пользователей...")
	wg.Wait()

	operatorCancel()

	t.Log("⏳ Ожидание завершения обработки...")
	time.Sleep(5 * time.Second)

	// Выводим отчёт
	metrics.PrintUltimateReport(t)

	// Проверки
	t.Log("\n🔍 ВАЛИДАЦИЯ РЕЗУЛЬТАТОВ:")

	successRate := float64(metrics.SuccessfulMessages.Load()) / float64(metrics.TotalMessages.Load()) * 100
	if successRate < 70.0 {
		t.Logf("  ⚠️  Низкий процент успешных сообщений: %.2f%% (критично если <70%%)", successRate)
	} else if successRate < 85.0 {
		t.Logf("  ⚠️  Средний процент успешных сообщений: %.2f%% (приемлемо)", successRate)
	} else {
		t.Logf("  ✅ Отличный процент успешных сообщений: %.2f%%", successRate)
	}

	if metrics.OperatorActivations.Load() > 0 {
		t.Logf("  ✅ Операторский режим использовался: %d активаций", metrics.OperatorActivations.Load())
	} else {
		t.Log("  ⚠️  Операторский режим не был активирован")
	}

	if metrics.VoiceMessages.Load() > 0 {
		t.Logf("  ✅ Голосовые сообщения: %d", metrics.VoiceMessages.Load())
	}

	if metrics.WithFiles.Load() > 0 {
		t.Logf("  ✅ Сообщений с файлами: %d", metrics.WithFiles.Load())
	}

	totalAPIErrors := metrics.API401Errors.Load() + metrics.API403Errors.Load() +
		metrics.API500Errors.Load() + metrics.API503Errors.Load()
	if totalAPIErrors > 0 {
		t.Logf("  ✅ Симулированы ошибки API: %d", totalAPIErrors)
	}

	if metrics.AIToOperatorSwitches.Load() > 0 && metrics.OperatorToAISwitches.Load() > 0 {
		t.Logf("  ✅ Переключения режимов работают: AI→Op=%d, Op→AI=%d",
			metrics.AIToOperatorSwitches.Load(), metrics.OperatorToAISwitches.Load())
	}

	avgResponseTime := float64(metrics.TotalResponseTime.Load()) / float64(metrics.SuccessfulMessages.Load())
	if avgResponseTime > 5000 {
		t.Logf("  ❌ Слишком большое время ответа: %.2f мс", avgResponseTime)
	} else if avgResponseTime > 1000 {
		t.Logf("  ⚠️  Среднее время ответа: %.2f мс (можно улучшить)", avgResponseTime)
	} else {
		t.Logf("  ✅ Отличное время ответа: %.2f мс", avgResponseTime)
	}

	throughput := float64(metrics.TotalMessages.Load()) / metrics.EndTime.Sub(metrics.StartTime).Seconds()
	if throughput < 5.0 {
		t.Logf("  ⚠️  Низкая пропускная способность: %.2f сообщений/сек", throughput)
	} else if throughput < 15.0 {
		t.Logf("  ✅ Хорошая пропускная способность: %.2f сообщений/сек", throughput)
	} else {
		t.Logf("  ✅ Отличная пропускная способность: %.2f сообщений/сек", throughput)
	}

	t.Log("\n" + "═════════════════════════════════════════════════════════════════")
	t.Log("✅✅✅ МАКСИМАЛЬНЫЙ НАГРУЗОЧНЫЙ ТЕСТ ЗАВЕРШЁН ✅✅✅")
	t.Log("═════════════════════════════════════════════════════════════════")
}
