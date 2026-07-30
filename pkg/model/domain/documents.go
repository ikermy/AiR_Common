package domain

type DocumentMetadata struct {
	Source    string `json:"source,omitempty"`
	FileName  string `json:"file_name,omitempty"`
	FileID    string `json:"file_id,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Tags      string `json:"tags,omitempty"`
	Category  string `json:"category,omitempty"`
	Custom    string `json:"custom,omitempty"`
}
type VectorDocument struct {
	ID        string           `json:"id"`
	UserID    uint32           `json:"user_id"`
	Name      string           `json:"name"`
	Content   string           `json:"content"`
	Embedding []float32        `json:"embedding"`
	Metadata  DocumentMetadata `json:"metadata,omitempty"`
	CreatedAt any              `json:"created_at"`
}
type ProvidersAvailability struct {
	Available   []string `json:"available"`
	Unavailable []string `json:"unavailable"`
}
