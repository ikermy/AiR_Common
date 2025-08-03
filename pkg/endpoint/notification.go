package endpoint

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"github.com/ikermy/AiR_Common/pkg/common"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"io"
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
		logger.Warn("CarpinteroCh: канал закрыт или переполнен, не удалось отправить сообщение: %+v", msg)
	}
}

func (e *Endpoint) SendNotification(msg common.CarpCh) error {
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

	var lastError error
	successCount := 0

	for _, ch := range channels {
		switch ch["channel_type"] {
		case "telegram":
			// Проверяю что Telegram не null
			if ch["channel_value"] == "null" {
				logger.Error("у пользователя %d не задан Telegram ID, уведомление не отправлено", msg.UserID)
				lastError = fmt.Errorf("у пользователя %d не задан Telegram ID", msg.UserID)
				continue
			}
			// Подготовка сообщения Telegram
			telegramValue, ok := ch["channel_value"].(string)
			if !ok {
				logger.Error("channel_value не является строкой для пользователя %d", msg.UserID)
				lastError = fmt.Errorf("channel_value не является строкой")
				continue
			}
			tId, err := strconv.ParseInt(telegramValue, 10, 64)
			if err != nil {
				logger.Error("ошибка преобразования Telegram ID для пользователя %d: %v", msg.UserID, err)
				lastError = err
				continue
			}
			err = SendTelegramNotification(tId, msg.Event, msg.UserName, msg.AssistName, msg.Target)
			if err != nil {
				logger.Error("Ошибка отправки Telegram уведомления: %v", err, msg.UserID)
				lastError = err
				continue
			}
			successCount++

		case "mail":
			// Проверяю что Email не null
			if ch["channel_value"] == "null" {
				logger.Error("у пользователя %d не задан Email, уведомление не отправлено", msg.UserID)
				lastError = fmt.Errorf("у пользователя %d не задан Email", msg.UserID)
				continue
			}
			// Подготовка сообщения Email
			emailValue, ok := ch["channel_value"].(string)
			if !ok {
				logger.Error("channel_value не является строкой для пользователя %d", msg.UserID)
				lastError = fmt.Errorf("channel_value не является строкой")
				continue
			}
			err = SendEmailNotification(emailValue, msg.Event, msg.UserName, msg.AssistName, msg.Target)
			if err != nil {
				logger.Error("ошибка отправки Email уведомления для пользователя %d: %v", msg.UserID, err)
				lastError = err
				continue
			}
			successCount++

		default:
			logger.Warn("Неизвестный канал уведомлений: %s для пользователя %d", ch["channel_type"], msg.UserID)
		}
	}

	// Если ни одно уведомление не отправилось успешно, возвращаем последнюю ошибку
	if successCount == 0 && lastError != nil {
		return lastError
	}

	return nil
}

func SendTelegramNotification(tId int64, event, userName, assistName, target string) error {
	var url string
	var client *http.Client

	if mode.ProductionMode {
		url = fmt.Sprintf("http://localhost:%s/notification", mode.CarpinteroPort)
		client = &http.Client{}
	} else {
		url = fmt.Sprintf("https://localhost:%s/notification", mode.CarpinteroPort)
		client = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	}

	payload := map[string]interface{}{
		"tid":    tId,
		"event":  event,
		"user":   userName,
		"assist": assistName,
		"target": target,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ошибка при преобразовании данных в JSON: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("ошибка при создании HTTP-запроса: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка при отправке HTTP-запроса: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("неожиданный статус ответа: %d, тело: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

func SendEmailNotification(email, event, userName, assistName, target string) error {
	// Формируем URL для webhook
	url := fmt.Sprintf("https://%s:%s/notification", mode.CarpinteroHost, mode.MailServerPort)

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
	logger.Info("Запуск 'NotificationListener' для прослушивания канала mode.CarpinteroCh")

	for {
		select {
		case msg, ok := <-mode.CarpinteroCh:
			if !ok {
				logger.Error("mode.CarpinteroCh closed")
				return
			}
			err := e.SendNotification(msg)
			if err != nil {
				logger.Error("'NotificationListener': ошибка отправки уведомления: %v", err, msg.UserID)
			}
		}
	}
}
