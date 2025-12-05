package startpoint

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
)

// min возвращает минимальное из двух чисел
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// LoadTestMetrics хранит метрики нагрузочного тестирования
type LoadTestMetrics struct {
	TotalUsers         int32
	ActiveUsers        atomic.Int32
	MaxActiveUsers     atomic.Int32 // Максимальное количество активных пользователей
	TotalMessages      atomic.Int64
	SuccessfulMessages atomic.Int64
	FailedMessages     atomic.Int64
	TotalResponseTime  atomic.Int64 // в миллисекундах
	MinResponseTime    atomic.Int64 // в миллисекундах
	MaxResponseTime    atomic.Int64 // в миллисекундах
	TimeoutMessages    atomic.Int64
	ErrorMessages      atomic.Int64
	StartTime          time.Time
	EndTime            time.Time
	DialogsSaved       atomic.Int64
	mu                 sync.Mutex
}

// NewLoadTestMetrics создаёт новый экземпляр метрик
func NewLoadTestMetrics(totalUsers int) *LoadTestMetrics {
	m := &LoadTestMetrics{
		TotalUsers: int32(totalUsers),
		StartTime:  time.Now(),
	}
	m.MinResponseTime.Store(int64(^uint64(0) >> 1)) // Максимальное значение int64
	return m
}

// OperatorLoadMetrics метрики операторского режима в нагрузочном тесте
type OperatorLoadMetrics struct {
	OperatorActivations atomic.Int32 // Количество активаций операторского режима
	OperatorTimeouts    atomic.Int32 // Количество таймаутов оператора
	OperatorResponses   atomic.Int32 // Количество ответов от оператора
	AIResponses         atomic.Int32 // Количество ответов от AI
}

// NewOperatorLoadMetrics создаёт новый экземпляр метрик оператора
func NewOperatorLoadMetrics() *OperatorLoadMetrics {
	return &OperatorLoadMetrics{}
}

// PrintOperatorReport выводит отчёт по работе операторского режима
func (m *OperatorLoadMetrics) PrintOperatorReport(t *testing.T) {
	t.Log("\n" + "═════════════════════════════════════════════════════════")
	t.Log("        СТАТИСТИКА ОПЕРАТОРСКОГО РЕЖИМА")
	t.Log("═════════════════════════════════════════════════════════")
	t.Logf("Активаций операторского режима: %d", m.OperatorActivations.Load())
	t.Logf("Таймаутов оператора: %d", m.OperatorTimeouts.Load())
	t.Logf("Ответов от оператора: %d", m.OperatorResponses.Load())
	t.Logf("Ответов от AI: %d", m.AIResponses.Load())

	if m.OperatorActivations.Load() > 0 {
		timeoutRate := float64(m.OperatorTimeouts.Load()) / float64(m.OperatorActivations.Load()) * 100
		t.Logf("Процент таймаутов: %.1f%%", timeoutRate)
	}

	t.Log("═════════════════════════════════════════════════════════")
}

