package logger

import (
	"fmt"
	"strings"
	"testing"
)

// старая версия
func logMessageConcatOld(args ...interface{}) string {
	var parts []string
	for _, arg := range args {
		parts = append(parts, fmt.Sprintf("%v", arg))
	}
	return strings.Join(parts, " ")
}

// новая версия (Builder)
func logMessageConcatNew(args ...interface{}) string {
	var sb strings.Builder
	for i, arg := range args {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(fmt.Sprint(arg))
	}
	return sb.String()
}

// чистая конкатенация через +
func logMessageConcatPlus(args ...interface{}) string {
	if len(args) == 0 {
		return ""
	}
	s := fmt.Sprint(args[0])
	for i := 1; i < len(args); i++ {
		s += " " + fmt.Sprint(args[i])
	}
	return s
}

func BenchmarkLogMessageConcatOld(b *testing.B) {
	args := []interface{}{"user", 42, true, "some text", 3.14}
	for i := 0; i < b.N; i++ {
		_ = logMessageConcatOld(args...)
	}
}

func BenchmarkLogMessageConcatNew(b *testing.B) {
	args := []interface{}{"user", 42, true, "some text", 3.14}
	for i := 0; i < b.N; i++ {
		_ = logMessageConcatNew(args...)
	}
}

func BenchmarkLogMessageConcatPlus(b *testing.B) {
	args := []interface{}{"user", 42, true, "some text", 3.14}
	for i := 0; i < b.N; i++ {
		_ = logMessageConcatPlus(args...)
	}
}

// go test -bench=BenchmarkLogMessageConcat -benchmem -run=^$
