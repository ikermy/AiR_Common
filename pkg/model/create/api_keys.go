package create

import "github.com/ikermy/AiR_Common/pkg/model/domain"

// ProvidersWithApiKeys проверяет наличие пользовательских ключей у всех
// зарегистрированных провайдеров.
func (m *UniversalModel) ProvidersWithApiKeys(userID uint32) domain.ProvidersAvailability {
	result := domain.ProvidersAvailability{Available: make([]string, 0), Unavailable: make([]string, 0)}
	for _, p := range domain.AllProviders {
		key, err := m.db.GetUserAPIKey(userID, p)
		if err == nil && key != "" {
			result.Available = append(result.Available, p.String())
		} else {
			result.Unavailable = append(result.Unavailable, p.String())
		}
	}
	return result
}

func (m *UniversalModel) SetUserAPIKey(userID uint32, provider domain.ProviderType, key string) error {
	return m.db.SetUserAPIKey(userID, provider, key)
}
func (m *UniversalModel) GetUserAPIKey(userID uint32, provider domain.ProviderType) (string, error) {
	return m.db.GetUserAPIKey(userID, provider)
}
func (m *UniversalModel) DeleteUserAPIKey(userID uint32, provider domain.ProviderType) error {
	return m.db.DeleteUserAPIKey(userID, provider)
}
