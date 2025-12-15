package comdb

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"

	_ "github.com/go-sql-driver/mysql"
)

// DB представляет соединение с базой данных
type DB struct {
	dsn     string
	conn    *sql.DB
	mainCTX context.Context
	ctx     context.Context
	cancel  context.CancelFunc
}

type CreatorType uint8

type Message struct {
	Creator   CreatorType          `json:"creator"`
	Message   model.AssistResponse `json:"message"`
	Timestamp time.Time            `json:"timestamp"`
}

type Espero struct {
	Limit  uint16 `json:"limit"`
	Wait   uint8  `json:"wait"`
	Ignore bool   `json:"ignore"`
}

const (
	AI        CreatorType = 1 // Право
	User      CreatorType = 2 // Лево
	UserVoice CreatorType = 3 // Лево
	Operator  CreatorType = 4 // Прав
)

// DecompressAndExtractMetadata Функция для распаковки сжатых данных и извлечения полей Meta и MetaAction
func DecompressAndExtractMetadata(compressedData []byte) (string, []string, *Espero, error) {
	// Создаем reader для распаковки данных
	gzipReader, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return "", nil, nil, fmt.Errorf("ошибка при создании gzip reader: %w", err)
	}
	defer func(gzipReader *gzip.Reader) {
		err := gzipReader.Close()
		if err != nil {
			logger.Error("ошибка закрытия gzip reader: %v", err)
		}
	}(gzipReader)

	// Читаем распакованные данные
	decompressedData, err := io.ReadAll(gzipReader)
	if err != nil {
		return "", nil, nil, fmt.Errorf("ошибка при распаковке данных: %w", err)
	}

	// Разбираем JSON
	var modelData map[string]interface{}
	if err := json.Unmarshal(decompressedData, &modelData); err != nil {
		return "", nil, nil, fmt.Errorf("ошибка при разборе JSON модели: %w", err)
	}

	// Извлекаем поля MetaAction
	var metaAction string
	var triggers []string
	var espero = &Espero{}

	if ma, ok := modelData["mact"].(string); ok {
		metaAction = ma
	}

	// Извлекаем и конвертируем поле triggers
	if t, ok := modelData["trig"]; ok {
		if trigArray, ok := t.([]interface{}); ok {
			for _, item := range trigArray {
				if str, ok := item.(string); ok {
					triggers = append(triggers, str)
				}
			}
		}
	}

	// Извлекаем поля espero
	if esp, ok := modelData["espero"].(map[string]interface{}); ok {
		if limit, ok := esp["limit"].(float64); ok {
			espero.Limit = uint16(limit)
		}
		if wait, ok := esp["wait"].(float64); ok {
			espero.Wait = uint8(wait)
		}
		if ignore, ok := esp["ignore"].(bool); ok {
			espero.Ignore = ignore
		}
	}

	return metaAction, triggers, espero, nil
}

// New создает новое подключение к базе данных
func New(parent context.Context, conf *conf.Conf) *DB {
	var dsn string
	if mode.ProductionMode {
		dsn = fmt.Sprintf("%s:%s@unix(%s)/%s?parseTime=true&charset=utf8mb4&loc=Local",
			conf.DB.User,
			conf.DB.Password,
			conf.DB.Host,
			conf.DB.Name,
		)
	} else {
		dsn = fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4&loc=Local",
			conf.DB.User,
			conf.DB.Password,
			conf.DB.Host,
			conf.DB.Name,
		)
	}
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		logger.Fatalf("ошибка открытия базы данных: %e", err)
	}

	// Пул соединений
	conn.SetMaxOpenConns(100)
	conn.SetMaxIdleConns(100)
	conn.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())

	return &DB{
		mainCTX: parent,
		ctx:     ctx,
		cancel:  cancel,
		dsn:     dsn,
		conn:    conn,
	}
}

// Close закрывает соединения с базой данных и отменяет контекст
func (d *DB) Close() error {
	// Отменяем контекст базы данных
	if d.cancel != nil {
		d.cancel()
	}

	logger.Info("DB: закрываю все соединения...")

	// Закрываем соединение с базой данных
	if d.conn != nil {
		if err := d.conn.Close(); err != nil {
			logger.Error("DB: ошибка закрытия соединения: %v", err)
			return err
		}
	}

	logger.Info("DB: все соединения закрыты")
	return nil
}

// Conn возвращает базовое подключение к БД для расширенного использования в приложениях
func (d *DB) Conn() *sql.DB {
	return d.conn
}

// Context возвращает контекст БД для использования в пользовательских методах
func (d *DB) Context() context.Context {
	return d.ctx
}

// MainCTX возвращает главный контекст приложения
func (d *DB) MainCTX() context.Context {
	return d.mainCTX
}

// ReadContext читает контекст диалога из базы данных
func (d *DB) ReadContext(dialogId uint64) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	var data sql.NullString
	err := d.conn.QueryRowContext(ctx, "SELECT ReadContext(?)", dialogId).Scan(&data)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове ReadContext: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена при вызове ReadContext: %w", err)
		default:
			return nil, fmt.Errorf("ошибка вызова хранимой функции ReadContext: %w", err)
		}
	}

	if !data.Valid {
		return nil, fmt.Errorf("получены пустые данные")
	}

	return json.RawMessage(data.String), nil
}

// SaveContext сохраняет контекст диалога в базу данных
func (d *DB) SaveContext(threadId uint64, dialogContext json.RawMessage) error {
	if threadId == 0 {
		return fmt.Errorf("получен пустой тред")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.conn.ExecContext(ctx, "CALL SaveContext(?, ?)", threadId, dialogContext); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при сохранении контекста: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка сохранения контекста: %w", err)
		}
	}

	return nil
}

// ReadDialog читает всю историю диалога и возвращает структурированные данные
func (d *DB) ReadDialog(dialogId uint64) (model.DialogData, error) {
	// Пустая структура для возврата в случае ошибки
	var emptyData model.DialogData

	// Проверяем входное значение
	if dialogId == 0 {
		return emptyData, fmt.Errorf("получен некорректный dialogId")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	// Выполняем вызов хранимой функции
	var data sql.NullString
	err := d.conn.QueryRowContext(ctx, "SELECT ReadDialog(?);", dialogId).Scan(&data)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return emptyData, fmt.Errorf("тайм-аут (%d с) при вызове функции ReadDialog: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return emptyData, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return emptyData, fmt.Errorf("диалог не найден")
		default:
			return emptyData, fmt.Errorf("ошибка вызова хранимой функции ReadDialog: %w", err)
		}
	}

	// Если диалог не найден или данные пустые
	if !data.Valid {
		return emptyData, fmt.Errorf("получены пустые данные")
	}

	// Десериализуем JSON в структуру DialogData
	var dialogData model.DialogData
	if err := json.Unmarshal([]byte(data.String), &dialogData); err != nil {
		logger.Error("ReadDialog(%d): Ошибка десериализации: %v. Данные: %s", dialogId, err, data.String)
		return emptyData, fmt.Errorf("ошибка десериализации данных диалога: %w", err)
	}

	return dialogData, nil
}

// SaveDialog сохраняет всю историю диалога в базу данных
func (d *DB) SaveDialog(treadId uint64, message json.RawMessage) error {
	if treadId == 0 {
		return fmt.Errorf("получен пустот тред")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	// Вызываем хранимую процедуру для сохранения данных диалога
	if _, err := d.conn.ExecContext(ctx, "CALL SaveDialog(?, ?)", treadId, message); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при сохранении диалога: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка сохранения диалога: %w", err)
		}
	}

	return nil
}
