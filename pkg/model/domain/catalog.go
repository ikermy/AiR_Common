package domain

type DefaultProvidersModels struct {
	GeneralModelID    uint
	GeneralModelName  string
	RealTimeModelID   uint
	RealTimeModelName string
}
type ProviderModelUserChange struct {
	UserID    uint32 `json:"user_id"`
	ModelID   uint64 `json:"model_id"`
	ModelName string `json:"model_name,omitempty"`
}
type ProviderModel struct {
	ID       uint64 `json:"Id"`
	Name     string `json:"Name"`
	Category string `json:"category,omitempty"`
}

const (
	ProviderModelGeneral  = "general"
	ProviderModelRealtime = "realtime"
	ProviderModelSTT      = "stt"
	ProviderModelTTS      = "tts"
)

type ProviderModelsSyncResult struct {
	Provider      ProviderType              `json:"provider"`
	Models        []ProviderModel           `json:"models,omitempty"`
	Synced        int                       `json:"synced"`
	Removed       int                       `json:"removed"`
	ClearedUsers  int                       `json:"cleared_users"`
	RemovedNames  []string                  `json:"removed_names,omitempty"`
	AffectedUsers []ProviderModelUserChange `json:"affected_users,omitempty"`
}
