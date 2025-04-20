package common

import (
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// CarpCh - канал для передачи уведомлений
type CarpCh struct {
	Event      string
	UserID     uint32
	UserName   string
	AssistName string
	Target     string
}

var CarpinteroCh = make(chan CarpCh, 1) // Канал для передачи уведомлений

// ErrorCode - константы кодов ошибок подписки
type ErrorCode int

const (
	ErrNoSubscription ErrorCode = iota + 1
	ErrSubscriptionExpired
	ErrMessageLimitExceeded // Не используется, можно реализовать разовое уведомление
	ErrInsufficientBalance
	ErrInvalidSubscriptionData
)

// SubscriptionError - структура для ошибок подписки
type SubscriptionError struct {
	Code    ErrorCode
	Message string
	UserID  uint32
}

// Error реализует интерфейс error
func (e *SubscriptionError) Error() string {
	return e.Message
}

// SubscriptionProvider интерфейс для получения данных подписки
type SubscriptionProvider interface {
	GetUserSubscriptionLimites(userId uint32) (json.RawMessage, error)
}

// CheckUserSubscription проверяет подписку пользователя
func CheckUserSubscription(provider SubscriptionProvider, userId uint32) error {
	subscription, err := provider.GetUserSubscriptionLimites(userId)
	if err != nil {
		return &SubscriptionError{
			Code:    ErrInvalidSubscriptionData,
			Message: fmt.Sprintf("ошибка получения лимитов подписки: %v", err),
			UserID:  userId,
		}
	}

	if subscription == nil {
		return &SubscriptionError{
			Code:    ErrNoSubscription,
			Message: "пользователь не имеет подписки",
			UserID:  userId,
		}
	}

	type UserSubscription struct {
		Balance      float64   `json:"balance"`
		MessageCost  float64   `json:"MessageCost"`
		EndDateStr   string    `json:"EndDate"`
		MessageLimit int       `json:"MessageLimit"`
		MessagesUsed int       `json:"MessagesUsed"`
		EndDate      time.Time `json:"-"`
	}

	var userSub UserSubscription
	if err := json.Unmarshal(subscription, &userSub); err != nil {
		return &SubscriptionError{
			Code:    ErrInvalidSubscriptionData,
			Message: fmt.Sprintf("ошибка парсинга данных подписки: %v", err),
			UserID:  userId,
		}
	}

	endDate, err := time.Parse("2006-01-02", userSub.EndDateStr)
	if err != nil {
		return &SubscriptionError{
			Code:    ErrInvalidSubscriptionData,
			Message: fmt.Sprintf("ошибка преобразования даты окончания подписки: %v", err),
			UserID:  userId,
		}
	}
	userSub.EndDate = endDate

	now := time.Now()
	if userSub.EndDate.Before(now) {
		return &SubscriptionError{
			Code:    ErrSubscriptionExpired,
			Message: fmt.Sprintf("подписка истекла %v", userSub.EndDate),
			UserID:  userId,
		}
	}

	if userSub.MessagesUsed >= userSub.MessageLimit {
		if userSub.Balance <= userSub.MessageCost {
			return &SubscriptionError{
				Code: ErrInsufficientBalance,
				Message: fmt.Sprintf("достигнут лимит сообщений (%d/%d) и недостаточен баланс: %f",
					userSub.MessagesUsed, userSub.MessageLimit, userSub.Balance),
				UserID: userId,
			}
		}
	}

	return nil
}

func SendEvent(userId uint32, event, userName, assistName, target string) {
	msg := CarpCh{
		UserID:     userId,
		Event:      event,
		UserName:   userName,
		AssistName: assistName,
		Target:     target,
	}

	select {
	case CarpinteroCh <- msg:
	default:
		log.Printf("CarpinteroCh: канал закрыт или переполнен, не удалось отправить сообщение: %+v", msg)
	}
}
