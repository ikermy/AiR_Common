package model

import models "github.com/ikermy/AiR_Common/pkg/model/create"

// Экспортируем константы провайдеров из пакета create для удобства использования
const (
	// CreateProviderOpenAI - константа для провайдера OpenAI (строковое значение)
	CreateProviderOpenAI = models.ProviderOpenAI

	// CreateProviderMistral - константа для провайдера Mistral (строковое значение)
	CreateProviderMistral = models.ProviderMistral
)

// ConvertProviderType конвертирует ProviderType (uint8) в CreateProviderType (string)
func ConvertProviderType(p ProviderType) CreateProviderType {
	return CreateProviderType(p.String())
}

// ConvertCreateProviderType конвертирует CreateProviderType (string) в ProviderType (uint8)
func ConvertCreateProviderType(p CreateProviderType) ProviderType {
	switch p {
	case CreateProviderOpenAI:
		return ProviderOpenAI
	case CreateProviderMistral:
		return ProviderMistral
	default:
		return 0 // Unknown
	}
}
