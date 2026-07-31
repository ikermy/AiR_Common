package mistral

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

func TestValidateReferenceAudioPCM(t *testing.T) {
	policy := ReferenceAudioPolicy{MinDuration: time.Second, MaxDuration: 3 * time.Second, Formats: []string{"pcm_s16le"}}
	pcm := make([]byte, 16000*2*2)
	if err := ValidateReferenceAudio(pcm, "pcm_s16le", 16000, 1, policy); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReferenceAudio(pcm[:100], "pcm_s16le", 16000, 1, policy); err == nil {
		t.Fatal("expected short PCM error")
	}
}

func TestValidateReferenceAudioWAV(t *testing.T) {
	wav := make([]byte, 44+32000)
	copy(wav[:4], "RIFF")
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], 1)
	binary.LittleEndian.PutUint32(wav[24:28], 16000)
	binary.LittleEndian.PutUint16(wav[34:36], 16)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], 32000)
	if err := ValidateReferenceAudio(wav, "wav", 0, 0, ReferenceAudioPolicy{MinDuration: time.Second, MaxDuration: 3 * time.Second, Formats: []string{"wav"}}); err != nil {
		t.Fatal(err)
	}
}

type cleanupRecorder struct {
	voices  []string
	samples []string
}

func (c *cleanupRecorder) DeleteVoice(_ context.Context, _ uint32, id string) error {
	c.voices = append(c.voices, id)
	return nil
}
func (c *cleanupRecorder) DeleteReferenceAudio(_ context.Context, _ uint32, id string) error {
	c.samples = append(c.samples, id)
	return nil
}

func TestVoiceCleanupJobRemovesExternalResources(t *testing.T) {
	recorder := &cleanupRecorder{}
	err := (VoiceCleanupJob{UserID: 7, ModelID: 11, VoiceID: "voice-1", ReferenceAudioID: "audio-1"}).Run(context.Background(), recorder)
	if err != nil {
		t.Fatal(err)
	}
	if len(recorder.voices) != 1 || len(recorder.samples) != 1 {
		t.Fatalf("cleanup was incomplete: %+v", recorder)
	}
}
