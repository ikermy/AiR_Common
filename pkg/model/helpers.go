package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// StreamingToSync буферизирует вызов RequestStreaming и возвращает AssistResponse.
// Устраняет дублирование одинаковой реализации метода Request у всех провайдеров.
//
// Использование:
//
//	func (m *MyModel) Request(UserID uint32, dialogID uint64, text string, files ...model.FileUpload) (model.AssistResponse, error) {
//	    return model.StreamingToSync(text, files, func(onDelta func(string, bool) error, files ...model.FileUpload) error {
//	        return m.RequestStreaming(UserID, dialogID, text, onDelta, files...)
//	    })
//	}
func StreamingToSync(
	text string,
	files []FileUpload,
	streaming func(onDelta func(delta string, done bool) error, files ...FileUpload) error,
) (AssistResponse, error) {
	var empty AssistResponse

	if text == "" && len(files) == 0 {
		return empty, fmt.Errorf("пустое сообщение и нет файлов")
	}

	var buf strings.Builder
	if err := streaming(func(delta string, done bool) error {
		if !done {
			buf.WriteString(delta)
		}
		return nil
	}, files...); err != nil {
		return empty, err
	}

	var resp AssistResponse
	if err := json.Unmarshal([]byte(buf.String()), &resp); err != nil {
		return empty, fmt.Errorf("ошибка парсинга ответа: %w", err)
	}

	return resp, nil
}

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

// ExtractChannelWithPriority извлекает канал с приоритетом ChanMap над Chan.
// Используется всеми провайдерами (OpenAI, Mistral, Google).
// Приоритет: ChanMap (True Streaming) → основной Chan (Fallback).
func ExtractChannelWithPriority(provider ChannelProvider) (*Ch, error) {
	chanMap := provider.GetChannelMap()

	// ПРИОРИТЕТ: ChanMap (установленный через TestSession для TRUE STREAMING)
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

// ============================================================================
// DIALOG PARSING HELPERS
// ============================================================================

// DialogMessageBase общая структура для парсинга истории диалога из БД
type DialogMessageBase struct {
	Creator   interface{} `json:"creator"`
	Message   interface{} `json:"message"`
	Timestamp string      `json:"timestamp"`
}

// ParseDialogHistory парсит историю диалога из БД JSON в структурированный формат
// Поддерживает множество форматов данных, которые БД может вернуть
// Возвращает: []DialogMessageBase с распарсенными сообщениями
func ParseDialogHistory(rawData []byte) ([]DialogMessageBase, error) {
	if len(rawData) == 0 {
		return []DialogMessageBase{}, nil
	}

	var result []DialogMessageBase

	// Структуры-обёртки для разных форматов данных
	type DataWrapperArray struct {
		Data []string `json:"Data"` // Массив JSON строк
	}

	type DataWrapperString struct {
		Data string `json:"Data"` // Строка JSON (с двойной экранизацией)
	}

	type DataWrapperDirect struct {
		Dialog []DialogMessageBase `json:"dialog"` // Прямой массив с полем "dialog"
	}

	// Попытка 1: Парсим как структуру с полем Data (массив строк JSON)
	var wrapperArray DataWrapperArray
	if err := json.Unmarshal(rawData, &wrapperArray); err == nil && len(wrapperArray.Data) > 0 {
		for _, jsonStr := range wrapperArray.Data {
			var msg DialogMessageBase
			if err := json.Unmarshal([]byte(jsonStr), &msg); err == nil {
				result = append(result, msg)
			}
		}
		if len(result) > 0 {
			return result, nil
		}
	}

	// Попытка 2: Парсим как структуру с полем Data (строка JSON)
	var wrapperString DataWrapperString
	if err := json.Unmarshal(rawData, &wrapperString); err == nil && len(wrapperString.Data) > 0 {
		var stringArray []string
		if err := json.Unmarshal([]byte(wrapperString.Data), &stringArray); err == nil && len(stringArray) > 0 {
			for _, jsonStr := range stringArray {
				var msg DialogMessageBase
				if err := json.Unmarshal([]byte(jsonStr), &msg); err == nil {
					result = append(result, msg)
				}
			}
			if len(result) > 0 {
				return result, nil
			}
		}
	}

	// Попытка 3: Парсим как структуру с полем "dialog"
	var wrapperDirect DataWrapperDirect
	if err := json.Unmarshal(rawData, &wrapperDirect); err == nil && len(wrapperDirect.Dialog) > 0 {
		return wrapperDirect.Dialog, nil
	}

	// Попытка 4: Парсим как массив строк напрямую
	var stringArray []string
	if err := json.Unmarshal(rawData, &stringArray); err == nil && len(stringArray) > 0 {
		for _, jsonStr := range stringArray {
			var msg DialogMessageBase
			if err := json.Unmarshal([]byte(jsonStr), &msg); err == nil {
				result = append(result, msg)
			}
		}
		if len(result) > 0 {
			return result, nil
		}
	}

	// Попытка 5: Парсим как прямой массив объектов
	if err := json.Unmarshal(rawData, &result); err == nil && len(result) > 0 {
		return result, nil
	}

	// Все попытки провалились - возвращаем пустой результат
	return []DialogMessageBase{}, nil
}
