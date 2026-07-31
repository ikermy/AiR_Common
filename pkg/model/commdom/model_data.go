package commdom

type UniversalModelData struct {
	Name         string        `json:"name"`
	Prompt       string        `json:"prompt"`
	MetaAction   string        `json:"mact"`
	Triggers     []string      `json:"trig"`
	FileIds      []Ids         `json:"fileIds"`
	VecIds       VecIds        `json:"vecIds"`
	Operator     bool          `json:"operator"`
	Search       bool          `json:"search"`
	Interpreter  bool          `json:"interpreter"`
	S3           bool          `json:"s3"`
	Haunter      bool          `json:"haunter"`
	Image        bool          `json:"image"`
	WebSearch    bool          `json:"web_search"`
	Realtime     bool          `json:"realtime"`
	RealtimeVAD  *RealtimeVAD  `json:"realtime_vad,omitempty"`
	Video        bool          `json:"video"`
	GOAuth       GOAuth        `json:"g_oauth"`
	Espero       EsperoConfig  `json:"espero"`
	UseModelName *UseModelName `json:"use_model_name"`
	Provider     ProviderType  `json:"provider"`
}

type GptType struct {
	Name string `json:"name"`
	ID   uint   `json:"id"`
}
type Realtime struct {
	Name string `json:"name"`
	ID   uint   `json:"id"`
}
type UseModelName struct {
	GptType  *GptType  `json:"gpttype"`
	Realtime *Realtime `json:"realtime"`
}

type GOAuth struct {
	Calendar bool `json:"calendar"`
	Sheets   bool `json:"sheets"`
}

func (g GOAuth) Enabled() bool { return g.Calendar || g.Sheets }

type EsperoConfig struct {
	Limit  uint16 `json:"limit"`
	Wait   uint8  `json:"wait"`
	Ignore bool   `json:"ignore"`
}

// UserModelsResponse представляет ответ со всеми моделями пользователя
type UserModelsResponse struct {
	Models         map[string]*UniversalModelData `json:"models"`          // Модели по провайдерам ("openai", "mistral")
	ActiveProvider string                         `json:"active_provider"` // Активный провайдер
}
