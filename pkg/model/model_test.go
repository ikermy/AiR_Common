package model

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSafeClose проверяет, что safeClose не вызывает панику при повторном закрытии
func TestSafeClose(t *testing.T) {
	ch := make(chan Message, 1)

	// Первое закрытие - должно быть успешным
	safeClose(ch)

	// Второе закрытие - НЕ должно вызывать панику
	safeClose(ch)

	// Третье закрытие nil канала - НЕ должно вызывать панику
	var nilCh chan Message
	safeClose(nilCh)
}

// TestChSendToRxConcurrent проверяет безопасность SendToRx при конкурентном доступе
func TestChSendToRxConcurrent(t *testing.T) {
	ch := &Ch{
		RxCh:     make(chan Message, 10),
		UserId:   1,
		DialogId: 100,
		RespName: "TestUser",
	}

	var wg sync.WaitGroup
	var successCount atomic.Int32
	var errorCount atomic.Int32

	// Запускаем 100 горутин, отправляющих сообщения
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			msg := Message{
				Type: "user",
				Content: AssistResponse{
					Message: "Test message",
				},
			}

			if err := ch.SendToRx(msg); err != nil {
				errorCount.Add(1)
			} else {
				successCount.Add(1)
			}
		}(i)
	}

	// Через 10ms закрываем канал
	time.Sleep(10 * time.Millisecond)
	ch.Close()

	wg.Wait()

	t.Logf("Success: %d, Errors: %d", successCount.Load(), errorCount.Load())

	// Проверяем, что не было паники
	if successCount.Load()+errorCount.Load() != 100 {
		t.Errorf("Expected 100 operations, got %d", successCount.Load()+errorCount.Load())
	}
}

// TestChCloseWhileSending проверяет отправку в закрываемый канал
func TestChCloseWhileSending(t *testing.T) {
	ch := &Ch{
		RxCh:     make(chan Message, 1),
		UserId:   1,
		DialogId: 100,
		RespName: "TestUser",
	}

	var wg sync.WaitGroup
	panicCount := atomic.Int32{}

	// Горутина отправляет сообщения
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				panicCount.Add(1)
				t.Errorf("Паника не должна происходить: %v", r)
			}
		}()

		for i := 0; i < 1000; i++ {
			msg := Message{Type: "user"}
			_ = ch.SendToRx(msg)
			time.Sleep(time.Microsecond)
		}
	}()

	// Горутина закрывает канал
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		ch.Close()
	}()

	wg.Wait()

	if panicCount.Load() > 0 {
		t.Errorf("Обнаружено %d паник", panicCount.Load())
	}
}

// TestChIsOpenFlags проверяет корректность флагов открытия/закрытия
func TestChIsOpenFlags(t *testing.T) {
	ch := &Ch{
		RxCh:     make(chan Message, 1),
		TxCh:     make(chan Message, 1),
		UserId:   1,
		DialogId: 100,
		RespName: "TestUser",
	}

	// Изначально каналы открыты
	if !ch.IsRxOpen() {
		t.Error("RxCh должен быть открыт")
	}
	if !ch.IsTxOpen() {
		t.Error("TxCh должен быть открыт")
	}

	// Закрываем
	ch.Close()

	// После закрытия флаги должны быть установлены
	if ch.IsRxOpen() {
		t.Error("RxCh должен быть закрыт")
	}
	if ch.IsTxOpen() {
		t.Error("TxCh должен быть закрыт")
	}

	// Попытка отправки должна вернуть ошибку
	msg := Message{Type: "user"}
	if err := ch.SendToRx(msg); err == nil {
		t.Error("Отправка в закрытый канал должна вернуть ошибку")
	}
}

// TestChSendToTx проверяет безопасную отправку в TxCh
func TestChSendToTx(t *testing.T) {
	ch := &Ch{
		TxCh:     make(chan Message, 1),
		UserId:   1,
		DialogId: 100,
		RespName: "TestUser",
	}

	msg := Message{
		Type: "assist",
		Content: AssistResponse{
			Message: "Test response",
		},
	}

	// Успешная отправка
	if err := ch.SendToTx(msg); err != nil {
		t.Errorf("Отправка должна быть успешной: %v", err)
	}

	// Закрываем канал
	ch.Close()

	// Отправка после закрытия должна вернуть ошибку
	if err := ch.SendToTx(msg); err == nil {
		t.Error("Отправка в закрытый канал должна вернуть ошибку")
	}
}

// BenchmarkChSendToRx измеряет производительность безопасной отправки
func BenchmarkChSendToRx(b *testing.B) {
	ch := &Ch{
		RxCh:     make(chan Message, 100),
		UserId:   1,
		DialogId: 100,
		RespName: "BenchUser",
	}

	// Горутина для чтения сообщений
	go func() {
		for range ch.RxCh {
			// Просто читаем и отбрасываем
		}
	}()

	msg := Message{
		Type: "user",
		Content: AssistResponse{
			Message: "Benchmark message",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ch.SendToRx(msg)
	}

	ch.Close()
}

// BenchmarkDirectChannelSend измеряет производительность прямой отправки (для сравнения)
func BenchmarkDirectChannelSend(b *testing.B) {
	ch := make(chan Message, 100)

	// Горутина для чтения сообщений
	go func() {
		for range ch {
			// Просто читаем и отбрасываем
		}
	}()

	msg := Message{
		Type: "user",
		Content: AssistResponse{
			Message: "Benchmark message",
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ch <- msg
	}

	close(ch)
}
