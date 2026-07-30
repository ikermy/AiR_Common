package mistral

import "testing"

func TestTurnGuardInvalidatesPreviousTurn(t *testing.T) {
	var guard TurnGuard
	first := guard.Begin()
	if !guard.IsCurrent(first) {
		t.Fatal("first turn should be current")
	}
	second := guard.Begin()
	if guard.IsCurrent(first) {
		t.Fatal("previous turn must be invalidated")
	}
	if !guard.IsCurrent(second) {
		t.Fatal("second turn should be current")
	}
}

func TestSentenceChunkerUsesPunctuationAndFlush(t *testing.T) {
	var chunker SentenceChunker
	if got := chunker.Push("Это достаточно длинная короткая фраза. "); len(got) != 1 || got[0] != "Это достаточно длинная короткая фраза." {
		t.Fatalf("Push() = %q, want one completed sentence", got)
	}
	got := chunker.Push("Остаток")
	if len(got) != 0 {
		t.Fatalf("Push() returned unfinished text: %q", got)
	}
	got = chunker.Flush()
	if len(got) != 1 || got[0] != "Остаток" {
		t.Fatalf("Flush() = %q, want остаток", got)
	}
}

func TestSentenceChunkerHardLimitSplitsLongText(t *testing.T) {
	var chunker SentenceChunker
	text := "слово слово слово слово слово слово слово слово слово слово слово слово слово слово слово слово слово слово слово слово слово слово слово слово"
	chunks := chunker.Push(text)
	if len(chunks) == 0 {
		t.Fatal("long text should produce a chunk at the hard limit")
	}
	for _, chunk := range chunks {
		if len([]rune(chunk)) > hardSentenceLimit {
			t.Fatalf("chunk length = %d, want <= %d", len([]rune(chunk)), hardSentenceLimit)
		}
	}
}
