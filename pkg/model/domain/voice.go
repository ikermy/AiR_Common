package domain

// Voice is provider-neutral metadata for a preset or custom voice.
type Voice struct {
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	Age             *int     `json:"age,omitempty"`
	Color           *string  `json:"color,omitempty"`
	CreatedAt       string   `json:"created_at,omitempty"`
	Description     *string  `json:"description,omitempty"`
	Gender          *string  `json:"gender,omitempty"`
	Languages       []string `json:"languages,omitempty"`
	RetentionNotice int      `json:"retention_notice,omitempty"`
	Slug            *string  `json:"slug,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	TrimmedSeconds  *float64 `json:"trimmed_seconds,omitempty"`
	UserID          *string  `json:"user_id,omitempty"`
}

type VoiceList struct {
	Items      []Voice `json:"items"`
	Page       int     `json:"page"`
	PageSize   int     `json:"page_size"`
	Total      int     `json:"total"`
	TotalPages int     `json:"total_pages"`
}

type CreateVoiceRequest struct {
	Name            string   `json:"name"`
	SampleAudio     string   `json:"sample_audio"`
	SampleFilename  *string  `json:"sample_filename,omitempty"`
	Age             *int     `json:"age,omitempty"`
	Color           *string  `json:"color,omitempty"`
	Description     *string  `json:"description,omitempty"`
	Gender          *string  `json:"gender,omitempty"`
	Languages       []string `json:"languages,omitempty"`
	RetentionNotice int      `json:"retention_notice,omitempty"`
	Slug            *string  `json:"slug,omitempty"`
	Tags            []string `json:"tags,omitempty"`
}

type UpdateVoiceRequest struct {
	Name        *string  `json:"name,omitempty"`
	Age         *int     `json:"age,omitempty"`
	Description *string  `json:"description,omitempty"`
	Gender      *string  `json:"gender,omitempty"`
	Languages   []string `json:"languages,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}
