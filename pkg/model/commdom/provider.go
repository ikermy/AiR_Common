package commdom

import "fmt"

type ProviderType uint8

const (
	ProviderOpenAI  ProviderType = 1
	ProviderMistral ProviderType = 2
	ProviderGoogle  ProviderType = 3
)

var AllProviders = []ProviderType{ProviderOpenAI, ProviderMistral, ProviderGoogle}

func (p ProviderType) String() string {
	switch p {
	case ProviderOpenAI:
		return "openai"
	case ProviderMistral:
		return "mistral"
	case ProviderGoogle:
		return "google"
	default:
		return "unknown"
	}
}
func FromString(s string) (ProviderType, error) {
	switch s {
	case "openai":
		return ProviderOpenAI, nil
	case "mistral":
		return ProviderMistral, nil
	case "google":
		return ProviderGoogle, nil
	default:
		return 0, fmt.Errorf("неизвестный провайдер: %s", s)
	}
}
func (p ProviderType) FromUint8(value uint8) ProviderType { return ProviderType(value) }
func (p ProviderType) IsValid() bool {
	for _, known := range AllProviders {
		if p == known {
			return true
		}
	}
	return false
}

type ModelType uint8

const (
	General  ModelType = 1
	RealTime ModelType = 2
)

func (m ModelType) IsRealtime() bool { return m == RealTime }
func (m ModelType) IsGeneral() bool  { return m == General }
func ModelTypeFromString(s string) (ModelType, error) {
	switch s {
	case "general":
		return General, nil
	case "realtime":
		return RealTime, nil
	default:
		return 0, fmt.Errorf("неизвестный тип модели: %s", s)
	}
}

type Union struct {
	Provider  ProviderType
	ModelType ModelType
}
