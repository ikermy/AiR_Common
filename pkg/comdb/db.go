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
	models "github.com/ikermy/AiR_Common/pkg/model/create"

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

// Используем типы из пакета model/create для совместимости с интерфейсом models.DB
type (
	Ids             = models.Ids
	VecIds          = models.VecIds
	UserModelRecord = models.UserModelRecord
	ProviderType    = models.ProviderType
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
	if dialogId == 0 {
		return nil, fmt.Errorf("получен пустой dialogId")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	var data sql.NullString
	if err := d.conn.QueryRowContext(ctx, "SELECT ReadContext(?)", dialogId).Scan(&data); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове ReadContext: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена при вызове ReadContext: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("контекст диалога не найден")
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

// SetActiveModel переключает активную модель пользователя
// Параметры:
//   - userId: ID пользователя
//   - modelId: ID записи из таблицы user_models
//
// Триггер БД автоматически снимет IsActive с других моделей пользователя
func (d *DB) SetActiveModel(userId uint32, modelId uint64) error {
	if userId == 0 {
		return fmt.Errorf("получен пустой userId")
	}

	if modelId == 0 {
		return fmt.Errorf("получен пустой modelId")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	// Начинаем транзакцию
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ошибка начала транзакции: %w", err)
	}
	defer tx.Rollback()

	// Обновляем IsActive для указанной модели
	// Триггер trg_user_models_before_update автоматически снимет IsActive с других моделей
	result, err := tx.ExecContext(ctx,
		"UPDATE user_models SET IsActive = 1 WHERE Id = ? AND UserId = ?",
		modelId, userId)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при переключении активной модели: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка переключения активной модели: %w", err)
		}
	}

	// Проверяем, была ли обновлена хотя бы одна строка
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("ошибка получения количества обновленных строк: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("модель с Id=%d для пользователя %d не найдена", modelId, userId)
	}

	// Фиксируем транзакцию
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ошибка фиксации транзакции: %w", err)
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

// UpdateDialogsMeta устанавливает достижение цели
func (d *DB) UpdateDialogsMeta(dialogId uint64, meta string) error {
	if dialogId == 0 {
		return fmt.Errorf("получен пустой dialogId")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.conn.ExecContext(ctx, "CALL UpdateDialogsMeta(?,?)", dialogId, meta); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при сохранении достижения цели: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка сохранения достижения цели: %w", err)
		}
	}

	return nil
}

// SaveChannelData сохраняет данные канала
func (d *DB) SaveChannelData(userId uint32, channelType string, data string, enabled bool) error {
	if userId == 0 || channelType == "" {
		return fmt.Errorf("получены некорректные значения: userId или channelType пусты")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	// Конвертируем boolean в int для MySQL
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}

	// Проверяем, является ли data уже валидным JSON
	var jsonData string
	if json.Valid([]byte(data)) {
		jsonData = data
	} else {
		// Выбираем ключ в зависимости от типа канала
		var key string
		switch channelType {
		case "tgbot":
			key = "token"
		case "widget":
			key = "script"
		case "tgubot":
			key = "token"
		default:
			key = "error" // Дефолтный ключ ошибка
		}

		// Оборачиваем данные в JSON объект с соответствующим ключом
		jsonData = fmt.Sprintf(`{%q: %q}`, key, data)
	}

	if _, err := d.conn.ExecContext(ctx, "CALL SaveChannelData(?, ?, ?, ?)",
		userId,                   // p_UserId
		channelType,              // p_Type
		jsonData,                 // p_Data (теперь валидный JSON)
		enabledInt); err != nil { // p_Enabled
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при сохранении канала: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка сохранения канала: %w", err)
		}
	}

	return nil
}

