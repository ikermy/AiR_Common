package domain

type Ids struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}
type VecIds struct {
	FileIds  []Ids    `json:"FileIds"`
	VectorId []string `json:"VectorId"`
}
type UserModelRecord struct {
	ModelId  uint64       `json:"model_id"`
	Provider ProviderType `json:"provider"`
	IsActive bool         `json:"is_active"`
	AssistId string       `json:"assist_id"`
	GptType  *GptType     `json:"gpttype"`
	Realtime *Realtime    `json:"realtime"`
	FileIds  []Ids        `json:"file_ids"`
	AllIds   []byte       `json:"all_ids"`
}
type UMCR struct {
	AssistID string       `json:"assist_id"`
	AllIds   []byte       `json:"all_ids"`
	Provider ProviderType `json:"provider"`
}
