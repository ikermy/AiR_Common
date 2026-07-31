package mistral

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// ReferenceAudioPolicy controls optional local validation before sending a
// voice sample to Mistral. It is intentionally configurable because provider
// limits can change independently from the library.
type ReferenceAudioPolicy struct {
	MinDuration time.Duration
	MaxDuration time.Duration
	MaxBytes    int
	Formats     []string
}

var DefaultReferenceAudioPolicy = ReferenceAudioPolicy{
	MinDuration: 2 * time.Second,
	MaxDuration: 10 * time.Second,
	MaxBytes:    25 * 1024 * 1024,
	Formats:     []string{"pcm_s16le", "wav", "mp3"},
}

// ValidateReferenceAudio validates raw sample bytes. WAV duration is read
// from its header; raw pcm_s16le duration requires SampleRate and Channels.
// MP3 is format-checked only because reliable duration parsing requires a
// full codec/frame parser and should not be silently approximated.
func ValidateReferenceAudio(audio []byte, format string, sampleRate, channels int, policy ReferenceAudioPolicy) error {
	if len(audio) == 0 {
		return fmt.Errorf("reference audio is empty")
	}
	if policy.MaxBytes > 0 && len(audio) > policy.MaxBytes {
		return fmt.Errorf("reference audio exceeds %d bytes", policy.MaxBytes)
	}
	format = strings.ToLower(strings.TrimSpace(format))
	if !allowedAudioFormat(format, policy.Formats) {
		return fmt.Errorf("unsupported reference audio format %q", format)
	}
	var duration time.Duration
	switch format {
	case "pcm_s16le":
		if sampleRate <= 0 || channels <= 0 {
			return fmt.Errorf("sample rate and channels are required for pcm_s16le")
		}
		bytesPerSecond := sampleRate * channels * 2
		duration = time.Duration(len(audio)) * time.Second / time.Duration(bytesPerSecond)
	case "wav":
		d, err := wavDuration(audio)
		if err != nil {
			return err
		}
		duration = d
	case "mp3":
		// Container validation only; Mistral remains authoritative for duration.
		if len(audio) < 2 || !(audio[0] == 0xff && audio[1]&0xe0 == 0xe0) {
			return fmt.Errorf("invalid mp3 header")
		}
		return nil
	}
	if policy.MinDuration > 0 && duration < policy.MinDuration {
		return fmt.Errorf("reference audio is shorter than %s", policy.MinDuration)
	}
	if policy.MaxDuration > 0 && duration > policy.MaxDuration {
		return fmt.Errorf("reference audio is longer than %s", policy.MaxDuration)
	}
	return nil
}

func ValidateBase64ReferenceAudio(encoded, format string, sampleRate, channels int, policy ReferenceAudioPolicy) error {
	audio, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decode reference audio: %w", err)
	}
	return ValidateReferenceAudio(audio, format, sampleRate, channels, policy)
}

func allowedAudioFormat(format string, formats []string) bool {
	for _, candidate := range formats {
		if strings.EqualFold(candidate, format) {
			return true
		}
	}
	return false
}

func wavDuration(data []byte) (time.Duration, error) {
	if len(data) < 44 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return 0, fmt.Errorf("invalid wav header")
	}
	var sampleRate, channels, bits uint32
	var dataSize uint32
	for offset := 12; offset+8 <= len(data); {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		if start+size > len(data) {
			return 0, fmt.Errorf("truncated wav chunk")
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return 0, fmt.Errorf("invalid wav fmt chunk")
			}
			channels = uint32(binary.LittleEndian.Uint16(data[start+2 : start+4]))
			sampleRate = binary.LittleEndian.Uint32(data[start+4 : start+8])
			bits = uint32(binary.LittleEndian.Uint16(data[start+14 : start+16]))
		case "data":
			dataSize = uint32(size)
		}
		offset = start + size
		if offset%2 != 0 {
			offset++
		}
	}
	if sampleRate == 0 || channels == 0 || bits != 16 || dataSize == 0 {
		return 0, fmt.Errorf("unsupported wav format")
	}
	return time.Duration(dataSize) * time.Second / time.Duration(sampleRate*channels*(bits/8)), nil
}