// GetOrSetTreadAndResponder получает или создает тред и респондера
func (d *DB) GetOrSetTreadAndResponder(
	userId uint32,
	responderRealId uint64,
	responderName string,
) (uint64, error) {
	if userId == 0 {
		return 0, fmt.Errorf("получен пустой userId")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	// Создаём временную переменную для выхода
	if _, err := d.conn.ExecContext(ctx, "SET @out_dialogId = 0;"); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, fmt.Errorf("тайм-аут (%d с) при создании временной переменной: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return 0, fmt.Errorf("операция отменена: %w", err)
		default:
			return 0, fmt.Errorf("ошибка при создании временной переменной: %w", err)
		}
	}

	// Выполняем вызов процедуры
	if _, err := d.conn.ExecContext(ctx, "CALL GetOrSetTreadAndResponder(?, ?, ?, ?, @out_dialogId);",
		userId, responderRealId, responderName, 2); err != nil { // Тип чата TgBot
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, fmt.Errorf("тайм-аут (%d с) при вызове процедуры GetOrSetTreadAndResponder: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return 0, fmt.Errorf("операция отменена: %w", err)
		default:
			return 0, fmt.Errorf("ошибка вызова процедуры GetOrSetTreadAndResponder: %w", err)
		}
	}

	// Читаем значение из переменной
	var dialogId uint64
	if err := d.conn.QueryRowContext(ctx, "SELECT @out_dialogId;").Scan(&dialogId); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, fmt.Errorf("тайм-аут (%d с) при получении значения @out_dialogId: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return 0, fmt.Errorf("операция отменена: %w", err)
		default:
			return 0, fmt.Errorf("ошибка получения значения @out_dialogId: %w", err)
		}
	}

	return dialogId, nil
}

// GetUserSubscriptionLimites получает лимиты подписки пользователя
func (d *DB) GetUserSubscriptionLimites(userId uint32) (json.RawMessage, error) {
	if userId == 0 {
		return nil, fmt.Errorf("получен пустой userId")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	var data sql.NullString
	if err := d.conn.QueryRowContext(ctx, "SELECT GetUserSubscriptionLimites(?)", userId).Scan(&data); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове функции GetUserSubscriptionLimites: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("данные подписки не найдены")
		default:
			return nil, fmt.Errorf("ошибка вызова хранимой функции GetUserSubscriptionLimites: %w", err)
		}
	}

	if !data.Valid {
		return nil, fmt.Errorf("получены пустые данные")
	}

	return json.RawMessage(data.String), nil
}

// DisableAllUserChannel отключает все каналы пользователя
func (d *DB) DisableAllUserChannel(userId uint32) error {
	if userId == 0 {
		return fmt.Errorf("получен пустой userId")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.conn.ExecContext(ctx, "CALL DisableAllUserChannel(?)", userId); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при отключении каналов: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка отключения каналов: %w", err)
		}
	}

	return nil
}

// SetChannelEnabled включает или отключает канал пользователя
func (d *DB) SetChannelEnabled(userId uint32, chName string, status bool) error {
	if userId == 0 || chName == "" {
		return fmt.Errorf("получены некорректные значения: userId или chName пусты")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.conn.ExecContext(ctx, "CALL SetChannelEnabled(?,?,?)", userId, chName, status); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при сохранении статуса канала: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка сохранения статуса канала: %w", err)
		}
	}

	return nil
}

// PlusOneMessage увеличивает счетчик сообщений пользователя на 1
func (d *DB) PlusOneMessage(userId uint32) error {
	if userId == 0 {
		return fmt.Errorf("получен пустой userId")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.conn.ExecContext(ctx, "CALL PlusOneMessage(?)", userId); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при вызове PlusOneMessage: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка вызова PlusOneMessage: %w", err)
		}
	}

	return nil
}

// GetNotificationChannel получает данные каналов уведомлений пользователя
func (d *DB) GetNotificationChannel(userId uint32) (json.RawMessage, error) {
	if userId == 0 {
		return nil, fmt.Errorf("получен пустой userId")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	var data sql.NullString
	if err := d.conn.QueryRowContext(ctx, "SELECT GetNotificationChannel(?)", userId).Scan(&data); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове функции GetNotificationChannel: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("каналы уведомлений не найдены")
		default:
			return nil, fmt.Errorf("ошибка вызова хранимой функции GetNotificationChannel: %w", err)
		}
	}

	if !data.Valid {
		return nil, fmt.Errorf("получены пустые данные")
	}

	return json.RawMessage(data.String), nil
}

