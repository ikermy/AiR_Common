package crm

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func BenchmarkFprintf(b *testing.B) {
	for i := 0; i < b.N; i++ {
		var sb strings.Builder
		fmt.Fprintf(&sb, "[%s] %s %d", "Meta", "Text", i)
		_ = sb.String()
	}
}

func BenchmarkWriteString(b *testing.B) {
	for i := 0; i < b.N; i++ {
		var sb strings.Builder
		sb.WriteString("[")
		sb.WriteString("Meta")
		sb.WriteString("] ")
		sb.WriteString("Text ")
		sb.WriteString(strconv.Itoa(i))
		_ = sb.String()
	}
}

func BenchmarkConcat(b *testing.B) {
	for i := 0; i < b.N; i++ {
		// простая конкатенация через +
		s := "[" + "Meta" + "] " + "Text " + strconv.Itoa(i)
		_ = s
	}
}
