package model

import (
	"encoding/json"

	"github.com/ikermy/AiR_Common/pkg/logger"
)

// CreatorType определяет тип создателя сообщения
type CreatorType int

const (
	CreatorAssistant CreatorType = 1
	CreatorUser      CreatorType = 2
)

// DialogDB интерфейс для работы с диалогами в БД
type DialogDB interface {
	// SaveDialog сохраняет сообщение диалога
	// p_DialogId - ID диалога
	// p_Data - JSON с данными сообщения (creator, message, timestamp)
	SaveDialog(dialogId uint64, data json.RawMessage) error

	// ReadDialog читает всю историю диалога
	// Возвращает JSON объект с полями: Data, Type, Model, Responder, Date
	// где Data - массив сообщений диалога
	ReadDialog(dialogId uint64) (DialogData, error)
}

// DialogData структура для десериализации данных диалога из БД
type DialogData struct {
	Data      []DialogMessage `json:"Data"`      // Массив сообщений диалога
	Type      int             `json:"Type"`      // Тип диалога
	Model     string          `json:"Model"`     // Имя модели (responder)
	Responder string          `json:"Responder"` // Имя пользователя (GPT name)
	Date      string          `json:"Date"`      // Дата последнего обновления
}

// UnmarshalJSON обрабатывает различные форматы данных диалога:
// 1. Массив JSON-строк (текущий формат БД)
// 2. Объект с полем Data как массив (новый формат)
// 3. Объект с полем Data как строка, содержащая JSON-массив (текущий формат БД)
// 4. Объект с полем Data как строка (старый формат)
func (d *DialogData) UnmarshalJSON(data []byte) error {
	// Пробуем распарсить как массив JSON-строк (формат из БД - вариант 1)
	var jsonStrings []string
	if err := json.Unmarshal(data, &jsonStrings); err == nil {
		// Успешно распарсили как массив строк
		d.Data = make([]DialogMessage, 0, len(jsonStrings))

		for i, jsonStr := range jsonStrings {
			var msg DialogMessage
			if err := json.Unmarshal([]byte(jsonStr), &msg); err != nil {
				// Логируем ошибку парсинга для диагностики
				logger.Warn("Ошибка парсинга сообщения %d в DialogData: %v. JSON: %s", i, err, jsonStr)
				continue
			}
			d.Data = append(d.Data, msg)
		}
		return nil
	}

	// Вспомогательная структура для парсинга объекта
	type Alias struct {
		Data      json.RawMessage `json:"Data"` // Сначала читаем как сырой JSON
		Type      json.RawMessage `json:"Type"` // Может быть int или string
		Model     string          `json:"Model"`
		Responder string          `json:"Responder"`
		Date      string          `json:"Date"`
	}

	var aux Alias
	if err := json.Unmarshal(data, &aux); err != nil {
		// Если не получилось распарсить - возвращаем пустой массив
		logger.Error("DialogData.UnmarshalJSON: Не удалось распарсить ни как массив, ни как объект. Данные: %s", string(data))
		d.Data = []DialogMessage{}
		return nil
	}

	// Пробуем распарсить Type
	var typeInt int
	if err := json.Unmarshal(aux.Type, &typeInt); err == nil {
		d.Type = typeInt
	}

	d.Model = aux.Model
	d.Responder = aux.Responder
	d.Date = aux.Date

	// Пробуем распарсить Data как строку, содержащую JSON-массив (текущий формат БД)
	var dataJsonStr string
	if err := json.Unmarshal(aux.Data, &dataJsonStr); err == nil {
		// Теперь парсим эту строку как массив JSON-строк
		var jsonStrings []string
		if err := json.Unmarshal([]byte(dataJsonStr), &jsonStrings); err == nil {
			d.Data = make([]DialogMessage, 0, len(jsonStrings))

			for i, jsonStr := range jsonStrings {
				var msg DialogMessage
				// json.Unmarshal автоматически вызовет кастомный UnmarshalJSON
				if err := json.Unmarshal([]byte(jsonStr), &msg); err != nil {
					logger.Error("Ошибка парсинга сообщения %d: %v. JSON: %s", i, err, jsonStr)
					continue
				}
				d.Data = append(d.Data, msg)
			}
			return nil
		} else {
			logger.Warn("DialogData.UnmarshalJSON: Data - строка, но не JSON-массив: %v", err)
			// Старый формат - просто строка
			d.Data = []DialogMessage{}
			return nil
		}
	}

	// Пробуем распарсить Data как массив (новый формат)
	var messages []DialogMessage
	if err := json.Unmarshal(aux.Data, &messages); err == nil {
		d.Data = messages
		return nil
	}

	// Если ничего не подошло, возвращаем пустой массив
	d.Data = []DialogMessage{}
	return nil
}

// DialogMessage представляет одно сообщение в диалоге
type DialogMessage struct {
	Creator   int    `json:"creator"`   // 1 - ассистент, 2 - пользователь
	Message   string `json:"message"`   // Текст сообщения
	Timestamp string `json:"timestamp"` // Временная метка
}

// UnmarshalJSON обрабатывает различные форматы поля message:
// 1. Простая строка: "message": "текст"
// 2. Объект: "message": {"message": "текст", "action": {}}
func (m *DialogMessage) UnmarshalJSON(data []byte) error {
	// Вспомогательная структура для парсинга
	type Alias struct {
		Creator   int             `json:"creator"`
		Message   json.RawMessage `json:"message"` // Читаем как сырой JSON
		Timestamp string          `json:"timestamp"`
	}

	var aux Alias
	if err := json.Unmarshal(data, &aux); err != nil {
		logger.Warn("DialogMessage.UnmarshalJSON: Ошибка парсинга базовой структуры: %v. JSON: %s", err, string(data))
		return err
	}

	m.Creator = aux.Creator
	m.Timestamp = aux.Timestamp

	// Пробуем распарсить message как строку (старый формат)
	var msgStr string
	if err := json.Unmarshal(aux.Message, &msgStr); err == nil {
		m.Message = msgStr
		return nil
	}

	// Пробуем распарсить как объект (новый формат)
	type MessageObject struct {
		Message string          `json:"message"`
		Action  json.RawMessage `json:"action"`
	}

	var msgObj MessageObject
	if err := json.Unmarshal(aux.Message, &msgObj); err == nil {
		m.Message = msgObj.Message
		return nil
	}

	// Если ничего не подошло, устанавливаем пустую строку
	logger.Warn("DialogMessage.UnmarshalJSON: Не удалось распарсить message, устанавливаем пустую строку. Raw: %s", string(aux.Message))
	m.Message = ""
	return nil
}