// GetUserModels получает все модели пользователя из таблицы user_models
func (d *DB) GetAllUserModels(userId uint32) ([]models.UserModelRecord, error) {
	if userId == 0 {
		return nil, fmt.Errorf("получен пустой userId")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel*time.Second)
	defer cancel()

	rows, err := d.conn.QueryContext(ctx,
		`SELECT 
            um.ModelId,
            um.Provider,
            um.IsActive,
            ug.AssistantId,
            ug.Ids
        FROM user_models um
        JOIN user_gpt ug ON um.ModelId = ug.Id
        WHERE um.UserId = ?
        ORDER BY um.IsActive DESC, um.CreatedAt DESC`, userId)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при получении моделей: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		default:
			return nil, fmt.Errorf("ошибка получения моделей: %w", err)
		}
	}
	defer rows.Close()

	var records []models.UserModelRecord
	for rows.Next() {
		var record models.UserModelRecord
		var isActive int8
		var idsRaw sql.NullString

		if err := rows.Scan(&record.ModelId, &record.Provider, &isActive, &record.AssistId, &idsRaw); err != nil {
			logger.Warn("Ошибка сканирования записи user_models: %v", err, userId)
			continue
		}

		record.IsActive = isActive == 1

		// Парсим JSON из поля Ids
		if idsRaw.Valid && idsRaw.String != "" {
			// Сохраняем raw JSON в AllIds для доступа к VectorId
			record.AllIds = []byte(idsRaw.String)

			// Парсим FileIds для обратной совместимости
			var data struct {
				FileIds  []models.Ids `json:"FileIds"`
				VectorId []string     `json:"VectorId"`
			}
			if err := json.Unmarshal([]byte(idsRaw.String), &data); err != nil {
				logger.Warn("Ошибка парсинга FileIds для ModelId %d: %v", record.ModelId, err, userId)
			} else {
				record.FileIds = data.FileIds
			}
		}

		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка при итерации по записям: %w", err)
	}

	return records, nil
}

// UpdateUserGPT обновляет поле Ids (AllIds) в таблице user_gpt
// Используется для обновления информации о файлах и векторных хранилищах/библиотеках
func (d *DB) UpdateUserGPT(userId uint32, modelId uint64, assistId string, allIds []byte) error {
	if userId == 0 {
		return fmt.Errorf("получен пустой userId")
	}
	if modelId == 0 {
		return fmt.Errorf("получен пустой modelId")
	}

	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	// Подготавливаем значение для БД
	// Если allIds == nil, то сохраняем SQL NULL, иначе строку
	var idsValue interface{}
	if allIds == nil || len(allIds) == 0 {
		idsValue = nil // SQL NULL
	} else {
		idsValue = string(allIds)
	}

	// Обновляем поле Ids в user_gpt
	_, err := d.conn.ExecContext(ctx, `
		UPDATE user_gpt 
		SET Ids = ? 
		WHERE Id = ? AND AssistantId = ?
	`, idsValue, modelId, assistId)

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("тайм-аут (%d с) при обновлении user_gpt: %w", mode.SqlTimeToCancel, err)
		}
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("операция отменена: %w", err)
		}
		return fmt.Errorf("ошибка обновления user_gpt: %w", err)
	}

	// Обновляем timestamp пользователя
	_, err = d.Conn().ExecContext(ctx, `
		UPDATE users 
		SET changed = 1, Timechange = CURRENT_TIMESTAMP() 
		WHERE Id = ?
	`, userId)

	if err != nil {
		// Логируем, но не возвращаем ошибку - основное обновление прошло успешно
		logger.Warn("Не удалось обновить timestamp пользователя %d: %v", userId, err, userId)
	}

	return nil
}

func (d *DB) GetUserVectorStorage(userId uint32) (string, error) {
	// Проверяем входное значение
	if userId == 0 {
		return "", fmt.Errorf("получен некорректный userId")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.ctx, mode.SqlTimeToCancel)
	defer cancel()

	// SQL запрос для получения первого элемента VectorId из JSON активной модели
	// Используем новую структуру через user_models
	query := `
  SELECT JSON_UNQUOTE(JSON_EXTRACT(ug.Ids, '$.VectorId[0]'))
  FROM user_models um
  JOIN user_gpt ug ON um.ModelId = ug.Id
  WHERE um.UserId = ? AND um.IsActive = 1
  LIMIT 1`

	var data sql.NullString
	err := d.conn.QueryRowContext(ctx, query, userId).Scan(&data)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return "", fmt.Errorf("тайм-аут (%d с) при получении VectorStorage: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return "", fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return "", nil // Данные не найдены, но это не ошибка
		default:
			return "", fmt.Errorf("ошибка получения VectorStorage: %w", err)
		}
	}

	if !data.Valid {
		return "", nil // Возвращаем пустую строку если данные NULL
	}

	return data.String, nil
}
