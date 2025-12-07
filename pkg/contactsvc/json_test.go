package contactsvc

import (
	"encoding/json"
	"testing"

	"github.com/ikermy/AiR_Common/pkg/contactsvc/pb"
)

func TestJSONSerialization(t *testing.T) {
	// Создаём тестовые данные
	original := &pb.Result{
		Humans: []*pb.Contact{
			{
				Id:        123,
				FirstName: "John",
				LastName:  "Doe",
				Username:  "johndoe",
				Phone:     "+1234567890",
			},
			{
				Id:        456,
				FirstName: "Jane",
				LastName:  "Smith",
				Username:  "janesmith",
				Phone:     "+0987654321",
			},
		},
		Bots: []*pb.Contact{
			{
				Id:        789,
				FirstName: "TestBot",
				Username:  "testbot",
			},
		},
		Channels: []*pb.Channel{
			{
				Id:       1001,
				Title:    "Test Channel",
				Username: "testchannel",
			},
		},
		Groups: []*pb.Group{
			{
				Id:    2001,
				Title: "Test Group",
			},
		},
		Supergroups: []*pb.Supergroup{
			{
				Id:       3001,
				Title:    "Test Supergroup",
				Username: "testsupergroup",
			},
		},
		Service: pb.TELEGRAM,
		UserId:  42,
	}

	// Сериализуем в JSON
	jsonData, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Failed to marshal to JSON: %v", err)
	}

	t.Logf("JSON data: %s", string(jsonData))

	// Десериализуем обратно
	var deserialized pb.Result
	err = json.Unmarshal(jsonData, &deserialized)
	if err != nil {
		t.Fatalf("Failed to unmarshal from JSON: %v", err)
	}

	// Проверяем Humans
	if len(deserialized.Humans) != 2 {
		t.Errorf("Expected 2 humans, got %d", len(deserialized.Humans))
	}
	if deserialized.Humans[0].Id != 123 {
		t.Errorf("Expected human[0].Id=123, got %d", deserialized.Humans[0].Id)
	}
	if deserialized.Humans[0].FirstName != "John" {
		t.Errorf("Expected human[0].FirstName='John', got '%s'", deserialized.Humans[0].FirstName)
	}

	// Проверяем Service и UserId
	if deserialized.Service != pb.TELEGRAM {
		t.Errorf("Expected Service=TELEGRAM, got %d", deserialized.Service)
	}
	if deserialized.UserId != 42 {
		t.Errorf("Expected UserId=42, got %d", deserialized.UserId)
	}

	// Проверяем Bots
	if len(deserialized.Bots) != 1 {
		t.Errorf("Expected 1 bot, got %d", len(deserialized.Bots))
	}
	if deserialized.Bots[0].Id != 789 {
		t.Errorf("Expected bot[0].Id=789, got %d", deserialized.Bots[0].Id)
	}

	// Проверяем Channels
	if len(deserialized.Channels) != 1 {
		t.Errorf("Expected 1 channel, got %d", len(deserialized.Channels))
	}
	if deserialized.Channels[0].Title != "Test Channel" {
		t.Errorf("Expected channel[0].Title='Test Channel', got '%s'", deserialized.Channels[0].Title)
	}

	// Проверяем Groups
	if len(deserialized.Groups) != 1 {
		t.Errorf("Expected 1 group, got %d", len(deserialized.Groups))
	}
	if deserialized.Groups[0].Title != "Test Group" {
		t.Errorf("Expected group[0].Title='Test Group', got '%s'", deserialized.Groups[0].Title)
	}

	// Проверяем Supergroups
	if len(deserialized.Supergroups) != 1 {
		t.Errorf("Expected 1 supergroup, got %d", len(deserialized.Supergroups))
	}
	if deserialized.Supergroups[0].Title != "Test Supergroup" {
		t.Errorf("Expected supergroup[0].Title='Test Supergroup', got '%s'", deserialized.Supergroups[0].Title)
	}
}

func TestJSONDeserializationFromRawMessage(t *testing.T) {
	// Тестируем десериализацию из json.RawMessage (как в client.go)
	jsonStr := `{
		"humans": [
			{
				"id": 123,
				"first_name": "John",
				"last_name": "Doe",
				"username": "johndoe",
				"phone": "+1234567890"
			}
		],
		"bots": [],
		"channels": [
			{
				"id": 1001,
				"title": "Test Channel",
				"username": "testchannel"
			}
		],
		"groups": [],
		"supergroups": [],
		"service": 1,
		"user_id": 42
	}`

	contactsData := json.RawMessage(jsonStr)

	var finalResult pb.Result
	err := json.Unmarshal(contactsData, &finalResult)
	if err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	if len(finalResult.Humans) != 1 {
		t.Errorf("Expected 1 human, got %d", len(finalResult.Humans))
	}

	if finalResult.Humans[0].Id != 123 {
		t.Errorf("Expected Id=123, got %d", finalResult.Humans[0].Id)
	}

	if finalResult.Humans[0].FirstName != "John" {
		t.Errorf("Expected FirstName='John', got '%s'", finalResult.Humans[0].FirstName)
	}

	if len(finalResult.Channels) != 1 {
		t.Errorf("Expected 1 channel, got %d", len(finalResult.Channels))
	}

	if finalResult.Channels[0].Title != "Test Channel" {
		t.Errorf("Expected Title='Test Channel', got '%s'", finalResult.Channels[0].Title)
	}

	if finalResult.Service != pb.TELEGRAM {
		t.Errorf("Expected Service=1, got %d", finalResult.Service)
	}

	if finalResult.UserId != 42 {
		t.Errorf("Expected UserId=42, got %d", finalResult.UserId)
	}
}
