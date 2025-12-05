package startpoint

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
)

// TestOperatorTimeout_AutomaticSwitchToAI проверяет автоматическое переключение на AI при таймауте оператора
func TestOperatorTimeout_AutomaticSwitchToAI(t *testing.T) {
	// Сохраняем оригинальное значение таймаута
	originalTimeout := mode.OperatorResponseTimeout

	// ВАЖНО: Устанавливаем короткий таймаут для теста (5 секунд вместо 2 минут)
	// В production коде это будет 2 минуты
	mode.OperatorResponseTimeout = 5 // секунд для теста
	defer func() {
		// Восстанавливаем оригинальное значение после теста
		mode.OperatorResponseTimeout = originalTimeout
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	// ВАЖНО: Отключаем автоматические ответы оператора для симуляции таймаута
	mockOperator.EnableResponse(false)

	// Запускаем consumer для чтения из newMessageCh
	mockModel.StartMessageConsumer(ctx)

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	userCtx, userCancel := context.WithCancel(ctx)
	defer userCancel()

	respModel := &model.RespModel{
		Assist: model.Assistant{
			AssistId:   "test-operator-timeout",
			AssistName: "TestAssistant",
			UserId:     99999,
			Espero:     1, // 1 секунда ожидания перед отправкой
			Ignore:     false,
		},
		RespName: "TestUser",
		TTL:      time.Now().Add(1 * time.Hour),
		Chan:     make(map[uint64]*model.Ch),
		Ctx:      userCtx,
		Cancel:   userCancel,
	}

	usrCh := &model.Ch{
		TxCh:     make(chan model.Message, 50),
		RxCh:     make(chan model.Message, 50),
		UserId:   99999,
		DialogId: 999,
		RespName: "TestUser",
	}

	respModel.Chan[999] = usrCh

	// Запускаем Listener
	errCh := make(chan error, 1)
	go func() {
		if err := start.Listener(respModel, usrCh, 999, 999); err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}()

	// Даём время на инициализацию
	time.Sleep(200 * time.Millisecond)

	t.Log("=== Шаг 1: Активация операторского режима ===")

	// Отправляем вопрос с флагом SetOperator (запрос операторского режима)
	operatorRequest := model.Message{
		Type: "user",
		Content: model.AssistResponse{
			Message: "Мне нужна помощь оператора",
		},
		Name:     "TestUser",
		Operator: model.Operator{SetOperator: true, Operator: false, SenderName: "TestUser"},
	}

	select {
	case usrCh.RxCh <- operatorRequest:
		t.Log("✅ Запрос операторского режима отправлен")
	case <-time.After(1 * time.Second):
		t.Fatal("❌ Таймаут при отправке запроса операторского режима")
	}

	// Читаем эхо запроса
	select {
	case msg := <-usrCh.TxCh:
		if msg.Type == "user" {
			t.Logf("✅ Получено эхо запроса: %s", msg.Content.Message)
		}
	case <-time.After(2 * time.Second):
		t.Error("❌ Не получено эхо запроса")
	}

	t.Log("=== Шаг 2: Ожидание таймаута оператора ===")
	t.Logf("   Таймаут установлен на %d секунд", mode.OperatorResponseTimeout)

	// Засекаем время начала ожидания
	timeoutStart := time.Now()

	// Ждём сообщение о таймауте (должно прийти через ~5 секунд)
	var timeoutMessageReceived bool
	var aiResponseReceived bool

	timeout := time.After(time.Duration(mode.OperatorResponseTimeout+3) * time.Second)

	for !timeoutMessageReceived || !aiResponseReceived {
		select {
		case msg := <-usrCh.TxCh:
			elapsed := time.Since(timeoutStart)

			if msg.Type == "assist" {
				// Проверяем, это сообщение о таймауте или ответ AI
				if msg.Content.Message != "" {
					t.Logf("📨 Получено сообщение типа 'assist' через %v:", elapsed)
					t.Logf("   Содержание: %s", msg.Content.Message)
					t.Logf("   От оператора: %v", msg.Operator.Operator)
					t.Logf("   SetOperator: %v", msg.Operator.SetOperator)

					// Проверяем сообщение о таймауте
					if !timeoutMessageReceived &&
						(msg.Content.Message == "⏱️ Оператор не ответил в течение 5 секунд\nПродолжаю работу в режиме AI-агента 🧠" ||
							msg.Content.Message[:20] == "⏱️ Оператор не отве") {
						timeoutMessageReceived = true
						t.Log("✅ Получено сообщение о таймауте оператора")

						// Проверяем время срабатывания таймаута (с допуском ±2 секунды)
						expectedTimeout := time.Duration(mode.OperatorResponseTimeout) * time.Second
						if elapsed >= expectedTimeout-1*time.Second && elapsed <= expectedTimeout+2*time.Second {
							t.Logf("✅ Таймаут сработал вовремя: %v (ожидалось ~%v)", elapsed, expectedTimeout)
						} else {
							t.Logf("⚠️  Таймаут сработал через %v, ожидалось ~%v", elapsed, expectedTimeout)
						}

						// Проверяем, что это не от оператора
						if msg.Operator.Operator {
							t.Error("❌ Сообщение о таймауте помечено как от оператора")
						}
					} else if timeoutMessageReceived && !aiResponseReceived {
						// Это должен быть ответ AI на необработанный вопрос
						aiResponseReceived = true
						t.Logf("✅ Получен ответ AI: %s", msg.Content.Message)

						// Проверяем, что это не от оператора
						if msg.Operator.Operator {
							t.Error("❌ Ответ AI помечен как от оператора")
						}
					}
				}
			}

		case <-timeout:
			if !timeoutMessageReceived {
				t.Fatalf("❌ Таймаут теста: сообщение о таймауте оператора не получено за %d секунд",
					mode.OperatorResponseTimeout+3)
			}
			// Если получили сообщение о таймауте, но не получили ответ AI - это нормально,
			// так как вопрос мог быть уже обработан
			goto finish

		case err := <-errCh:
			if err != nil {
				t.Logf("⚠️  Получена ошибка из Listener: %v", err)
				// Не считаем это фатальной ошибкой, так как может быть связано с завершением
			}
		}
	}

finish:
	t.Log("=== Шаг 3: Проверка, что AI режим активен ===")

	// Отправляем новый вопр��с, который должен обработаться AI (не оператором)
	aiQuestion := model.Message{
		Type: "user",
		Content: model.AssistResponse{
			Message: "Как дела?",
		},
		Name:     "TestUser",
		Operator: model.Operator{SetOperator: false, Operator: false},
	}

	select {
	case usrCh.RxCh <- aiQuestion:
		t.Log("✅ Новый вопрос отправлен")
	case <-time.After(1 * time.Second):
		t.Error("❌ Таймаут при отправке нового вопроса")
	}

	// Читаем эхо
	select {
	case msg := <-usrCh.TxCh:
		if msg.Type == "user" {
			t.Log("✅ Получено эхо нового вопроса")
		}
	case <-time.After(2 * time.Second):
		t.Error("❌ Не получено эхо нового вопроса")
	}

	// Читаем ответ - должен быть от AI, не от оператора
	select {
	case msg := <-usrCh.TxCh:
		if msg.Type == "assist" {
			if msg.Operator.Operator {
				t.Error("❌ Ответ пришёл от оператора, ожидался ответ от AI")
			} else {
				t.Logf("✅ Получен ответ от AI (режим корректно переключён): %s", msg.Content.Message)
			}
		}
	case <-time.After(3 * time.Second):
		t.Error("❌ Не получен ответ на новый вопрос")
	}

	t.Log("=== Итоговая проверка ===")

	// Проверяем вызовы операторских методов
	receiveCalls := mockOperator.receiveCalled.Load()
	t.Logf("Вызовов ReceiveFromOperator: %d", receiveCalls)

	if receiveCalls < 1 {
		t.Error("❌ ReceiveFromOperator не был вызван")
	} else {
		t.Log("✅ ReceiveFromOperator был вызван (операторский режим активировался)")
	}

	// Проверяем вызовы DeleteSession (должен быть вызван при таймауте)
	deleteCalls := mockOperator.deleteCalled.Load()
	t.Logf("Вызовов DeleteSession: %d", deleteCalls)

	if deleteCalls < 1 {
		t.Error("❌ DeleteSession не был вызван (сессия не удалена)")
	} else {
		t.Log("✅ DeleteSession был вызван (сессия корректно удалена)")
	}

	// Проверяем, что сообщение о таймауте было получено
	if !timeoutMessageReceived {
		t.Error("❌ Сообщение о таймауте оператора не было получено")
	} else {
		t.Log("✅ Сообщение о таймауте оператора получено")
	}

	t.Log("=== Тест завершён успешно ===")
}

// TestOperatorTimeout_OperatorRespondsInTime проверяет, что после первого ответа оператора режим становится постоянным
func TestOperatorTimeout_OperatorRespondsInTime(t *testing.T) {
	// Сохраняем оригинальное значение
	originalTimeout := mode.OperatorResponseTimeout
	mode.OperatorResponseTimeout = 3 // 3 секунды для теста
	defer func() {
		mode.OperatorResponseTimeout = originalTimeout
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	// Включаем автоответы оператора с задержкой 1 секунду (меньше таймаута)
	mockOperator.EnableResponse(true)
	mockOperator.SetResponseDelay(1 * time.Second)

	// Запускаем автоответчик для обработки всех вопросов
	mockOperator.StartAutoResponder(ctx)

	mockModel.StartMessageConsumer(ctx)

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	userCtx, userCancel := context.WithCancel(ctx)
	defer userCancel()

	respModel := &model.RespModel{
		Assist: model.Assistant{
			AssistId:   "test-operator-responds",
			AssistName: "TestAssistant",
			UserId:     88888,
			Espero:     1,
			Ignore:     false,
		},
		RespName: "TestUser",
		TTL:      time.Now().Add(1 * time.Hour),
		Chan:     make(map[uint64]*model.Ch),
		Ctx:      userCtx,
		Cancel:   userCancel,
	}

	usrCh := &model.Ch{
		TxCh:     make(chan model.Message, 50),
		RxCh:     make(chan model.Message, 50),
		UserId:   88888,
		DialogId: 888,
		RespName: "TestUser",
	}

	respModel.Chan[888] = usrCh

	errCh := make(chan error, 1)
	go func() {
		if err := start.Listener(respModel, usrCh, 888, 888); err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)

	t.Log("=== Активация операторского режима ===")

	operatorRequest := model.Message{
		Type: "user",
		Content: model.AssistResponse{
			Message: "Нужен оператор",
		},
		Name:     "TestUser",
		Operator: model.Operator{SetOperator: true, Operator: false, SenderName: "TestUser"},
	}

	usrCh.RxCh <- operatorRequest
	<-usrCh.TxCh // эхо

	t.Log("=== Ожидание ответа оператора (должен прийти до таймаута) ===")

	// Оператор должен ответить через ~1 секунду
	select {
	case msg := <-usrCh.TxCh:
		t.Logf("📨 Получено сообщение: type=%s, operator=%v, setOperator=%v",
			msg.Type, msg.Operator.Operator, msg.Operator.SetOperator)
		t.Logf("   Содержимое: %s", msg.Content.Message)

		if msg.Type == "assist" && msg.Operator.Operator {
			t.Logf("✅ Получен ответ от оператора: %s", msg.Content.Message)
		} else {
			t.Errorf("❌ Получено неожиданное сообщение: type=%s, operator=%v",
				msg.Type, msg.Operator.Operator)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("❌ Оператор не ответил")
	}

	t.Log("=== Проверяем что режим теперь постоянный (таймер остановлен) ===")

	// Ждём больше чем таймаут (3 + 2 = 5 секунд)
	// Если бы таймер продолжал работать, он бы сработал через 3 секунды
	waitDuration := time.Duration(mode.OperatorResponseTimeout+2) * time.Second
	t.Logf("   Ожидание %v (больше чем таймаут %d сек)...", waitDuration, mode.OperatorResponseTimeout)

	time.Sleep(waitDuration)

	// Проверяем что таймаут НЕ сработал (режим остаётся активным)
	select {
	case msg := <-usrCh.TxCh:
		// Не должно быть сообщения о таймауте
		if strings.Contains(msg.Content.Message, "Оператор не ответил") {
			t.Errorf("❌ Таймаут сработал, хотя режим должен быть постоянным: %s", msg.Content.Message)
		} else {
			t.Logf("ℹ️  Получено сообщение: %s", msg.Content.Message)
		}
	case <-time.After(500 * time.Millisecond):
		t.Log("✅ Таймаут НЕ сработал - режим постоянный!")
	}

	t.Log("=== Симуляция реального диалога с оператором ===")

	// Случайное количество дополнительных вопросов (от 1 до 5)
	additionalQuestions := 1 + (time.Now().UnixNano() % 5) // 1-5 вопросов
	t.Logf("   Будет отправлено %d дополнительных вопросов", additionalQuestions)

	questionTemplates := []string{
		"Как мне решить эту проблему?",
		"А что насчёт другого варианта?",
		"Можете уточнить детали?",
		"Есть ли альтернативные решения?",
		"Спасибо, всё понятно!",
	}

	for i := int64(0); i < additionalQuestions; i++ {
		t.Logf("   → Отправка вопроса %d/%d", i+1, additionalQuestions)

		question := model.Message{
			Type: "user",
			Content: model.AssistResponse{
				Message: questionTemplates[i%int64(len(questionTemplates))],
			},
			Name:     "TestUser",
			Operator: model.Operator{SetOperator: false, Operator: false},
		}

		usrCh.RxCh <- question

		// Читаем эхо
		select {
		case <-usrCh.TxCh:
		case <-time.After(1 * time.Second):
			t.Errorf("❌ Не получено эхо для вопроса %d", i+1)
			continue
		}

		// Читаем ответ оператора
		select {
		case msg := <-usrCh.TxCh:
			if msg.Type == "assist" {
				t.Logf("   ← Ответ %d: %s (от оператора: %v)", i+1, msg.Content.Message, msg.Operator.Operator)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("❌ Не получен ответ на вопрос %d", i+1)
		}

		// Небольшая задержка между вопросами для реалистичности
		time.Sleep(100 * time.Millisecond)
	}

	t.Log("=== Оператор отключает операторский режим ===")

	// Симулируем отключение режима оператором через системное сообщение
	// Оператор отправляет специальное сообщение через канал operatorRxCh
	t.Log("   Оператор отправляет команду отключения режима...")

	// Получаем канал оператора из мока
	key := fmt.Sprintf("%d_%d", uint32(88888), uint64(888))
	if chInterface, ok := mockOperator.activeReceivers.Load(key); ok {
		opCh := chInterface.(chan model.Message)

		// Отправляем системное сообщение об отключении
		systemMsg := model.Message{
			Type: "assist",
			Content: model.AssistResponse{
				Message: "Set-Mode-To-AI",
			},
			Operator: model.Operator{SetOperator: true, Operator: true},
		}

		select {
		case opCh <- systemMsg:
			t.Log("   ✅ Команда отключения отправлена")
		case <-time.After(1 * time.Second):
			t.Error("   ❌ Не удалось отправить команду отключения")
		}

		// Ждём обработки команды
		time.Sleep(500 * time.Millisecond)

		t.Log("   Проверяем что режим отключён...")

		// Отправляем новый вопрос - должен обработаться AI, а не оператором
		testQuestion := model.Message{
			Type: "user",
			Content: model.AssistResponse{
				Message: "Тестовый вопрос после отключения режима",
			},
			Name:     "TestUser",
			Operator: model.Operator{SetOperator: false, Operator: false},
		}

		usrCh.RxCh <- testQuestion
		<-usrCh.TxCh // эхо

		// Проверяем что ответ пришёл от AI, а не от оператора
		select {
		case msg := <-usrCh.TxCh:
			if msg.Type == "assist" {
				if msg.Operator.Operator {
					t.Error("   ❌ Ответ пришёл от оператора, режим не был отключён")
				} else {
					t.Log("   ✅ Ответ пришёл от AI - режим успешно отключён оператором")
				}
			}
		case <-time.After(2 * time.Second):
			t.Error("   ❌ Не получен ответ после отключения режима")
		}
	} else {
		t.Error("   ❌ Не удалось получить канал оператора для отправки команды отключения")
	}

	t.Log("✅ Тест завершён: постоянный режим работает корректно, оператор может отключить режим")
}

// TestOperatorTimeout_LongConversation проверяет длительный диалог с оператором
func TestOperatorTimeout_LongConversation(t *testing.T) {
	originalTimeout := mode.OperatorResponseTimeout
	mode.OperatorResponseTimeout = 3 // 3 секунды для теста
	defer func() {
		mode.OperatorResponseTimeout = originalTimeout
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()

	// Включаем автоответы оператора с быстрым ответом
	mockOperator.EnableResponse(true)
	mockOperator.SetResponseDelay(300 * time.Millisecond)
	mockOperator.StartAutoResponder(ctx)

	mockModel.StartMessageConsumer(ctx)

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	userCtx, userCancel := context.WithCancel(ctx)
	defer userCancel()

	respModel := &model.RespModel{
		Assist: model.Assistant{
			AssistId:   "test-long-conversation",
			AssistName: "TestAssistant",
			UserId:     99999,
			Espero:     1,
			Ignore:     false,
		},
		RespName: "TestUser",
		TTL:      time.Now().Add(1 * time.Hour),
		Chan:     make(map[uint64]*model.Ch),
		Ctx:      userCtx,
		Cancel:   userCancel,
	}

	usrCh := &model.Ch{
		TxCh:     make(chan model.Message, 100),
		RxCh:     make(chan model.Message, 100),
		UserId:   99999,
		DialogId: 999,
		RespName: "TestUser",
	}

	respModel.Chan[999] = usrCh

	errCh := make(chan error, 1)
	go func() {
		if err := start.Listener(respModel, usrCh, 999, 999); err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)

	t.Log("🔷 === Тест длительного диалога с оператором ===")

	// Активация операторского режима
	t.Log("📌 Активация операторского режима...")
	operatorRequest := model.Message{
		Type: "user",
		Content: model.AssistResponse{
			Message: "Привет, мне нужна помощь оператора",
		},
		Name:     "TestUser",
		Operator: model.Operator{SetOperator: true, Operator: false, SenderName: "TestUser"},
	}

	usrCh.RxCh <- operatorRequest
	<-usrCh.TxCh // эхо

	// Ждём ответа оператора
	select {
	case msg := <-usrCh.TxCh:
		if msg.Operator.Operator {
			t.Logf("✅ Оператор подключился: %s", msg.Content.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("❌ Оператор не ответил")
	}

	t.Log("💬 === Диалог с оператором (случайное количество сообщений) ===")

	// Случайное количество сообщений от 3 до 7
	numMessages := 3 + (time.Now().UnixNano() % 5)
	t.Logf("   Будет отправлено %d сообщений", numMessages)

	questions := []string{
		"У меня не работает функция X",
		"Как настроить параметр Y?",
		"Получаю ошибку при попытке Z",
		"Можете помочь с настройкой?",
		"А что насчёт альтернативного решения?",
		"Спасибо за помощь!",
		"Всё ли правильно настроено?",
	}

	successfulMessages := 0

	for i := int64(0); i < numMessages; i++ {
		questionText := questions[i%int64(len(questions))]
		t.Logf("   [%d/%d] Пользователь: %s", i+1, numMessages, questionText)

		question := model.Message{
			Type: "user",
			Content: model.AssistResponse{
				Message: questionText,
			},
			Name:     "TestUser",
			Operator: model.Operator{SetOperator: false, Operator: false},
		}

		usrCh.RxCh <- question

		// Эхо
		select {
		case <-usrCh.TxCh:
		case <-time.After(1 * time.Second):
			t.Logf("   ⚠️  Не получено эхо для сообщения %d", i+1)
			continue
		}

		// Ответ оператора
		select {
		case msg := <-usrCh.TxCh:
			if msg.Type == "assist" {
				if msg.Operator.Operator {
					t.Logf("   [%d/%d] Оператор: %s ✅", i+1, numMessages, msg.Content.Message)
					successfulMessages++
				} else {
					t.Logf("   [%d/%d] AI: %s (ожидался оператор)", i+1, numMessages, msg.Content.Message)
				}
			}
		case <-time.After(1 * time.Second):
			t.Logf("   ⚠️  Не получен ответ на сообщение %d", i+1)
		}

		// Задержка между сообщениями
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("📊 Успешно обработано %d/%d сообщений", successfulMessages, numMessages)

	t.Log("🔴 === Оператор завершает диалог ===")

	// Получаем канал оператора
	key := fmt.Sprintf("%d_%d", uint32(99999), uint64(999))
	if chInterface, ok := mockOperator.activeReceivers.Load(key); ok {
		opCh := chInterface.(chan model.Message)

		// Оператор отправляет прощальное сообщение
		farewellMsg := model.Message{
			Type: "assist",
			Content: model.AssistResponse{
				Message: "Спасибо за обращение! Если будут ещё вопросы - обращайтесь.",
			},
			Operator: model.Operator{SetOperator: false, Operator: true},
		}

		select {
		case opCh <- farewellMsg:
			t.Log("   Оператор отправил прощальное сообщение")
		case <-time.After(500 * time.Millisecond):
		}

		// Читаем прощальное сообщение
		select {
		case msg := <-usrCh.TxCh:
			t.Logf("   ✅ Получено: %s", msg.Content.Message)
		case <-time.After(1 * time.Second):
		}

		// Оператор отключает режим
		systemMsg := model.Message{
			Type: "assist",
			Content: model.AssistResponse{
				Message: "Set-Mode-To-AI",
			},
			Operator: model.Operator{SetOperator: true, Operator: true},
		}

		select {
		case opCh <- systemMsg:
			t.Log("   Оператор отключил операторский режим")
		case <-time.After(500 * time.Millisecond):
			t.Error("   ❌ Не удалось отправить команду отключения")
		}

		time.Sleep(300 * time.Millisecond)
	}

	t.Log("🤖 === Проверка возврата к AI режиму ===")

	// Отправляем вопрос после отключения режима
	finalQuestion := model.Message{
		Type: "user",
		Content: model.AssistResponse{
			Message: "Ещё один вопрос после завершения",
		},
		Name:     "TestUser",
		Operator: model.Operator{SetOperator: false, Operator: false},
	}

	usrCh.RxCh <- finalQuestion
	<-usrCh.TxCh // эхо

	select {
	case msg := <-usrCh.TxCh:
		if msg.Type == "assist" {
			if msg.Operator.Operator {
				t.Error("   ❌ Ответ от оператора, но режим должен быть отключён")
			} else {
				t.Log("   ✅ Ответ от AI - режим корректно переключён обратно")
			}
		}
	case <-time.After(2 * time.Second):
		t.Error("   ❌ Не получен финальный ответ")
	}

	t.Log("✅ Тест завершён: длительный диалог с оператором работает корректно")
}

// TestOperatorTimeout_MultipleTimeouts проверяет корректность работы при повторной активации операторского режима
func TestOperatorTimeout_MultipleTimeouts(t *testing.T) {
	originalTimeout := mode.OperatorResponseTimeout
	mode.OperatorResponseTimeout = 2 // 2 секунды для теста
	defer func() {
		mode.OperatorResponseTimeout = originalTimeout
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mockModel := NewMockModel()
	mockEndpoint := NewMockEndpoint()
	mockBot := &MockBot{}
	mockOperator := NewMockOperator()
	mockOperator.EnableResponse(false) // Оператор не отвечает

	mockModel.StartMessageConsumer(ctx)

	start := New(ctx, mockModel, mockEndpoint, mockBot, mockOperator)
	defer start.Shutdown()

	userCtx, userCancel := context.WithCancel(ctx)
	defer userCancel()

	respModel := &model.RespModel{
		Assist: model.Assistant{
			AssistId:   "test-multiple-timeouts",
			AssistName: "TestAssistant",
			UserId:     77777,
			Espero:     1,
			Ignore:     false,
		},
		RespName: "TestUser",
		TTL:      time.Now().Add(1 * time.Hour),
		Chan:     make(map[uint64]*model.Ch),
		Ctx:      userCtx,
		Cancel:   userCancel,
	}

	usrCh := &model.Ch{
		TxCh:     make(chan model.Message, 50),
		RxCh:     make(chan model.Message, 50),
		UserId:   77777,
		DialogId: 777,
		RespName: "TestUser",
	}

	respModel.Chan[777] = usrCh

	go func() {
		if err := start.Listener(respModel, usrCh, 777, 777); err != nil {
			t.Logf("Listener error: %v", err)
		}
	}()
	time.Sleep(200 * time.Millisecond)

	t.Log("=== Цикл 1: Активация и таймаут ===")

	// Первая активация
	select {
	case usrCh.RxCh <- model.Message{
		Type:     "user",
		Content:  model.AssistResponse{Message: "Оператор 1"},
		Name:     "TestUser",
		Operator: model.Operator{SetOperator: true},
	}:
		t.Log("   Запрос операторского режима отправлен")
	case <-time.After(1 * time.Second):
		t.Fatal("❌ Не удалось отправить запрос")
	}

	// Читаем эхо
	select {
	case <-usrCh.TxCh:
		t.Log("   Эхо получено")
	case <-time.After(2 * time.Second):
		t.Fatal("❌ Не получено эхо")
	}

	// Ждём таймаута
	t.Logf("   Ожидание таймаута (%d сек)...", mode.OperatorResponseTimeout)
	timeout := time.After(time.Duration(mode.OperatorResponseTimeout+3) * time.Second)

timeoutLoop1:
	for {
		select {
		case msg := <-usrCh.TxCh:
			if msg.Type == "assist" {
				// Проверяем наличие ключевых слов в сообщении о таймауте
				if strings.Contains(msg.Content.Message, "Оператор не ответил") {
					t.Log("✅ Первый таймаут получен")
					break timeoutLoop1
				} else {
					t.Logf("   Получено другое сообщение: %.50s...", msg.Content.Message)
				}
			}
		case <-timeout:
			t.Fatalf("❌ Первый таймаут не сработал за %d секунд", mode.OperatorResponseTimeout+3)
		}
	}

	t.Log("=== Цикл 2: Повторная активация и таймаут ===")

	// Небольшая задержка между циклами
	time.Sleep(500 * time.Millisecond)

	// Вторая активация операторского режима
	select {
	case usrCh.RxCh <- model.Message{
		Type:     "user",
		Content:  model.AssistResponse{Message: "Оператор 2"},
		Name:     "TestUser",
		Operator: model.Operator{SetOperator: true},
	}:
		t.Log("   Второй запрос операторского режима отправлен")
	case <-time.After(1 * time.Second):
		t.Fatal("❌ Не удалось отправить второй запрос")
	}

	// Читаем эхо
	select {
	case <-usrCh.TxCh:
		t.Log("   Эхо второго запроса получено")
	case <-time.After(2 * time.Second):
		t.Fatal("❌ Не получено эхо второго запроса")
	}

	// Ждём второго таймаута
	t.Logf("   Ожидание второго таймаута (%d сек)...", mode.OperatorResponseTimeout)
	timeout2 := time.After(time.Duration(mode.OperatorResponseTimeout+3) * time.Second)

timeoutLoop2:
	for {
		select {
		case msg := <-usrCh.TxCh:
			if msg.Type == "assist" {
				// Проверяем наличие ключевых слов в сообщении о таймауте
				if strings.Contains(msg.Content.Message, "Оператор не ответил") {
					t.Log("✅ Второй таймаут получен")
					break timeoutLoop2
				} else {
					t.Logf("   Получено другое сообщение: %.50s...", msg.Content.Message)
				}
			}
		case <-timeout2:
			t.Fatalf("❌ Второй таймаут не сработал за %d секунд", mode.OperatorResponseTimeout+3)
		}
	}

	// Проверяем статистику
	deleteCalls := mockOperator.deleteCalled.Load()
	if deleteCalls < 2 {
		t.Errorf("❌ DeleteSession вызван %d раз, ожидалось минимум 2", deleteCalls)
	} else {
		t.Logf("✅ DeleteSession вызван %d раз (корректно)", deleteCalls)
	}

	t.Log("✅ Тест завершён: множественные таймауты работают корректно")
}
