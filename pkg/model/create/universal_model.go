package create

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/ikermy/air_common/pkg/mode"
	"github.com/ikermy/air_common/pkg/model/commdom"
)

// New creates the provider-aware UniversalModel facade.
func New(ctx context.Context, db DB) *UniversalModel {
	m := &UniversalModel{
		ctx: ctx,
		db:  db,
	}

	// Инициализируем OpenAI клиент БЕЗ глобального ключа — глобальные ключи из конфига
	// должны игнорироваться полностью. Персональный ключ читается из БД через keyResolver.
	m.openaiClient = &OpenAIAgentClient{
		url: mode.OpenAIAgentsURL,
		ctx: ctx,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		universalModel: m, // Передаем ссылку на universalModel
	}
	m.openaiClient.SetKeyResolver(func(userID uint32) string {
		if key, err := db.GetUserAPIKey(userID, commdom.ProviderOpenAI); err == nil {
			return key
		}
		return ""
	})

	// Инициализируем Mistral клиент БЕЗ глобального ключа — глобальные ключи из конфига
	// должны игнорироваться полностью. Персональный ключ читается из БД через keyResolver.
	m.mistralClient = &MistralAgentClient{
		url:            mode.MistralAgentsURL,
		ctx:            ctx,
		universalModel: m,
	}
	m.mistralClient.SetKeyResolver(func(userID uint32) string {
		if key, err := db.GetUserAPIKey(userID, commdom.ProviderMistral); err == nil {
			return key
		}
		return ""
	})

	// Инициализируем google клиент БЕЗ глобального ключа — глобальные ключи из конфига
	// должны игнорироваться полностью. Персональный ключ читается из БД через keyResolver.
	m.googleClient = &GoogleAgentClient{
		url:            mode.GoogleAgentsURL,
		ctx:            ctx,
		universalModel: m,
	}
	m.googleClient.SetKeyResolver(func(userID uint32) string {
		if key, err := db.GetUserAPIKey(userID, commdom.ProviderGoogle); err == nil {
			return key
		}
		return ""
	})

	return m
}

// CreateModel creates provider resources and returns the database references.
func (m *UniversalModel) CreateModel(userID uint32, provider commdom.ProviderType, modelData *commdom.UniversalModelData, fileIDs []commdom.Ids) (commdom.UMCR, error) {
	if modelData == nil {
		return commdom.UMCR{}, fmt.Errorf("modelData не может быть nil")
	}

	if modelData.UseModelName == nil {
		return commdom.UMCR{}, fmt.Errorf("modelData.UseModelName не может быть пустым")
	}

	switch provider {
	case commdom.ProviderOpenAI:
		return m.createModel(userID, modelData, fileIDs)
	case commdom.ProviderMistral:
		return m.createMistralModel(userID, modelData, fileIDs)
	case commdom.ProviderGoogle:
		return m.createGoogleModel(userID, modelData, fileIDs)
	default:
		return commdom.UMCR{}, fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// SaveModel сохраняет модель в БД в универсальном формате
// Работает для любого провайдера (OpenAI, Mistral..)
// Автоматически устанавливает модель как активную если это первая модель пользователя
func (m *UniversalModel) SaveModel(userID uint32, umcr commdom.UMCR, data *commdom.UniversalModelData) error {
	if data == nil {
		return fmt.Errorf("не указана модель провайдера")
	}
	if data.UseModelName == nil {
		data.UseModelName = &commdom.UseModelName{}
	}

	// При частичном обновлении клиент может прислать только часть UseModelName.
	// Восстанавливаем отсутствующие ссылки на модели из актуальных данных БД.
	if data.UseModelName.GptType == nil || data.UseModelName.GptType.ID == 0 || data.UseModelName.Realtime == nil || data.UseModelName.Realtime.ID == 0 {
		existingModels, lookupErr := m.db.GetAllUserModels(userID)
		if lookupErr == nil {
			for _, existing := range existingModels {
				if existing.Provider != umcr.Provider {
					continue
				}
				if (data.UseModelName.GptType == nil || data.UseModelName.GptType.ID == 0) && existing.GptType != nil && existing.GptType.ID != 0 {
					data.UseModelName.GptType = existing.GptType
				}
				if (data.UseModelName.Realtime == nil || data.UseModelName.Realtime.ID == 0) && existing.Realtime != nil && existing.Realtime.ID != 0 {
					data.UseModelName.Realtime = existing.Realtime
				}
				break
			}
		}
	}

	// При обновлении конфигурации клиент может прислать только имя модели.
	// Model в user_gpt — это FK на gpt_models.Id, поэтому нельзя сохранять ID=0.
	// Восстанавливаем ID из уже существующей записи для любого провайдера.
	if data.UseModelName.GptType == nil || data.UseModelName.GptType.ID == 0 {
		return fmt.Errorf("не указан корректный ID модели gpt_models для провайдера %s", umcr.Provider)
	}
	if data.UseModelName.Realtime == nil || data.UseModelName.Realtime.ID == 0 {
		return fmt.Errorf("не указан корректный ID realtime-модели для провайдера %s", umcr.Provider)
	}
	if data.RealtimeVAD != nil && data.RealtimeVAD.Mistral != nil && data.RealtimeVAD.Mistral.VoiceClone != nil {
		if err := data.RealtimeVAD.Mistral.VoiceClone.Validate(); err != nil {
			return fmt.Errorf("некорректная конфигурация voice cloning: %w", err)
		}
	}

	compressed, err := compressModelData(data)
	if err != nil {
		return err
	}

	err = m.db.SaveUserModel(
		userID,
		umcr.Provider,
		data.Name,
		umcr.AssistID,
		compressed,
		commdom.DefaultProvidersModels{
			GeneralModelID:  data.UseModelName.GptType.ID,
			RealTimeModelID: data.UseModelName.Realtime.ID,
		},
		umcr.AllIds,
		data.Operator,
	)
	if err != nil {
		return fmt.Errorf("ошибка сохранения модели в БД: %w", err)
	}

	return nil
}

// SetMistralMCPFetchers устанавливает MCP-fetchers на mistralClient.
// Вызывается из mistral/model.go после инициализации UniversalModel.
func (m *UniversalModel) SetMistralMCPFetchers(promptFetcher GooglePromptHintFetcher, toolsFetcher GoogleFunctionDeclarationsFetcher) {
	if m.mistralClient != nil {
		m.mistralClient.SetMCPConfigFetchers(promptFetcher, toolsFetcher)
	}
}
