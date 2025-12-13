package model

// ProviderType определяет тип провайдера модели
type ProviderType uint8

const (
	// ProviderOpenAI представляет провайдера OpenAI
	ProviderOpenAI ProviderType = iota + 1
	// ProviderMistral представляет провайдера Mistral
	ProviderMistral
)

// String возвращает строковое представление типа провайдера
func (p ProviderType) String() string {
	switch p {
	case ProviderOpenAI:
		return "openai"
	case ProviderMistral:
		return "mistral"
	default:
		return "unknown"
	}
}

// IsValid проверяет, является ли тип провайдера валидным
func (p ProviderType) IsValid() bool {
	return p == ProviderOpenAI || p == ProviderMistral
}
