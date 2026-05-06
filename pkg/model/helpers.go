package model

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ============================================================================
// CHANNEL PROVIDER INTERFACE
// ============================================================================

// ChannelProvider интерфейс для провайдеров, которые могут предоставить канал
// Реализуется провайдеро-специфичными структурами RespModel
type ChannelProvider interface {
	GetChannel() *Ch
	GetChannelMap() map[uint64]*Ch
}

// ============================================================================
// UNIVERSAL CHANNEL FUNCTIONS
// ============================================================================

// GetChannel универсальная функция для получения канала от провайдера
// Работает с waitChannels и автоматически ждет создания канала если его еще нет
// Параметры:
//   - respId: ID респондента
//   - ctx: контекст для отмены операции
//   - waitChannels: sync.Map для координации между горутинами
//   - responders: sync.Map содержащий RespModel провайдера
//   - extractChannel: функция для извлечения канала из провайдеро-специфичной структуры
func GetChannel(
	respId uint64,
	ctx context.Context,
	waitChannels *sync.Map,
	responders *sync.Map,
	extractChannel func(interface{}) (*Ch, error),
) (*Ch, error) {
	// Пытаемся получить или создать wait channel
	waitChInterface, exists := waitChannels.Load(respId)
	var waitCh chan struct{}

	if !exists {
		waitCh = make(chan struct{})
		waitChannels.Store(respId, waitCh)
	} else {
		waitCh = waitChInterface.(chan struct{})
	}

	// Пробуем сразу получить канал
	userCh, err := getTryChannel(respId, responders, extractChannel)
	if err == nil {
		return userCh, nil
	}

	// Если канала нет, ждем его создания
	select {
	case <-waitCh:
		return getTryChannel(respId, responders, extractChannel)
	case <-ctx.Done():
		return nil, fmt.Errorf("отменено контекстом ожидание канала для responderId %d", respId)
	case <-time.After(1 * time.Second):
		return nil, fmt.Errorf("тайм-аут при ожидании канала для responderId %d", respId)
	}
}

// getTryChannel универсальная функция для попытки получить канал
// Не ждет создания канала, возвращает ошибку если канал не найден
// Параметры:
//   - respId: ID респондента
//   - responders: sync.Map содержащий RespModel провайдера
//   - extractChannel: функция для извлечения канала из провайдеро-специфичной структуры
func getTryChannel(
	respId uint64,
	responders *sync.Map,
	extractChannel func(interface{}) (*Ch, error),
) (*Ch, error) {
	val, ok := responders.Load(respId)
	if !ok {
		return nil, fmt.Errorf("RespModel не найден для respId %d", respId)
	}

	return extractChannel(val)
}

// ============================================================================
// CHANNEL EXTRACTION HELPERS
// ============================================================================

// ExtractChannelWithPriority извлекает канал с приоритетом ChanMap над Chan
// Используется для OpenAI и Google провайдеров
// ПРИОРИТЕТ: ChanMap (установленный через TestSession для TRUE STREAMING)
// Fallback: возвращаем основной канал если ChanMap пуст
func ExtractChannelWithPriority(provider ChannelProvider) (*Ch, error) {
	chanMap := provider.GetChannelMap()

	// ПРИОРИТЕТ: ChanMap (установленный через TestSession для TRUE STREAMING)
	if chanMap != nil && len(chanMap) > 0 {
		for _, ch := range chanMap {
			return ch, nil
		}
	}

	// Fallback: возвращаем основной канал если ChanMap пуст
	mainChan := provider.GetChannel()
	if mainChan == nil {
		return nil, fmt.Errorf("канал не найден")
	}

	return mainChan, nil
}

// ExtractChannelSimple извлекает только основной канал
// Используется для Mistral провайдера (если не используется ChanMap)
func ExtractChannelSimple(provider ChannelProvider) (*Ch, error) {
	// Сначала проверяем ChanMap (для унификации)
	chanMap := provider.GetChannelMap()
	if chanMap != nil && len(chanMap) > 0 {
		for _, ch := range chanMap {
			return ch, nil
		}
	}

	// Fallback: основной канал
	mainChan := provider.GetChannel()
	if mainChan == nil {
		return nil, fmt.Errorf("канал не найден")
	}

	return mainChan, nil
}

// ============================================================================
// RESPONDER MANAGEMENT HELPERS
// ============================================================================

// GetRespIdBydialogIDUniversal универсальная функция (non-generic) для поиска respId по DialogID
// Использует type assertion для работы с любым типом RespModel
func GetRespIdBydialogIDUniversal(dialogID uint64, responders *sync.Map) (uint64, error) {
	var foundRespId uint64
	found := false

	responders.Range(func(key, value interface{}) bool {
		// Используем интерфейс ChannelProvider для унифицированного доступа к каналу
		if provider, ok := value.(ChannelProvider); ok {
			mainChan := provider.GetChannel()
			if mainChan != nil && mainChan.DialogID == dialogID {
				respId, ok := key.(uint64)
				if ok {
					foundRespId = respId
					found = true
					return false // Прекращаем поиск
				}
			}
		}
		return true // Продолжаем поиск
	})

	if !found {
		return 0, fmt.Errorf("RespModel не найден для DialogID %d", dialogID)
	}

	return foundRespId, nil
}

// CloseResponderChannelsUniversal универсальная функция (non-generic) для закрытия каналов
func CloseResponderChannelsUniversal(provider ChannelProvider) {
	// Закрываем основной канал
	if mainChan := provider.GetChannel(); mainChan != nil {
		if mainChan.IsTxOpen() {
			mainChan.CloseTx()
		}
		if mainChan.IsRxOpen() {
			mainChan.CloseRx()
		}
	}

	// Закрываем все каналы в ChanMap
	if chanMap := provider.GetChannelMap(); chanMap != nil {
		for _, ch := range chanMap {
			if ch != nil {
				if ch.IsTxOpen() {
					ch.CloseTx()
				}
				if ch.IsRxOpen() {
					ch.CloseRx()
				}
			}
		}
	}
}

// CleanupWaitChannelsUniversal универсальная функция для очистки зависших waitChannels
func CleanupWaitChannelsUniversal(waitChannels *sync.Map, responders *sync.Map) int {
	deletedCount := 0

	waitChannels.Range(func(key, value interface{}) bool {
		respId, ok := key.(uint64)
		if !ok {
			return true
		}

		// Проверяем, существует ли респондент
		if _, exists := responders.Load(respId); !exists {
			// Закрываем канал перед удалением
			if ch, ok := value.(chan struct{}); ok {
				select {
				case <-ch:
					// Канал уже закрыт
				default:
					close(ch)
				}
			}
			waitChannels.Delete(respId)
			deletedCount++
		}

		return true
	})

	return deletedCount
}

// CleanupAllRespondersUniversal универсальная функция для очистки всех респондеров при shutdown
func CleanupAllRespondersUniversal(
	responders *sync.Map,
	cancelFunc func(interface{}),
	closeChannels func(interface{}),
) {
	responders.Range(func(key, value interface{}) bool {
		// Отменяем контекст
		if cancelFunc != nil {
			cancelFunc(value)
		}

		// Закрываем каналы
		if closeChannels != nil {
			closeChannels(value)
		} else if provider, ok := value.(ChannelProvider); ok {
			CloseResponderChannelsUniversal(provider)
		}

		// Удаляем респондента
		responders.Delete(key)

		return true
	})
}
