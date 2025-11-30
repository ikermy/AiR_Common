package endpoint

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/common"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
)

// sendHTTPRequest отправляет HTTP POST запрос с JSON payload
func sendHTTPRequest(url string, payload map[string]interface{}) error {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ошибка при преобразовании данных в JSON: %w", err)
	}

	if mode.ProductionMode {
		url = strings.Replace(url, "https://", "http://", 1)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("ошибка при создании HTTP-запроса: %w", err)
	}

	// Устанавливаем заголовки
	req.Header.Set("Content-Type", "application/json")

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

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("неожиданный статус ответа: %d, тело: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

func (e *Endpoint) SendEvent(userId uint32, event, userName, assistName, target string) {
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
		case "instant":
			err := SendInstantNotification(msg.UserID, msg.Event, msg.UserName, msg.AssistName, msg.Target)
			if err != nil {
				logger.Error("Ошибка отправки Instant уведомления: %v", err, msg.UserID)
				lastError = err
				continue
			}
			successCount++

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
			err = SendTelegramNotification(msg.UserID, tId, msg.Event, msg.UserName, msg.AssistName, msg.Target)
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

func SendTelegramNotification(uid uint32, tId int64, event, userName, assistName, target string) error {
	// Добавить userID для возможности смены языка уведомлений
	url := fmt.Sprintf("https://localhost:%s/notification/telega", mode.MailServerPort)

	payload := map[string]interface{}{
		"uid":    uid,
		"tid":    tId,
		"event":  event,
		"user":   userName,
		"assist": assistName,
		"target": target,
	}

	return sendHTTPRequest(url, payload)
}

func SendEmailNotification(uid uint32, email, event, userName, assistName, target string) error {
	// Добавить userID для возможности смены языка уведомлений
	url := fmt.Sprintf("https://localhost:%s/notification/mail", mode.MailServerPort)

	payload := map[string]interface{}{
		"uid":    uid,
		"email":  email,
		"event":  event,
		"user":   userName,
		"assist": assistName,
		"target": target,
	}

	return sendHTTPRequest(url, payload)
}

func SendInstantNotification(uid uint32, event, userName, assistName, target string) error {
	// Добавить userID для возможности смены языка уведомлений
	url := fmt.Sprintf("https://localhost:%s/notification/instant", mode.MailServerPort)

	payload := map[string]interface{}{
		"uid":    uid,
		"event":  event,
		"user":   userName,
		"assist": assistName,
		"target": target,
	}

	return sendHTTPRequest(url, payload)
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

// CreateMessageFromEvent создает сообщение на основе события
func CreateMessageFromEvent(Event, UserName, AssistName, Target string) (string, error) {
	// Добавить userID для возможности смены языка уведомлений
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
	case "model-operator":
		msg = fmt.Sprintf("Ассистент %s запросил переключение на оператора в диалоге с пользователем %s", AssistName, UserName)
	// События подписки
	case "subscription":
		errMsg := map[common.ErrorCode]string{
			common.ErrNoSubscription:       "У вас нет подписки. Пожалуйста, оформите подписку.",
			common.ErrSubscriptionExpired:  "Ваша подписка истекла. Пожалуйста, продлите подписку.",
			common.ErrMessageLimitExceeded: "Вы превысили лимит сообщений. Пожалуйста, пополните баланс.",
			common.ErrInsufficientBalance:  "Недостаточно средств на балансе. Пожалуйста, пополните баланс.",
		}
		errorCode, _ := strconv.Atoi(Target)
		msg = errMsg[common.ErrorCode(errorCode)]
		// Разбан ботов для service lead generation
	case "lead-botunban":
		msg = fmt.Sprintf("Боты:\n%s\nразблокированны по таймеру, попробуйте их снова использовать", Target)
	case "lead-start":
		msg = fmt.Sprintf("Поиск лидов запущен:\n-всего контактов для обработки %s", Target)
	case "lead-stop":
		msg = fmt.Sprintf("Поиск лидов завершён:\n-всего контактов %s\n-обработанно %s", Target, AssistName)
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
