package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

func SendWebhookNotification(msg CarpCh) error {
	// Формируем URL для webhook
	url := fmt.Sprintf("https://localhost:8088/notification")
	//url := fmt.Sprintf("https://app:8088/notification")

	// Создаем данные для отправки
	payload := map[string]interface{}{
		"id":     msg.UserID,
		"event":  msg.Event,
		"user":   msg.UserName,
		"assist": msg.AssistName,
		"target": msg.Target,
	}

	// Преобразуем данные в JSON
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ошибка при преобразовании данных в JSON: %w", err)
	}

	// Создаем HTTP-запрос
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("ошибка при создании HTTP-запроса: %w", err)
	}

	// Устанавливаем заголовки
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}

	// Отправляем запрос
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка при отправке HTTP-запроса: %w", err)
	}
	defer resp.Body.Close()

	// Проверяем статус ответа
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("неожиданный статус ответа: %d, тело: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

func NotificationListener() {
	for {
		select {
		case msg, ok := <-CarpinteroCh:
			if !ok {
				log.Println("CarpinteroCh closed")
				return
			}
			SendWebhookNotification(msg)
		}
	}
}
