package logger

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// ANSI цветовые коды
const (
	ColorReset  = "\033[0m"
	ColorWhite  = ""         // INFO
	ColorRed    = "\033[31m" // ERROR
	ColorYellow = "\033[33m" // WARNING
	ColorGreen  = "\033[32m" // DEBUG
	ColorPurple = "\033[35m" // FATAL
)

var generalLogger *log.Logger

var levelColors = map[string]string{
	"[INFO]":    ColorWhite,
	"[ERROR]":   ColorRed,
	"[WARNING]": ColorYellow,
	"[DEBUG]":   ColorGreen,
	"[FATAL]":   ColorPurple,
}

// Set инициализирует логгер с ротацией файлов
func Set(path string) {
	logFile := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    1,
		MaxBackups: 3,
		MaxAge:     30,
		Compress:   true,
	}
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	generalLogger = log.New(multiWriter, "", 0)
}

// --- публичные методы ---
func Infoln(args ...interface{})               { logMessage("[INFO]", 2, args...) }
func Info(format string, args ...interface{})  { logMessagef(format, "[INFO]", 2, args...) }
func Error(format string, args ...interface{}) { logMessagef(format, "[ERROR]", 2, args...) }
func Warn(format string, args ...interface{})  { logMessagef(format, "[WARNING]", 2, args...) }
func Debug(format string, args ...interface{}) { logMessagef(format, "[DEBUG]", 2, args...) }
func Fatal(args ...interface{})                { logMessage("[FATAL]", 2, args...); os.Exit(1) }
func Fatalf(format string, args ...interface{}) {
	logMessagef(format, "[FATAL]", 2, args...)
	os.Exit(1)
}

// --- внутренние функции ---
func callerInfo(skip int) string {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return ""
	}
	parts := strings.Split(file, "/")
	return fmt.Sprintf("%s:%d:", parts[len(parts)-1], line)
}

func logMessagef(format, level string, skip int, args ...interface{}) {
	userID, args := extractUserID(args)
	message := fmt.Sprintf(format, args...)
	writeLog(level, skip, message, userID)
}

func logMessage(level string, skip int, args ...interface{}) {
	userID, args := extractUserID(args)

	var sb strings.Builder
	sb.Grow(len(args) * 12) // предвыделение буфера

	for i, arg := range args {
		if i > 0 {
			sb.WriteByte(' ')
		}
		switch v := arg.(type) {
		case int:
			sb.WriteString(strconv.Itoa(v))
		case int64:
			sb.WriteString(strconv.FormatInt(v, 10))
		case uint32:
			sb.WriteString(strconv.Itoa(int(v)))
		case uint64:
			sb.WriteString(strconv.FormatUint(v, 10))
		case bool:
			sb.WriteString(strconv.FormatBool(v))
		case string:
			sb.WriteString(v)
		default:
			sb.WriteString(fmt.Sprint(v)) // fallback для остальных типов
		}
	}

	writeLog(level, skip, sb.String(), userID)
}

func extractUserID(args []interface{}) (*uint32, []interface{}) {
	if len(args) > 0 {
		if uid, ok := args[len(args)-1].(uint32); ok {
			return &uid, args[:len(args)-1]
		}
	}
	return nil, args
}

func writeLog(level string, skip int, message string, userID *uint32) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("logMessage panic:", r)
		}
	}()

	now := time.Now().Format("2006/01/02 15:04:05")
	color := levelColors[level]
	caller := callerInfo(skip)

	var sb strings.Builder
	sb.Grow(len(message) + 64) // предвыделение

	sb.WriteString(color)
	sb.WriteString(now)
	sb.WriteByte(' ')
	sb.WriteString(caller)
	sb.WriteByte(' ')
	sb.WriteString(level)
	sb.WriteByte(' ')
	if userID != nil {
		sb.WriteString("[USER:")
		sb.WriteString(strconv.Itoa(int(*userID)))
		sb.WriteString("] ")
	}
	sb.WriteString(message)
	sb.WriteString(ColorReset)

	if generalLogger != nil {
		generalLogger.Print(sb.String())
	} else {
		fmt.Println(sb.String())
	}
}

// --- утилита для выборки логов ---
func GetUserLogs(logFilePath string, userID uint32, writer func(string)) error {
	logMsg := func(msg string) {
		if writer != nil {
			writer(msg)
		} else {
			fmt.Println(msg)
		}
	}

	file, err := os.Open(logFilePath)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	userPattern := fmt.Sprintf("[USER:%d]", userID)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, userPattern) {
			logMsg(line)
		}
	}
	return scanner.Err()
}
