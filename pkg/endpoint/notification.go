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
	"time"
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

// Структура для информации о инициации платежа
type PaymentInfo struct {
	UserId    int    `json:"userId"`
	Currency  string `json:"currency"`
	Amount    int    `json:"amount"`
	AmountUsd int    `json:"amountUsd"`
	OrderId   string `json:"orderId"`
	Network   string `json:"network"`
	ExpiresAt int64  `json:"expiresAt"`
}

// Структура для информации о статусе платежа
type PaymentStatus struct {
	OrderID        string  `json:"orderId"`
	UserID         uint32  `json:"userId"`
	Status         string  `json:"status"`
	Currency       string  `json:"currency"`
	Network        string  `json:"network"`
	Amount         float64 `json:"amount"`
	AmountUsd      float64 `json:"amountUsd"`
	ReceivedAmount float64 `json:"receivedAmount"`
	TxHash         string  `json:"txHash"`
	Confirmations  int     `json:"confirmations"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
	ExpiresAt      string  `json:"expiresAt"`
}

func CreateMessageFromEvent(Event, UserName, AssistName, Target string) (string, error) {
	var msg, payment string

	if AssistName != "init" && Event == "usdt_pay" {
		var paritalInfo PaymentStatus
		err := json.Unmarshal([]byte(Target), &paritalInfo)
		if err != nil {
			return "", fmt.Errorf("ошибка парсинга PaymentStatus: %v", err)
		}

		layout := "2006-01-02 15:04:05"
		createdAt, err1 := time.Parse(layout, paritalInfo.CreatedAt)
		updatedAt, err2 := time.Parse(layout, paritalInfo.UpdatedAt)
		expiresAt, err3 := time.Parse(layout, paritalInfo.ExpiresAt)

		formatOrRaw := func(t time.Time, err error, raw string) string {
			if err != nil {
				return raw
			}
			return t.Format("02.01.2006 15:04:05")
		}

		payment = fmt.Sprintf(
			"	Статус: %s\n	Валюта: %s\n	Сумма: %.2f\n	Сумма в USD: %.2f\n	Поступление: %.2f\n Номер заказа: %s\n	Сеть: %s\n	Хэш транзакции: %s\n	Подтверждения: %d\n	Создано: %s\n	Обновлено: %s\n	Срок действия: %s",
			paritalInfo.Status,
			paritalInfo.Currency,
			paritalInfo.Amount,
			paritalInfo.AmountUsd,
			paritalInfo.ReceivedAmount,
			paritalInfo.OrderID,
			paritalInfo.Network,
			paritalInfo.TxHash,
			paritalInfo.Confirmations,
			formatOrRaw(createdAt, err1, paritalInfo.CreatedAt),
			formatOrRaw(updatedAt, err2, paritalInfo.UpdatedAt),
			formatOrRaw(expiresAt, err3, paritalInfo.ExpiresAt),
		)
	}
	switch Event {
	// События оплаты подписки
	case "usdt_pay":

		switch AssistName {
		case "init":
			// Для инициализации платежа своя структура
			var (
				answer  string
				payInfo PaymentInfo
			)
			err := json.Unmarshal([]byte(Target), &payInfo)
			if err != nil {
				logger.Error("Ошибка парсинга PaymentInfo: %v", err)
			}

			if UserName == "false" {
				answer = "новый платёж"
			} else {
				answer = "активный платёж"
			}

			expiresAt := int64(1755884144)
			t := time.Unix(expiresAt, 0)

			pending := fmt.Sprintf(
				"	Статус: %s\n	Валюта: %s\n	Сумма: %d\n	Сумма в USD: %d\n	Номер заказа: %s\n	Сеть: %s\n	Срок действия: %s",
				answer,
				payInfo.Currency,
				payInfo.Amount,
				payInfo.AmountUsd,
				payInfo.OrderId,
				payInfo.Network,
				t.Format("02.01.2006 15:04:05"),
			)

			msg = fmt.Sprintf("Сформированн счёт для оплаты подписки:\n%s", pending)
		case "pending":
			msg = fmt.Sprintf("Инициирована оплата подписки:\n%s", payment)
		case "partial":
			msg = fmt.Sprintf("Частичная оплата подписки:\n%s", payment)
		case "confirmed":
			msg = fmt.Sprintf("Подтверждена оплата подписки:\n%s", payment)
		case "failed":
			msg = fmt.Sprintf("Ошибка оплаты подписки:\n%s", payment)
		default:
			return "", fmt.Errorf("Неизвестное событие pay:\n%s", AssistName)
		}

	// События диалога с ассистентом
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