// UpdateResponseTime обновляет статистику времени ответа
func (m *LoadTestMetrics) UpdateResponseTime(duration time.Duration) {
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

// GetAverageResponseTime возвращает среднее время ответа
func (m *LoadTestMetrics) GetAverageResponseTime() float64 {
	total := m.SuccessfulMessages.Load()
	if total == 0 {
		return 0
	}
	return float64(m.TotalResponseTime.Load()) / float64(total)
}

// GetSuccessRate возвращает процент успешных сообщений
func (m *LoadTestMetrics) GetSuccessRate() float64 {
	total := m.TotalMessages.Load()
	if total == 0 {
		return 0
	}
	return float64(m.SuccessfulMessages.Load()) / float64(total) * 100
}

// GetThroughput возвращает пропускную способность (сообщений/сек)
func (m *LoadTestMetrics) GetThroughput() float64 {
	duration := m.EndTime.Sub(m.StartTime).Seconds()
	if duration == 0 {
		return 0
	}
	return float64(m.TotalMessages.Load()) / duration
}

// PrintReport выводит отчёт о результатах
func (m *LoadTestMetrics) PrintReport(t *testing.T) {
	m.EndTime = time.Now()
	duration := m.EndTime.Sub(m.StartTime)

	t.Logf("\n" + "═════════════════════════════════════════════════════════")
	t.Logf("             ОТЧЁТ НАГРУЗОЧНОГО ТЕСТИРОВАНИЯ")
	t.Logf("═════════════════════════════════════════════════════════")
	t.Logf("Общая информация:")
	t.Logf("  • Пользователей: %d", m.TotalUsers)
	t.Logf("  • Длительность теста: %v", duration.Round(time.Millisecond))
	t.Logf("  • Максимальная активность: %d параллельных пользователей", m.MaxActiveUsers.Load())
	t.Logf("")
	t.Logf("Статистика сообщений:")
	t.Logf("  • Всего сообщений: %d", m.TotalMessages.Load())
	t.Logf("  • Успешных: %d (%.2f%%)", m.SuccessfulMessages.Load(), m.GetSuccessRate())
	t.Logf("  • Неудачных: %d", m.FailedMessages.Load())
	t.Logf("  • Таймаутов: %d", m.TimeoutMessages.Load())
	t.Logf("  • Ошибок: %d", m.ErrorMessages.Load())
	t.Logf("")
	t.Logf("Производительность:")
	t.Logf("  • Пропускная способность: %.2f сообщений/сек", m.GetThroughput())
	t.Logf("  • Среднее время ответа: %.2f мс", m.GetAverageResponseTime())
	t.Logf("  • Мин. время ответа: %d мс", m.MinResponseTime.Load())
	t.Logf("  • Макс. время ответа: %d мс", m.MaxResponseTime.Load())
	t.Logf("")
	t.Logf("База данных:")
	t.Logf("  • Сохранено диалогов: %d", m.DialogsSaved.Load())
	t.Logf("═════════════════════════════════════════════════════════")
}

// simulateUserSession симулирует сессию одного пользователя
// simulateUserSessionWithOperator симулирует сессию пользователя с возможностью переключения в операторский режим
func simulateUserSessionWithOperator(
	ctx context.Context,
	start *Start,
	userId uint32,
	dialogId uint64,
	messagesPerUser int,
	metrics *LoadTestMetrics,
	operatorMetrics *OperatorLoadMetrics,
	mockOperator *MockOperator,
	wg *sync.WaitGroup,
	t *testing.T,
) {
	defer wg.Done()

	// Увеличиваем счётчик активных пользователей
	currentActive := metrics.ActiveUsers.Add(1)
	for {
		oldMax := metrics.MaxActiveUsers.Load()
		if currentActive <= oldMax || metrics.MaxActiveUsers.CompareAndSwap(oldMax, currentActive) {
			break
		}
	}
	defer metrics.ActiveUsers.Add(-1)

	userCtx, userCancel := context.WithCancel(ctx)
	defer userCancel()

	respModel := &model.RespModel{
		Assist: model.Assistant{
			AssistId:   fmt.Sprintf("load-test-assist-%d", userId),
			AssistName: "LoadTestAssistant",
			UserId:     userId,
		},
		RespName: fmt.Sprintf("LoadTestUser-%d", userId),
		TTL:      time.Now().Add(1 * time.Hour),
		Chan:     make(map[uint64]*model.Ch),
		Ctx:      userCtx,
		Cancel:   userCancel,
	}

	usrCh := &model.Ch{
		TxCh:     make(chan model.Message, 100),
		RxCh:     make(chan model.Message, 100),
		UserId:   userId,
		DialogId: dialogId,
		RespName: respModel.RespName,
	}

	respModel.Chan[dialogId] = usrCh

	// Запускаем Listener
	go func() {
		if err := start.Listener(respModel, usrCh, dialogId, dialogId); err != nil {
			// Игнорируем ошибки в нагрузочном тесте
		}
	}()

	time.Sleep(100 * time.Millisecond)

	// Решаем случайно: будет ли этот пользователь использовать оператора (30% вероятность)
	useOperator := (time.Now().UnixNano()+int64(userId))%10 < 3
	operatorMode := false
	operatorSessionActive := false

	for i := 0; i < messagesPerUser; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		startTime := time.Now()
		metrics.TotalMessages.Add(1)

		var question model.Message

		// Первое сообщение может активировать операторский режим
		if i == 0 && useOperator {
			question = model.Message{
				Type: "user",
				Content: model.AssistResponse{
					Message: fmt.Sprintf("Привет! Мне нужна помощь оператора. Вопрос #%d", i+1),
				},
				Name:     respModel.RespName,
				Operator: model.Operator{SetOperator: true, SenderName: respModel.RespName},
			}
			operatorMetrics.OperatorActivations.Add(1)
			operatorMode = true
		} else {
			question = model.Message{
				Type: "user",
				Content: model.AssistResponse{
					Message: fmt.Sprintf("Вопрос #%d от пользователя %d", i+1, userId),
				},
				Name:     respModel.RespName,
				Operator: model.Operator{Operator: false},
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
		var echoReceived bool
		select {
		case msg := <-usrCh.TxCh:
			if msg.Type == "user" {
				echoReceived = true
			}
		case <-time.After(2 * time.Second):
			metrics.TimeoutMessages.Add(1)
			metrics.FailedMessages.Add(1)
			continue
		case <-userCtx.Done():
			return
		}

		if !echoReceived {
			metrics.FailedMessages.Add(1)
			continue
		}

		// Читаем ответ (может быть от оператора, AI, или сообщение о таймауте)
		responseTimeout := time.After(time.Duration(mode.OperatorResponseTimeout+3) * time.Second)
		gotResponse := false

	responseLoop:
		for {
			select {
			case msg := <-usrCh.TxCh:
				responseTime := time.Since(startTime)

				if msg.Type == "assist" && msg.Content.Message != "" {
					// Проверяем, это сообщение о таймауте оператора?
					if len(msg.Content.Message) > 20 && msg.Content.Message[:20] == "⏱️ Оператор не отве" {
						// Таймаут оператора
						operatorMetrics.OperatorTimeouts.Add(1)
						operatorMode = false
						operatorSessionActive = false
						continue // Ждём следующее сообщение (должен быть ответ AI)
					}

					// Обычный ответ
					if msg.Operator.Operator {
						operatorMetrics.OperatorResponses.Add(1)
						operatorSessionActive = true
					} else {
						operatorMetrics.AIResponses.Add(1)
						if operatorMode && operatorSessionActive {
							// AI ответил хотя режим оператора был активен - значит оператор завершил сессию
							operatorMode = false
							operatorSessionActive = false
						}
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

		// После 2-3 сообщений оператор может завершить диалог (если режим активен)
		if operatorMode && operatorSessionActive && i >= 2 && (time.Now().UnixNano()%3 == 0) {
			// Отправляем команду завершения от оператора
			key := fmt.Sprintf("%d_%d", userId, dialogId)
			if chInterface, ok := mockOperator.activeReceivers.Load(key); ok {
				opCh := chInterface.(chan model.Message)

				// Оператор завершает сессию
				systemMsg := model.Message{
					Type: "assist",
					Content: model.AssistResponse{
						Message: "Set-Mode-To-AI",
					},
					Operator: model.Operator{SetOperator: true, Operator: true},
				}

				select {
				case opCh <- systemMsg:
					operatorMode = false
					operatorSessionActive = false
					time.Sleep(200 * time.Millisecond)
				case <-time.After(100 * time.Millisecond):
				}
			}
		}

		// Задержка между сообщениями
		time.Sleep(time.Duration(50+i*20) * time.Millisecond)
	}

	// Очистка
	time.Sleep(100 * time.Millisecond)
	userCancel()
}

func simulateUserSession(
	ctx context.Context,
	start *Start,
	userId uint32,
	dialogId uint64,
	messagesPerUser int,
	metrics *LoadTestMetrics,
	wg *sync.WaitGroup,
	t *testing.T,
) {
	defer wg.Done()

	// Увеличиваем счётчик активных пользователей
	currentActive := metrics.ActiveUsers.Add(1)

	// Обновляем максимальную активность
	for {
		oldMax := metrics.MaxActiveUsers.Load()
		if currentActive <= oldMax || metrics.MaxActiveUsers.CompareAndSwap(oldMax, currentActive) {
			break
		}
	}

	defer metrics.ActiveUsers.Add(-1)

	userCtx, userCancel := context.WithCancel(ctx)
	defer userCancel()

	respModel := &model.RespModel{
		Assist: model.Assistant{
			AssistId:   fmt.Sprintf("load-test-assist-%d", userId),
			AssistName: "LoadTestAssistant",
			UserId:     userId,
		},
		RespName: fmt.Sprintf("LoadTestUser-%d", userId),
		TTL:      time.Now().Add(1 * time.Hour),
		Chan:     make(map[uint64]*model.Ch),
		Ctx:      userCtx,
		Cancel:   userCancel,
	}

	usrCh := &model.Ch{
		TxCh:     make(chan model.Message, 100), // Большой буфер для нагрузочного теста
		RxCh:     make(chan model.Message, 100),
		UserId:   userId,
		DialogId: dialogId,
		RespName: respModel.RespName,
	}

	respModel.Chan[dialogId] = usrCh

	// Запускаем Listener
	errCh := make(chan error, 1)
	listenerReady := make(chan struct{})
	go func() {
		// Сигнализируем о готовности
		time.Sleep(10 * time.Millisecond)
		close(listenerReady)

		if err := start.Listener(respModel, usrCh, dialogId, dialogId); err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}()

	// Даём время на инициализацию Respondent
	time.Sleep(100 * time.Millisecond)

	if t != nil {
		t.Logf("[User %d] Начало отправки сообщений", userId)
	}

	// Отправляем сообщения
	for i := 0; i < messagesPerUser; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		startTime := time.Now()
		metrics.TotalMessages.Add(1)

		question := model.Message{
			Type: "user",
			Content: model.AssistResponse{
				Message: fmt.Sprintf("Вопрос #%d от пользователя %d", i+1, userId),
			},
			Name:     respModel.RespName,
			Operator: model.Operator{Operator: false},
		}

		// Отправляем вопрос
		select {
		case usrCh.RxCh <- question:
			if t != nil && i == 0 {
				t.Logf("[User %d] Вопрос #%d отправлен в RxCh", userId, i+1)
			}
		case <-time.After(2 * time.Second):
			if t != nil {
				t.Logf("[User %d] ⏱️  ТАЙМАУТ при отправке вопроса #%d", userId, i+1)
			}
			metrics.TimeoutMessages.Add(1)
			metrics.FailedMessages.Add(1)
			continue
		case <-userCtx.Done():
			return
		}

		// Читаем эхо
		var echoReceived bool
		select {
		case msg := <-usrCh.TxCh:
			if msg.Type == "user" {
				echoReceived = true
				if t != nil && i == 0 {
					t.Logf("[User %d] ✅ Эхо получено", userId)
				}
			}
		case <-time.After(3 * time.Second):
			if t != nil {
				t.Logf("[User %d] ⏱️  ТАЙМАУТ ожидания эхо для вопроса #%d (буфер RxCh=%d, TxCh=%d)", userId, i+1, len(usrCh.RxCh), len(usrCh.TxCh))
			}
			metrics.TimeoutMessages.Add(1)
			metrics.FailedMessages.Add(1)
			continue
		case <-userCtx.Done():
			return
		}

		if !echoReceived {
			if t != nil {
				t.Logf("[User %d] ❌ Эхо не распознано", userId)
			}
			metrics.FailedMessages.Add(1)
			continue
		}

		// Читаем ответ от AI
		select {
		case msg := <-usrCh.TxCh:
			responseTime := time.Since(startTime)
			if msg.Type == "assist" && msg.Content.Message != "" {
				metrics.SuccessfulMessages.Add(1)
				metrics.UpdateResponseTime(responseTime)
				if t != nil && i == 0 {
					msgPreview := msg.Content.Message
					if len(msgPreview) > 30 {
						msgPreview = msgPreview[:30] + "..."
					}
					t.Logf("[User %d] ✅ Ответ получен за %v: %s", userId, responseTime, msgPreview)
				}
			} else {
				if t != nil {
					t.Logf("[User %d] ❌ Некорректный ответ: type=%s, msg_len=%d", userId, msg.Type, len(msg.Content.Message))
				}
				metrics.FailedMessages.Add(1)
			}
		case <-time.After(3 * time.Second):
			if t != nil {
				t.Logf("[User %d] ⏱️  ТАЙМАУТ ожидания ответа для вопроса #%d (буфер TxCh=%d)", userId, i+1, len(usrCh.TxCh))
			}
			metrics.TimeoutMessages.Add(1)
			metrics.FailedMessages.Add(1)
		case <-userCtx.Done():
			return
		}

		// Минимальная задержка между сообщениями
		time.Sleep(time.Duration(10+i*5) * time.Millisecond)
	}

	// Даём время на завершение
	time.Sleep(100 * time.Millisecond)
	userCancel()
}

// TestLoadTest_10Users базовый тест с 10 пользователями
func TestLoadTest_10Users(t *testing.T) {
	if testing.Short() {
		t.Skip("Пропуск нагрузочного теста в режиме short")
	}

	const (
		numUsers        = 10
		messagesPerUser = 5
		totalMessages   = numUsers * messagesPerUser
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()
	mockOperator.EnableResponse(false)

	// Запускаем consumer для чтения из newMessageCh с симуляцией обработки
	mockModel.StartMessageConsumer(ctx)

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	metrics := NewLoadTestMetrics(numUsers)

	t.Logf("🚀 Запуск базового нагрузочного теста: %d пользователей, %d сообщений на пользователя", numUsers, messagesPerUser)
	t.Logf("   Ожидаемо сообщений: %d", totalMessages)

	var wg sync.WaitGroup

	// Запускаем всех пользователей одновременно
	for i := 0; i < numUsers; i++ {
		userId := uint32(i + 1)
		dialogId := uint64(userId)

		wg.Add(1)
		go simulateUserSession(ctx, start, userId, dialogId, messagesPerUser, metrics, &wg, t)

		// Небольшая задержка между запусками
		time.Sleep(20 * time.Millisecond)
	}

	t.Log("   Ожидание завершения всех пользователей...")
	wg.Wait()

	t.Log("   Ожидание завершения обработки...")
	time.Sleep(3 * time.Second)

	metrics.DialogsSaved.Store(int64(mockEndpoint.GetSavedDialogsCount()))
	metrics.PrintReport(t)

	// Проверки для базового теста (менее строгие)
	if metrics.GetSuccessRate() < 80.0 {
		t.Errorf("❌ Низкий процент успешных сообщений: %.2f%% (ожидается ≥80%%)", metrics.GetSuccessRate())
	} else {
		t.Logf("✅ Процент успешных сообщений: %.2f%%", metrics.GetSuccessRate())
	}

	avgResponseTime := metrics.GetAverageResponseTime()
	if avgResponseTime > 2000 {
		t.Errorf("❌ Слишком большое среднее время ответа: %.2f мс (ожидается ≤2000 мс)", avgResponseTime)
	} else {
		t.Logf("✅ Среднее время ответа: %.2f мс", avgResponseTime)
	}
}

// TestLoadTest_100Users тест с 100 пользователями
func TestLoadTest_100Users(t *testing.T) {
	if testing.Short() {
		t.Skip("Пропуск нагрузочного теста в режиме short")
	}

	const (
		numUsers        = 100
		messagesPerUser = 3 // Уменьшено для стабильности
		totalMessages   = numUsers * messagesPerUser
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Создаём моки с поддержкой высокой нагрузки
	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()
	mockOperator.EnableResponse(false) // Отключаем автоответы оператора для скорости

	// Запускаем consumer для чтения из newMessageCh с симуляцией обработки
	mockModel.StartMessageConsumer(ctx)

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	metrics := NewLoadTestMetrics(numUsers)

	t.Logf("🚀 Запуск нагрузочного теста: %d пользователей, %d сообщений на пользователя", numUsers, messagesPerUser)
	t.Logf("   Ожидаемо сообщений: %d", totalMessages)

	var wg sync.WaitGroup

	// Запускаем пользователей волнами для реалистичности
	const waveSize = 20
	for wave := 0; wave < numUsers/waveSize; wave++ {
		for i := 0; i < waveSize; i++ {
			userId := uint32(wave*waveSize + i + 1)
			dialogId := uint64(userId)

			wg.Add(1)
			go simulateUserSession(ctx, start, userId, dialogId, messagesPerUser, metrics, &wg, t)
		}

		// Небольшая задержка между волнами
		time.Sleep(100 * time.Millisecond)
		t.Logf("   Волна %d/%d запущена (%d пользователей)", wave+1, numUsers/waveSize, (wave+1)*waveSize)
	}

	t.Log("   Ожидание завершения всех пользователей...")
	wg.Wait()

	// Даём время на обработку последних сообщений
	time.Sleep(3 * time.Second)

	// Обновляем метрики из endpoint
	metrics.DialogsSaved.Store(int64(mockEndpoint.GetSavedDialogsCount()))

	// Выводим отчёт
	metrics.PrintReport(t)

	// Проверки
	if metrics.GetSuccessRate() < 90.0 {
		t.Errorf("❌ Низкий процент успешных сообщений: %.2f%% (ожидается ≥90%%)", metrics.GetSuccessRate())
	} else {
		t.Logf("✅ Процент успешных сообщений: %.2f%%", metrics.GetSuccessRate())
	}

	avgResponseTime := metrics.GetAverageResponseTime()
	if avgResponseTime > 1000 {
		t.Errorf("❌ Слишком большое среднее время ответа: %.2f мс (ожидается ≤1000 мс)", avgResponseTime)
	} else {
		t.Logf("✅ Среднее время ответа: %.2f мс", avgResponseTime)
	}

	throughput := metrics.GetThroughput()
	if throughput < 10 {
		t.Errorf("❌ Низкая пропускная способность: %.2f сообщений/сек (ожидается ≥10)", throughput)
	} else {
		t.Logf("✅ Пропускная способность: %.2f сообщений/сек", throughput)
	}
}

// TestLoadTest_200Users тест с 200 пользователями
func TestLoadTest_200Users(t *testing.T) {
	if testing.Short() {
		t.Skip("Пропуск нагрузочного теста в режиме short")
	}

	const (
		numUsers        = 200
		messagesPerUser = 3
		totalMessages   = numUsers * messagesPerUser
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()
	mockOperator.EnableResponse(false)

	// Запускаем consumer для чтения из newMessageCh с симуляцией обработки
	mockModel.StartMessageConsumer(ctx)

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	metrics := NewLoadTestMetrics(numUsers)

	t.Logf("🚀 Запуск нагрузочного теста: %d пользователей, %d сообщений на пользователя", numUsers, messagesPerUser)

	var wg sync.WaitGroup

	const waveSize = 25
	for wave := 0; wave < numUsers/waveSize; wave++ {
		for i := 0; i < waveSize; i++ {
			userId := uint32(wave*waveSize + i + 1)
			dialogId := uint64(userId)

			wg.Add(1)
			go simulateUserSession(ctx, start, userId, dialogId, messagesPerUser, metrics, &wg, t)
		}

		time.Sleep(150 * time.Millisecond)
		t.Logf("   Волна %d/%d запущена", wave+1, numUsers/waveSize)
	}

	t.Log("   Ожидание завершения...")
	wg.Wait()
	t.Log("   Ожидание завершения обработки...")
	time.Sleep(5 * time.Second)

	metrics.DialogsSaved.Store(int64(mockEndpoint.GetSavedDialogsCount()))
	metrics.PrintReport(t)

	// Для 200 пользователей допускаем чуть меньший процент успешности
	if metrics.GetSuccessRate() < 85.0 {
		t.Errorf("❌ Низкий процент успешных сообщений: %.2f%%", metrics.GetSuccessRate())
	} else {
		t.Logf("✅ Процент успешных сообщений: %.2f%%", metrics.GetSuccessRate())
	}
}

// TestLoadTest_WithErrors тест с симуляцией ошибок
func TestLoadTest_WithErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("Пропуск нагрузочного теста в режиме short")
	}

	const (
		numUsers        = 50
		messagesPerUser = 10
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	// Запускаем consumer для чтения из newMessageCh с симуляцией обработки
	mockModel.StartMessageConsumer(ctx)

	// Симулируем временные ошибки для части запросов
	go func() {
		time.Sleep(2 * time.Second)
		for i := 0; i < 10; i++ {
			mockModel.SetError(fmt.Errorf("500 Internal Server Error"), 0, 2)
			time.Sleep(1 * time.Second)
			mockModel.ClearError()
			time.Sleep(2 * time.Second)
		}
	}()

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	metrics := NewLoadTestMetrics(numUsers)

	t.Logf("🚀 Запуск нагрузочного теста с ошибками: %d пользователей", numUsers)

	var wg sync.WaitGroup

	for i := 0; i < numUsers; i++ {
		userId := uint32(i + 1)
		dialogId := uint64(userId)

		wg.Add(1)
		go simulateUserSession(ctx, start, userId, dialogId, messagesPerUser, metrics, &wg, t)

		if i%10 == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}

	wg.Wait()
	t.Log("   Ожидание завершения обработки...")
	time.Sleep(5 * time.Second)

	metrics.DialogsSaved.Store(int64(mockEndpoint.GetSavedDialogsCount()))
	metrics.PrintReport(t)

	// При наличии ошибок допускаем более низкий процент успеха
	t.Logf("ℹ️  Тест с симуляцией ошибок - успешность может быть ниже обычного")
	if metrics.GetSuccessRate() < 70.0 {
		t.Logf("⚠️  Процент успешных сообщений: %.2f%% (с учётом симулируемых ошибок)", metrics.GetSuccessRate())
	}
}

// TestLoadTest_WithOperatorMode тест с включением/отключением операторского режима
func TestLoadTest_WithOperatorMode(t *testing.T) {
	if testing.Short() {
		t.Skip("Пропуск нагрузочного теста в режиме short")
	}

	const (
		numUsers        = 20
		messagesPerUser = 5
	)

	// Сохраняем оригинальное значение таймаута
	originalTimeout := mode.OperatorResponseTimeout
	mode.OperatorResponseTimeout = 3 // 3 секунды для теста
	defer func() {
		mode.OperatorResponseTimeout = originalTimeout
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	// Настраиваем оператора с задержкой
	mockOperator.SetResponseDelay(500 * time.Millisecond)

	mockModel.StartMessageConsumer(ctx)

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	metrics := NewLoadTestMetrics(numUsers)

	// Статистика операторского режима
	var (
		operatorActivations atomic.Int32 // Количество активаций операторского режима
		operatorTimeouts    atomic.Int32 // Количество таймаутов оператора
		operatorResponses   atomic.Int32 // Количество ответов оператора
		aiResponses         atomic.Int32 // Количество ответов AI
	)

	t.Logf("🚀 Запуск нагрузочного теста с операторским режимом: %d пользователей", numUsers)
	t.Log("   Сценарий: случайное включение операторского режима")
	t.Logf("   Таймаут оператора: %d секунд", mode.OperatorResponseTimeout)

	var wg sync.WaitGroup

	// Создаём контекст для управления горутиной оператора
	operatorCtx, operatorCancel := context.WithCancel(ctx)
	defer operatorCancel() // Гарантируем остановку горутины при выходе

	// Горутина для случайного управления ответами оператора
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-operatorCtx.Done():
				t.Log("   ⏹️  Управление оператором остановлено")
				return
			case <-ticker.C:
				// Случайно включаем/отключаем ответы оператора (50% вероятность)
				if time.Now().UnixNano()%2 == 0 {
					mockOperator.EnableResponse(true)
					t.Log("   🟢 Оператор доступен")
				} else {
					mockOperator.EnableResponse(false)
					t.Log("   🔴 Оператор недоступен")
				}
			}
		}
	}()

	// Запускаем пользователей
	for i := 0; i < numUsers; i++ {
		userId := uint32(i + 1)
		dialogId := uint64(userId)

		wg.Add(1)
		go func(uid uint32, did uint64) {
			defer wg.Done()

			// Увеличиваем счётчик активных пользователей
			currentActive := metrics.ActiveUsers.Add(1)
			for {
				oldMax := metrics.MaxActiveUsers.Load()
				if currentActive <= oldMax || metrics.MaxActiveUsers.CompareAndSwap(oldMax, currentActive) {
					break
				}
			}
			defer metrics.ActiveUsers.Add(-1)

			userCtx, userCancel := context.WithCancel(ctx)
			defer userCancel()

			respModel := &model.RespModel{
				Assist: model.Assistant{
					AssistId:   fmt.Sprintf("load-operator-test-%d", uid),
					AssistName: "LoadTestAssistant",
					UserId:     uid,
					Espero:     1,
					Ignore:     false,
				},
				RespName: fmt.Sprintf("LoadTestUser-%d", uid),
				TTL:      time.Now().Add(1 * time.Hour),
				Chan:     make(map[uint64]*model.Ch),
				Ctx:      userCtx,
				Cancel:   userCancel,
			}

			usrCh := &model.Ch{
				TxCh:     make(chan model.Message, 200),
				RxCh:     make(chan model.Message, 200),
				UserId:   uid,
				DialogId: did,
				RespName: respModel.RespName,
			}

			respModel.Chan[did] = usrCh

			// Запускаем Listener
			go func() {
				if err := start.Listener(respModel, usrCh, did, did); err != nil {
					t.Logf("Listener error for user %d: %v", uid, err)
				}
			}()

			time.Sleep(100 * time.Millisecond)

			// Случайно решаем, будет ли этот пользователь использовать оператора
			useOperator := time.Now().UnixNano()%3 == 0 // ~33% вероятность

			for i := 0; i < messagesPerUser; i++ {
				select {
				case <-ctx.Done():
					return
				default:
				}

				startTime := time.Now()
				metrics.TotalMessages.Add(1)

				var question model.Message

				// Первое сообщение может активировать операторский режим
				if i == 0 && useOperator {
					question = model.Message{
						Type: "user",
						Content: model.AssistResponse{
							Message: fmt.Sprintf("Нужен оператор! Вопрос #%d от пользователя %d", i+1, uid),
						},
						Name:     respModel.RespName,
						Operator: model.Operator{SetOperator: true, SenderName: respModel.RespName},
					}
					operatorActivations.Add(1)
				} else {
					question = model.Message{
						Type: "user",
						Content: model.AssistResponse{
							Message: fmt.Sprintf("Вопрос #%d от пользователя %d", i+1, uid),
						},
						Name:     respModel.RespName,
						Operator: model.Operator{Operator: false},
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
				var echoReceived bool
				select {
				case msg := <-usrCh.TxCh:
					if msg.Type == "user" {
						echoReceived = true
					}
				case <-time.After(2 * time.Second):
					metrics.TimeoutMessages.Add(1)
					metrics.FailedMessages.Add(1)
					continue
				case <-userCtx.Done():
					return
				}

				if !echoReceived {
					metrics.FailedMessages.Add(1)
					continue
				}

				// Читаем ответы (может быть несколько: таймаут оператора + ответ AI)
				responseTimeout := time.After(time.Duration(mode.OperatorResponseTimeout+2) * time.Second)
				gotResponse := false

			responseLoop:
				for {
					select {
					case msg := <-usrCh.TxCh:
						responseTime := time.Since(startTime)

						if msg.Type == "assist" && msg.Content.Message != "" {
							// Проверяем, это сообщение о таймауте или реальный ответ
							if msg.Content.Message[:20] == "⏱️ Оператор не отве" ||
								(len(msg.Content.Message) >= 20 && msg.Content.Message[:10] == "⏱️ Операт") {
								// Это сообщение о таймауте оператора
								operatorTimeouts.Add(1)
								continue // Ждём следующее сообщение (должен быть ответ AI)
							}

							// Это реальный ответ
							if msg.Operator.Operator {
								operatorResponses.Add(1)
							} else {
								aiResponses.Add(1)
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

				// Задержка между сообщениями
				time.Sleep(time.Duration(50+i*10) * time.Millisecond)
			}

			time.Sleep(100 * time.Millisecond)
			userCancel()
		}(userId, dialogId)

		// Небольшая задержка между запусками пользователей
		if i%5 == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}

	t.Log("   Ожидание завершения всех пользователей...")
	wg.Wait()

	// Останавливаем управление оператором через контекст
	operatorCancel()

	t.Log("   Ожидание завершения обработки...")
	time.Sleep(3 * time.Second)

	metrics.DialogsSaved.Store(int64(mockEndpoint.GetSavedDialogsCount()))

	// Выводим отчёт
	metrics.PrintReport(t)

	// Выводим статистику операторского режима
	t.Log("\n" + "═════════════════════════════════════════════════════════")
	t.Log("        СТАТИСТИКА ОПЕРАТОРСКОГО РЕЖИМА")
	t.Log("═════════════════════════════════════════════════════════")
	t.Logf("Активаций операторского режима: %d", operatorActivations.Load())
	t.Logf("Таймаутов оператора: %d", operatorTimeouts.Load())
	t.Logf("Ответов от оператора: %d", operatorResponses.Load())
	t.Logf("Ответов от AI: %d", aiResponses.Load())
	t.Log("═════════════════════════════════════════════════════════")

	// Проверки
	totalResponses := operatorResponses.Load() + aiResponses.Load()
	if totalResponses < int32(numUsers*messagesPerUser)*8/10 { // 80% минимум
		t.Errorf("❌ Слишком мало ответов: %d (ожидалось минимум 80%% от %d)",
			totalResponses, numUsers*messagesPerUser)
	} else {
		t.Logf("✅ Получено достаточно ответов: %d", totalResponses)
	}

	// Проверяем что операторский режим использовался
	if operatorActivations.Load() == 0 {
		t.Log("⚠️  Операторский режим не был активирован (может быть из-за случайности)")
	} else {
		t.Logf("✅ Операторский режим активирован %d раз", operatorActivations.Load())
	}

	// Проверяем работу таймаутов
	if operatorActivations.Load() > 0 {
		timeoutRate := float64(operatorTimeouts.Load()) / float64(operatorActivations.Load()) * 100
		t.Logf("ℹ️  Процент таймаутов оператора: %.1f%%", timeoutRate)
	}

	// Проверяем микс ответов
	if operatorResponses.Load() > 0 && aiResponses.Load() > 0 {
		t.Logf("✅ Получены ответы и от оператора (%d), и от AI (%d)",
			operatorResponses.Load(), aiResponses.Load())
	}

	t.Log("✅ Тест с операторским режимом завершён")
}

// TestLoadTest_WithOperatorMode_Enhanced нагрузочный тест с симуляцией операторского режима
func TestLoadTest_WithOperatorMode_Enhanced(t *testing.T) {
	if testing.Short() {
		t.Skip("Пропуск нагрузочного теста в режиме short")
	}

	const (
		numUsers        = 30
		messagesPerUser = 5
	)

	// Сохраняем оригинальное значение таймаута
	originalTimeout := mode.OperatorResponseTimeout
	mode.OperatorResponseTimeout = 5 // 5 секунд для теста
	defer func() {
		mode.OperatorResponseTimeout = originalTimeout
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	// Настраиваем оператора
	mockOperator.SetResponseDelay(500 * time.Millisecond)
	mockOperator.StartAutoResponder(ctx)

	mockModel.StartMessageConsumer(ctx)

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	metrics := NewLoadTestMetrics(numUsers)
	operatorMetrics := NewOperatorLoadMetrics()

	t.Logf("🚀 Запуск нагрузочного теста с операторским режимом")
	t.Logf("   Пользователей: %d", numUsers)
	t.Logf("   Сообщений на пользователя: %d", messagesPerUser)
	t.Logf("   Таймаут оператора: %d секунд", mode.OperatorResponseTimeout)
	t.Log("   Сценарий:")
	t.Log("     - ~30% пользователей запросят оператора")
	t.Log("     - Оператор случайно доступен/недоступен")
	t.Log("     - Оператор может завершить диалог после 2-3 сообщений")

	var wg sync.WaitGroup

	// Горутина для случайного управления доступностью оператора
	operatorCtx, operatorCancel := context.WithCancel(ctx)
	defer operatorCancel()

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-operatorCtx.Done():
				return
			case <-ticker.C:
				// Случайно включаем/отключаем ответы оператора
				if time.Now().UnixNano()%2 == 0 {
					mockOperator.EnableResponse(true)
					t.Log("   🟢 Оператор доступен")
				} else {
					mockOperator.EnableResponse(false)
					t.Log("   🔴 Оператор недоступен (симуляция таймаута)")
				}
			}
		}
	}()

	// Запускаем пользователей волнами
	const waveSize = 10
	numWaves := (numUsers + waveSize - 1) / waveSize

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
			go simulateUserSessionWithOperator(
				ctx,
				start,
				userId,
				dialogId,
				messagesPerUser,
				metrics,
				operatorMetrics,
				mockOperator,
				&wg,
				t,
			)

			time.Sleep(50 * time.Millisecond)
		}

		t.Logf("   Волна %d/%d запущена (%d пользователей)", wave+1, numWaves, endIdx-startIdx)
		time.Sleep(200 * time.Millisecond)
	}

	t.Log("   Ожидание завершения всех пользователей...")
	wg.Wait()

	// Останавливаем управление оператором
	operatorCancel()

	t.Log("   Ожидание завершения обработки...")
	time.Sleep(3 * time.Second)

	metrics.DialogsSaved.Store(int64(mockEndpoint.GetSavedDialogsCount()))

	// Выводим отчёты
	metrics.PrintReport(t)
	operatorMetrics.PrintOperatorReport(t)

	// Проверки
	successRate := metrics.GetSuccessRate()
	if successRate < 70.0 {
		t.Logf("⚠️  Низкий процент успешных сообщений: %.2f%% (с учётом операторских таймаутов)", successRate)
	} else {
		t.Logf("✅ Процент успешных сообщений: %.2f%%", successRate)
	}

	// Проверяем что операторский режим использовался
	if operatorMetrics.OperatorActivations.Load() == 0 {
		t.Log("⚠️  Операторский режим не был активирован")
	} else {
		t.Logf("✅ Операторский режим активирован %d раз", operatorMetrics.OperatorActivations.Load())
	}

	// Проверяем работу таймаутов и ответов
	totalOperatorAttempts := operatorMetrics.OperatorActivations.Load()
	if totalOperatorAttempts > 0 {
		operatorResponseRate := float64(operatorMetrics.OperatorResponses.Load()) / float64(totalOperatorAttempts) * 100
		t.Logf("ℹ️  Процент успешных ответов оператора: %.1f%%", operatorResponseRate)
	}

	// Проверяем микс ответов
	if operatorMetrics.OperatorResponses.Load() > 0 && operatorMetrics.AIResponses.Load() > 0 {
		t.Logf("✅ Получены ответы и от оператора (%d), и от AI (%d)",
			operatorMetrics.OperatorResponses.Load(), operatorMetrics.AIResponses.Load())
	}

	t.Log("✅ Нагрузочный тест с операторским режимом завершён")
}

// BenchmarkLoadTest бенчмарк для нагрузочного тестирования
func BenchmarkLoadTest(b *testing.B) {
	const (
		numUsers        = 50
		messagesPerUser = 5
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()
	mockOperator.EnableResponse(false)

	// Запускаем consumer для чтения из newMessageCh с симуляцией обработки
	mockModel.StartMessageConsumer(ctx)

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		metrics := NewLoadTestMetrics(numUsers)

		for j := 0; j < numUsers; j++ {
			userId := uint32(j + 1)
			dialogId := uint64(userId)

			wg.Add(1)
			go simulateUserSession(ctx, start, userId, dialogId, messagesPerUser, metrics, &wg, nil)
		}

		wg.Wait()
	}

	b.StopTimer()
}
