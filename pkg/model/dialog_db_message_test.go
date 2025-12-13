package model

import (
	"encoding/json"
	"testing"
)

func TestDialogMessage_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name        string
		jsonData    string
		wantMessage string
		wantCreator int
	}{
		{
			name:        "Старый формат - строка",
			jsonData:    `{"creator":2,"message":"привет мне нужен дизельный погрузчик\n","timestamp":"2025-12-13T14:11:40-03:00"}`,
			wantMessage: "привет мне нужен дизельный погрузчик\n",
			wantCreator: 2,
		},
		{
			name:        "Новый формат - объект",
			jsonData:    `{"creator":2,"message":{"message":"привет мне нужен дизельный погрузчик","action":{}},"timestamp":"2025-12-13T14:11:39.7772397-03:00"}`,
			wantMessage: "привет мне нужен дизельный погрузчик",
			wantCreator: 2,
		},
		{
			name:        "Ассистент - старый формат",
			jsonData:    `{"creator":1,"message":"Какая грузоподъемность вам нужна?","timestamp":"2025-12-13T14:11:40-03:00"}`,
			wantMessage: "Какая грузоподъемность вам нужна?",
			wantCreator: 1,
		},
		{
			name:        "Ассистент - новый формат",
			jsonData:    `{"creator":1,"message":{"message":"Какая грузоподъемность вам нужна?","action":{}},"timestamp":"2025-12-13T14:11:40.5993325-03:00"}`,
			wantMessage: "Какая грузоподъемность вам нужна?",
			wantCreator: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg DialogMessage
			if err := json.Unmarshal([]byte(tt.jsonData), &msg); err != nil {
				t.Errorf("Ошибка парсинга: %v", err)
				return
			}

			if msg.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", msg.Message, tt.wantMessage)
			}

			if msg.Creator != tt.wantCreator {
				t.Errorf("Creator = %d, want %d", msg.Creator, tt.wantCreator)
			}
		})
	}
}

func TestDialogData_UnmarshalJSON_MixedFormats(t *testing.T) {
	// Реальный пример из БД с смешанными форматами
	jsonData := `[
		"{\"creator\":2,\"message\":\"привет мне нужен дизельный погрузчик\\n\",\"timestamp\":\"2025-12-13T14:11:40-03:00\"}",
		"{\"creator\":1,\"message\":\"Какая грузоподъемность вам нужна?\",\"timestamp\":\"2025-12-13T14:11:40-03:00\"}",
		"{\"creator\":2,\"message\":{\"message\":\"привет мне нужен дизельный погрузчик\",\"action\":{}},\"timestamp\":\"2025-12-13T14:11:39.7772397-03:00\"}",
		"{\"creator\":1,\"message\":{\"message\":\"Какая грузоподъемность вам нужна?\",\"action\":{}},\"timestamp\":\"2025-12-13T14:11:40.5993325-03:00\"}"
	]`

	var dialogData DialogData
	if err := json.Unmarshal([]byte(jsonData), &dialogData); err != nil {
		t.Fatalf("Ошибка парсинга DialogData: %v", err)
	}

	if len(dialogData.Data) != 4 {
		t.Fatalf("Ожидалось 4 сообщения, получено %d", len(dialogData.Data))
	}

	// Проверяем первое сообщение (старый формат)
	if dialogData.Data[0].Message != "привет мне нужен дизельный погрузчик\n" {
		t.Errorf("Сообщение 0: %q", dialogData.Data[0].Message)
	}

	// Проверяем второе сообщение (старый формат)
	if dialogData.Data[1].Message != "Какая грузоподъемность вам нужна?" {
		t.Errorf("Сообщение 1: %q", dialogData.Data[1].Message)
	}

	// Проверяем третье сообщение (новый формат)
	if dialogData.Data[2].Message != "привет мне нужен дизельный погрузчик" {
		t.Errorf("Сообщение 2: %q", dialogData.Data[2].Message)
	}

	// Проверяем четвертое сообщение (новый формат)
	if dialogData.Data[3].Message != "Какая грузоподъемность вам нужна?" {
		t.Errorf("Сообщение 3: %q", dialogData.Data[3].Message)
	}
}
