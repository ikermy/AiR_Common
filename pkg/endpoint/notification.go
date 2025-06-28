package endpoint

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"github.com/ikermy/AiR_Common/pkg/common"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"io"
	"log"
	"net/http"
	"strconv"
)

func SendEvent(userId uint32, event, userName, assistName, target string) {
	msg := common.CarpCh{
		UserID:     userId,
		Event:      event,
		UserName:   userName,
		AssistName: assistName,
		Target:     target,
	}

	select {
	case mode.CarpinteroCh <- msg:
	default:
		log.Printf("CarpinteroCh: канал закрыт или переполнен, не удалось отправить сообщение: %+v", msg)
	}
}

func (e *Endpoint) SendWebhookNotification(msg common.CarpCh) error {
	res, err := e.Db.GetNotificationChannel(msg.UserID)
	if err != nil {
		return fmt.Errorf("ошибка получения каналов уведомлений: %w", err)
	}

	// Парсим JSON
	var channels []map[string]interface{}
	err = json.Unmarshal(res, &channels)
	if err != nil {
		return fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	for _, ch := range channels {
		fmt.Printf("Тип: %v, Значение: %v\n", ch["channel_type"], ch["channel_value"])

		switch ch["channel_type"] {
		case "telegram":
			// Проверяю что Telegram не null
			if ch["channel_value"] == "null" {
				return fmt.Errorf("у пользователя %d не задан Telegram ID, уведомление не отправлено", msg.UserID)
			}
			// Подготовка сообщения Telegram
			telegramValue, ok := ch["channel_value"].(string)
			if !ok {
				return fmt.Errorf("channel_value не является строкой")
			}
			tId, err := strconv.ParseInt(telegramValue, 10, 64)
			if err != nil {
				return fmt.Errorf("ошибка преобразования Telegram ID: %w", err)
			}
			err = SendTelegramNotification(tId, msg.Event, msg.UserName, msg.AssistName, msg.Target)
			if err != nil {
				return fmt.Errorf("ошибка отправки Telegram уведомления: %w", err)
			}
		case "email":
			// Проверяю что Email не null
			if ch["channel_value"] == "null" {
				return fmt.Errorf("у пользователя %d не задан Email, уведомление не отправлено", msg.UserID)
			}
			// Подготовка сообщения Email
			emailValue, ok := ch["channel_value"].(string)
			if !ok {
				return fmt.Errorf("channel_value не является строкой")
			}
			err = SendEmailNotification(emailValue, msg.Event, msg.UserName, msg.AssistName, msg.Target)
			if err != nil {
				return fmt.Errorf("ошибка отправки Email уведомления: %w", err)
			}
		default:
			log.Printf("Неизвестный канал уведомлений: %s для пользователя %d", ch["channel_type"], msg.UserID)
		}
	}

	return nil
}

func SendTelegramNotification(tId int64, event, userName, assistName, target string) error {
	// Формируем URL для webhook
	//url := fmt.Sprintf("https://localhost:8088/notification")
	url := fmt.Sprintf("http://localhost:%s/notification", mode.CarpinteroPort)

	// Создаем данные для отправки
	payload := map[string]interface{}{
		"tid":    tId,
		"event":  event,
		"user":   userName,
		"assist": assistName,
		"target": target,
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

func SendEmailNotification(email, event, userName, assistName, target string) error {
	// Формируем URL для webhook
	url := fmt.Sprintf("https://%s:%s/notification", mode.CarpinteroHost, mode.CarpinteroPort)

	// Создаем данные для отправки
	payload := map[string]interface{}{
		"email":  email,
		"event":  event,
		"user":   userName,
		"assist": assistName,
		"target": target,
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

	// Создаем HTTP-клиент с отключенной проверкой сертификата
	// нужно брать сертификаты из /etc/ssl/custom/..
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

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

func CreateMessageFromEvent(Event, UserName, AssistName, Target string) (string, error) {
	var msg string

	switch Event {
	case "start":
		msg = fmt.Sprintf("Пользователь %s начал диалог с ассистентом %s", UserName, AssistName)
	case "end":
		msg = fmt.Sprintf("Пользователь %s завершил диалог с ассистентом %s", UserName, AssistName)
	case "target":
		msg = fmt.Sprintf("Ассистент %s достиг цели '%s' в диалоге с пользователем %s", AssistName, Target, UserName)
	case "trigger":
		msg = fmt.Sprintf("Ассистент %s сработал на триггер '%s' в диалоге с пользователем %s", AssistName, Target, UserName)
	case "reauth":
		msg = fmt.Sprintf("Канал %s отключен, требуется повторная авторизация", Target)
	case "subscription":
		errMsg := map[common.ErrorCode]string{
			common.ErrNoSubscription:       "У вас нет подписки. Пожалуйста, оформите подписку.",
			common.ErrSubscriptionExpired:  "Ваша подписка истекла. Пожалуйста, продлите подписку.",
			common.ErrMessageLimitExceeded: "Вы превысили лимит сообщений. Пожалуйста, пополните баланс.",
			common.ErrInsufficientBalance:  "Недостаточно средств на балансе. Пожалуйста, пополните баланс.",
		}
		errorCode, _ := strconv.Atoi(Target)
		msg = errMsg[common.ErrorCode(errorCode)]
	default:
		return "", fmt.Errorf("неизвестное событие: %s", Event)
	}

	return msg, nil
}

func (e *Endpoint) NotificationListener() {
	for {
		select {
		case msg, ok := <-mode.CarpinteroCh:
			if !ok {
				log.Println("CarpinteroCh closed")
				return
			}
			err := e.SendWebhookNotification(msg)
			if err != nil {
				log.Println("'NotificationListener': ошибка отправки уведомления:", err)
			}
		}
	}
}
