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
)

var generalLogger *log.Logger

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

	// Логируем с или без userId
	if userID != nil {
		generalLogger.Printf("%s %s [USER:%d] %s", caller, level, *userID, message)
	} else {
		generalLogger.Printf("%s %s %s", caller, level, message)
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
