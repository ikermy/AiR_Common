package logger

import (
	"gopkg.in/natefinch/lumberjack.v2"
	"io"
	"log"
	"os"
)

func Set(patch string) {
	logFile := &lumberjack.Logger{
		Filename:   patch, // Путь к файлу логов
		MaxSize:    1,     // Максимальный размер файла в мегабайтах
		MaxBackups: 3,     // Максимальное количество старых файлов для хранения
		MaxAge:     30,    // Максимальное количество дней для хранения старых файлов
		Compress:   true,  // Сжимать ли старые файлы
	}
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)
	log.SetFlags(log.LstdFlags | log.Lshortfile) // Включаем временные метки и короткие имена файлов
}
