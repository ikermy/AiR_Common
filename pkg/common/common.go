package common

import (
	"encoding/json"
	"fmt"
	"time"
)

// InstMsg - структура для передачи мгновенных сообщений в панель управления
type InstMsg struct {
	UID uint32
	Msg string
}

// CarpCh - канал для передачи уведомлений
type CarpCh struct {
	Event      string
	UserName   string
	AssistName string
	Target     string
	UserID     uint32
}

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
	Message string
	Code    ErrorCode
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
		EndDate      time.Time `json:"-"`
		EndDateStr   string    `json:"EndDate"`
		Balance      float64   `json:"balance"`
		MessageCost  float64   `json:"MessageCost"`
		MessageLimit int       `json:"MessageLimit"`
		MessagesUsed int       `json:"MessagesUsed"`
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
