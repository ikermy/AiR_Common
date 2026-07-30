package mistral

import (
	"strings"
	"sync/atomic"
	"unicode"
)

const (
	softSentenceLimit = 20
	hardSentenceLimit = 100
)

// TurnGuard invalidates work belonging to an older user turn.
// It prevents late LLM/TTS goroutines from writing audio after an interruption.
type TurnGuard struct {
	current atomic.Uint64
}

// Begin starts a new turn and returns its ID.
func (g *TurnGuard) Begin() uint64 {
	return g.current.Add(1)
}

// Current returns the currently active turn ID.
func (g *TurnGuard) Current() uint64 {
	return g.current.Load()
}

// IsCurrent reports whether turnID is still allowed to publish output.
func (g *TurnGuard) IsCurrent(turnID uint64) bool {
	return turnID != 0 && turnID == g.current.Load()
}

// Invalidate cancels the current turn without starting a replacement turn.
func (g *TurnGuard) Invalidate() uint64 {
	return g.current.Add(1)
}

// SentenceChunker splits streamed LLM text into TTS-sized chunks.
// It prefers punctuation after a small minimum and enforces a hard length limit.
type SentenceChunker struct {
	buffer strings.Builder
}

// Push appends a text delta and returns chunks ready for TTS.
func (c *SentenceChunker) Push(delta string) []string {
	if delta == "" {
		return nil
	}
	c.buffer.WriteString(delta)
	return c.take(false)
}

// Flush returns the remaining text as the final chunk.
func (c *SentenceChunker) Flush() []string {
	return c.take(true)
}

func (c *SentenceChunker) take(flush bool) []string {
	text := c.buffer.String()
	var chunks []string
	for len([]rune(text)) > 0 {
		runes := []rune(text)
		cut := -1
		for i, r := range runes {
			if i+1 < softSentenceLimit {
				continue
			}
			if isSentenceBoundary(r) {
				cut = i + 1
				break
			}
		}
		if cut == -1 && len(runes) > hardSentenceLimit {
			cut = hardSentenceLimit
			for i := hardSentenceLimit; i > softSentenceLimit; i-- {
				if unicode.IsSpace(runes[i-1]) {
					cut = i
					break
				}
			}
		}
		if cut == -1 {
			if !flush {
				break
			}
			cut = len(runes)
		}

		chunk := strings.TrimSpace(string(runes[:cut]))
		text = strings.TrimSpace(string(runes[cut:]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
	}
	c.buffer.Reset()
	c.buffer.WriteString(text)
	return chunks
}

func isSentenceBoundary(r rune) bool {
	switch r {
	case '.', '!', '?', ';', ',', '\n':
		return true
	default:
		return false
	}
}
