package logger

import (
	"bufio"
	"fmt"
	"gopkg.in/natefinch/lumberjack.v2"
	"io"
	"log"
	"os"
	"runtime"
	"strings"
	"time"
)

// ANSI цветовые коды
const (
	ColorReset = "\033[0m"
	//ColorWhite  = "\033[37m" // INFO - белый
	ColorWhite  = ""         // INFO - белый
	ColorRed    = "\033[31m" // ERROR - красный
	ColorYellow = "\033[33m" // WARNING - желтый
	ColorGreen  = "\033[32m" // DEBUG - зеленый
	ColorPurple = "\033[35m" // FATAL - фиолетовый
)

var generalLogger *log.Logger

// getColorForLevel возвращает цветовой код для уровня логирования
func getColorForLevel(level string) string {
	switch level {
	case "[INFO]":
		return ColorWhite
	case "[ERROR]":
		return ColorRed
	case "[WARNING]":
		return ColorYellow
	case "[DEBUG]":
		return ColorGreen
	case "[FATAL]":
		return ColorPurple
	default:
		return ColorWhite
	}
}

func Set(patch string) {
	logFile := &lumberjack.Logger{
		Filename:   patch,
		MaxSize:    1,
		MaxBackups: 3,
		MaxAge:     30,
		Compress:   true,
	}

	multiWriter := io.MultiWriter(os.Stdout, logFile)
	//generalLogger = log.New(multiWriter, "", log.LstdFlags)
	generalLogger = log.New(multiWriter, "", 0)
}

// Infoln записывает информационное сообщение, объединяя все аргументы
func Infoln(args ...interface{}) {
	logMessageConcat("[INFO]", 2, args...)
}

// Info записывает информационное сообщение с поддержкой форматирования
func Info(format string, args ...interface{}) {
	logMessage(format, "[INFO]", 2, args...)
}

// Error записывает сообщение об ошибке с поддержкой форматирования
func Error(format string, args ...interface{}) {
	logMessage(format, "[ERROR]", 2, args...)
}

// Warn записывает предупреждение с поддержкой форматирования
func Warn(format string, args ...interface{}) {
	logMessage(format, "[WARNING]", 2, args...)
}

// Debug записывает отладочное сообщение с поддержкой форматирования
func Debug(format string, args ...interface{}) {
	logMessage(format, "[DEBUG]", 2, args...)
}

// Fatal записывает критическое сообщение об ошибке и завершает программу
func Fatal(args ...interface{}) {
	logMessageConcat("[FATAL]", 2, args...)
	os.Exit(1)
}

// Fatalf записывает критическое сообщение об ошибке с форматированием и завершает программу
func Fatalf(format string, args ...interface{}) {
	logMessage(format, "[FATAL]", 2, args...)
	os.Exit(1)
}

// logMessage обрабатывает форматирование и определяет наличие userId
func logMessage(format string, level string, skip int, args ...interface{}) {
	var userID *uint32
	var formatArgs []interface{}

	// Получаем информацию о вызывающем коде
	_, file, line, ok := runtime.Caller(skip)
	var caller string
	if ok {
		// Извлекаем только имя файла без полного пути
		parts := strings.Split(file, "/")
		if len(parts) > 0 {
			caller = fmt.Sprintf("%s:%d:", parts[len(parts)-1], line)
		}
	}

	// Проверяем последний аргумент - если это uint32, считаем его userId
	if len(args) > 0 {
		if uid, ok := args[len(args)-1].(uint32); ok {
			userID = &uid
			formatArgs = args[:len(args)-1] // Исключаем userId из аргументов форматирования
		} else {
			formatArgs = args
		}
	}

	// Форматируем сообщение
	var message string
	if len(formatArgs) > 0 {
		message = fmt.Sprintf(format, formatArgs...)
	} else {
		message = format
	}

	// Добавляем timestamp
	now := time.Now().Format("2006/01/02 15:04:05")

	// Получаем цвет для уровня
	color := getColorForLevel(level)

	// Логируем с или без userId с цветом
	if userID != nil {
		generalLogger.Printf("%s%s %s %s [USER:%d] %s%s", color, now, caller, level, *userID, message, ColorReset)
	} else {
		generalLogger.Printf("%s%s %s %s %s%s", color, now, caller, level, message, ColorReset)
	}
}

// logMessageConcat обрабатывает конкатенацию аргументов
func logMessageConcat(level string, skip int, args ...interface{}) {
	var userID *uint32
	var messageArgs []interface{}

	// Получаем информацию о вызывающем коде
	_, file, line, ok := runtime.Caller(skip)
	var caller string
	if ok {
		parts := strings.Split(file, "/")
		if len(parts) > 0 {
			caller = fmt.Sprintf("%s:%d:", parts[len(parts)-1], line)
		}
	}

	// Проверяем последний аргумент - если это uint32, считаем его userId
	if len(args) > 0 {
		if uid, ok := args[len(args)-1].(uint32); ok {
			userID = &uid
			messageArgs = args[:len(args)-1]
		} else {
			messageArgs = args
		}
	}

	// Объединяем все аргументы в строку
	var parts []string
	for _, arg := range messageArgs {
		parts = append(parts, fmt.Sprintf("%v", arg))
	}
	message := strings.Join(parts, " ")

	// Добавляем timestamp
	now := time.Now().Format("2006/01/02 15:04:05")

	// Получаем цвет для уровня
	color := getColorForLevel(level)

	// Логируем с цветом
	if userID != nil {
		generalLogger.Printf("%s%s %s %s [USER:%d] %s%s", color, now, caller, level, *userID, message, ColorReset)
	} else {
		generalLogger.Printf("%s%s %s %s %s%s", color, now, caller, level, message, ColorReset)
	}
}

// GetUserLogs выводит все логи для конкретного пользователя через callback функцию
func GetUserLogs(logFilePath string, userID uint32, writer func(string)) error {
	logMsg := func(msg string) {
		if writer != nil {
			writer(msg) // Дополнительно отправляем через callback
		} else {
			fmt.Println(msg) // Если callback не задан, выводим в консоль
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
