package mistral

import "context"

// VoiceResourceCleanup is implemented by the application orchestration layer.
// Cleanup must be scheduled after the model deletion is committed (preferably
// through a transactional outbox), not executed inside a SQL transaction.
type VoiceResourceCleanup interface {
	DeleteVoice(ctx context.Context, userID uint32, voiceID string) error
	DeleteReferenceAudio(ctx context.Context, userID uint32, referenceAudioID string) error
}

// VoiceCleanupJob describes external resources that must be removed after a
// user model is deleted. It is safe to persist this value in an outbox and
// retry it until both operations succeed.
type VoiceCleanupJob struct {
	UserID           uint32 `json:"user_id"`
	ModelID          uint32 `json:"model_id"`
	VoiceID          string `json:"voice_id,omitempty"`
	ReferenceAudioID string `json:"reference_audio_id,omitempty"`
}

func (j VoiceCleanupJob) Run(ctx context.Context, cleanup VoiceResourceCleanup) error {
	if cleanup == nil {
		return nil
	}
	if j.VoiceID != "" {
		if err := cleanup.DeleteVoice(ctx, j.UserID, j.VoiceID); err != nil {
			return err
		}
	}
	if j.ReferenceAudioID != "" {
		if err := cleanup.DeleteReferenceAudio(ctx, j.UserID, j.ReferenceAudioID); err != nil {
			return err
		}
	}
	return nil
}
