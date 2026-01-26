package comdb

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model/create"

	_ "github.com/go-sql-driver/mysql"
)

const sqlTimeToCancel = 5 // Тайм-аут на операции с БД

type Exterior interface {
	GetOrSetTreadAndResponder(userId uint32, responderRealId uint64, responderName string, chatType ChatType) (uint64, error)
	DisableAllUserChannel(userId uint32) error
	PlusOneMessage(userId uint32) error
	GetNotificationChannel(userId uint32) (json.RawMessage, error)
	GetUserSubscriptionLimites(userId uint32) (json.RawMessage, error)
	SaveDialog(treadId uint64, message json.RawMessage) error
	ReadDialog(dialogId uint64, limit ...uint8) (json.RawMessage, error)
	DeleteDialog(userId uint32, dialogId uint64) error
	UpdateDialogsMeta(dialogId uint64, meta string) error
	ReadContext(dialogId uint64, provider create.ProviderType) (json.RawMessage, error)
	SaveContext(threadId uint64, provider create.ProviderType, dialogContext json.RawMessage) error
	GetActiveProvider(userId uint32) (create.ProviderType, error)
	GetAllUserModels(userId uint32) ([]create.UserModelRecord, error)
	UpdateUserGPT(userId uint32, modelId uint64, assistId string, allIds []byte) error
	GetUserVectorStorage(userId uint32) (string, error)
	SetChannelEnabled(userId uint32, chName string, status bool) error
	SaveUserModel(userId uint32, provider create.ProviderType, name, assistantId string, data []byte, modType uint8, ids json.RawMessage, operator bool) error
	GetOrSetUserStorageLimit(userID uint32, setStorage int64) (remaining uint64, totalLimit uint64, err error)
	ReadUserModel(userId uint32) ([]byte, *create.VecIds, error)

	// User Model Management - методы для управления моделями пользователя (для create.DB)
	ReadUserModelByProvider(userId uint32, provider create.ProviderType) ([]byte, *create.VecIds, error)
	GetActiveModel(userId uint32) (*create.UserModelRecord, error)
	GetModelByProvider(userId uint32, provider create.ProviderType) (*create.UserModelRecord, error)
	GetModelByProviderAnyStatus(userId uint32, provider create.ProviderType) (*create.UserModelRecord, error)
	SetActiveModel(userId uint32, modelId uint64) error
	SetActiveModelByProvider(userId uint32, provider create.ProviderType) error
	RemoveModelFromUser(userId uint32, modelId uint64) error

	// Vector Embeddings - методы для работы с эмбеддингами в MariaDB
	SaveEmbedding(userId uint32, modelId uint64, docID, docName, content string, embedding []float32, metadata create.DocumentMetadata) error
	GetEmbedding(modelId uint64, docID string) ([]float32, error)
	DeleteEmbedding(modelId uint64, docID string) error
	DeleteAllModelEmbeddings(modelId uint64) error
	CountModelEmbeddings(modelId uint64) (int, error)
	ListModelEmbeddings(modelId uint64) ([]create.VectorDocument, error)
	SearchSimilarEmbeddings(modelId uint64, queryEmbedding []float32, limit int) ([]create.VectorDocument, error)

	// Contact Availability - методы для работы с доступностью контактов в разных провайдерах
	SetContactAvailability(userID uint32, contact, provider string, isAvailable bool) error
	GetContactAvailability(userID uint32, contact string) (map[string]bool, error)
	GetContactsAvailableIn(userID uint32, provider string) ([]string, error)
	GetContactsInBothProviders(userID uint32, provider1, provider2 string) ([]string, error)
}

// ChatType определяет тип чата (используется в БД)
type ChatType uint8

const (
	TelegramBot ChatType = 0
	Web         ChatType = 1
	Telegram    ChatType = 2
	Keyboard    ChatType = 3
	Widget      ChatType = 4
	WhatsApp    ChatType = 5
	Instagram   ChatType = 6
)

type Espero struct {
	Limit  uint16 `json:"limit"`
	Wait   uint8  `json:"wait"`
	Ignore bool   `json:"ignore"`
}

type CreatorType uint8

const (
	AI        CreatorType = 1 // Право
	User      CreatorType = 2 // Лево
	UserVoice CreatorType = 3 // Лево
	Operator  CreatorType = 4 // Прав
)

// Используем типы из пакета model/create для совместимости с интерфейсом create.DB
type (
	Ids             = create.Ids
	VecIds          = create.VecIds
	UserModelRecord = create.UserModelRecord
	ProviderType    = create.ProviderType
)

// DB представляет соединение с базой данных
type DB struct {
	dsn     string
	conn    *sql.DB
	mainCTX context.Context
	ctx     context.Context
	cancel  context.CancelFunc
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

// DecompressAndExtractMetadata Функция для распаковки сжатых данных и извлечения полей Meta и MetaAction
// Также извлекает параметры Google модели: Image, WebSearch, Video, Haunter и Search
func DecompressAndExtractMetadata(compressedData []byte) (metaAction string, triggers []string, espero *Espero, image, webSearch, video, haunter, search bool, err error) {
	// Создаем reader для распаковки данных
	gzipReader, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return "", nil, nil, false, false, false, false, false, fmt.Errorf("ошибка при создании gzip reader: %w", err)
	}
	defer func(gzipReader *gzip.Reader) {
		closeErr := gzipReader.Close()
		if closeErr != nil {
			logger.Error("ошибка закрытия gzip reader: %v", closeErr)
		}
	}(gzipReader)

	// Читаем распакованные данные
	decompressedData, err := io.ReadAll(gzipReader)
	if err != nil {
		return "", nil, nil, false, false, false, false, false, fmt.Errorf("ошибка при распаковке данных: %w", err)
	}

	// Разбираем JSON
	var modelData map[string]interface{}
	if err := json.Unmarshal(decompressedData, &modelData); err != nil {
		return "", nil, nil, false, false, false, false, false, fmt.Errorf("ошибка при разборе JSON модели: %w", err)
	}

	// Извлекаем поля MetaAction
	espero = &Espero{}

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

	// Извлекаем параметры Google модели (image, web_search, video)
	if val, ok := modelData["image"].(bool); ok {
		image = val
	}
	if val, ok := modelData["web_search"].(bool); ok {
		webSearch = val
	}
	if val, ok := modelData["video"].(bool); ok {
		video = val
	}

	// Извлекаем флаг haunter
	if val, ok := modelData["haunter"].(bool); ok {
		haunter = val
	}

	// Извлекаем флаг search
	if val, ok := modelData["search"].(bool); ok {
		search = val
	}

	return metaAction, triggers, espero, image, webSearch, video, haunter, search, nil
}

// ReadContext читает контекст диалога из базы данных
func (d *DB) ReadContext(dialogId uint64, provider create.ProviderType) (json.RawMessage, error) {
	if dialogId == 0 {
		return nil, fmt.Errorf("получен пустой dialogId")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	var data sql.NullString
	if err := d.Conn().QueryRowContext(ctx, "SELECT ReadContext(?, ?)", dialogId, provider.String()).Scan(&data); err != nil {
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
func (d *DB) SaveContext(threadId uint64, provider create.ProviderType, dialogContext json.RawMessage) error {
	if threadId == 0 {
		return fmt.Errorf("получен пустой тред")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.Conn().ExecContext(ctx, "CALL SaveContext(?, ?, ?)", threadId, provider.String(), dialogContext); err != nil {
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
func (d *DB) ReadDialog(dialogId uint64, limit ...uint8) (json.RawMessage, error) {
	// Проверяем входное значение
	if dialogId == 0 {
		return nil, fmt.Errorf("получен некорректный dialogId")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// Выполняем вызов хранимой функции
	var data sql.NullString
	var err error

	if len(limit) == 0 {
		// без лимита
		err = d.Conn().QueryRowContext(ctx,
			"SELECT ReadDialog(?, NULL);", dialogId).Scan(&data)
	} else {
		// с лимитом
		err = d.Conn().QueryRowContext(ctx,
			"SELECT ReadDialog(?, ?);", dialogId, limit[0]).Scan(&data)
	}
	//err := d.Conn().QueryRowContext(ctx, "SELECT ReadDialog(?);", dialogId).Scan(&data)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове функции ReadDialog: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("диалог не найден")
		default:
			return nil, fmt.Errorf("ошибка вызова хранимой функции ReadDialog: %w", err)
		}
	}

	// Если диалог не найден или данные пустые
	if !data.Valid {
		return nil, fmt.Errorf("получены пустые данные")
	}

	return json.RawMessage(data.String), nil
}

// DeleteDialog удаляет диалог с проверкой прав пользователя
func (d *DB) DeleteDialog(userId uint32, dialogId uint64) error {
	// Проверяем входные значения
	if dialogId == 0 {
		return fmt.Errorf("получен некорректный dialogId")
	}
	if userId == 0 {
		return fmt.Errorf("получен некорректный userId")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// Вызываем хранимую процедуру с проверкой прав
	_, err := d.Conn().ExecContext(ctx, "CALL DeleteDialog(?, ?)", dialogId, userId)
	if err != nil {
		// Проверяем специальный код ошибки для демо-пользователя
		if strings.Contains(err.Error(), "SQLSTATE 45001") ||
			strings.Contains(err.Error(), "Невозможно удалить диалог демо пользователя") {
			return fmt.Errorf("демо пользователь не может удалять диалоги")
		}

		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при удалении диалога: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка удаления диалога: %w", err)
		}
	}

	return nil
}

// SaveDialog сохраняет всю историю диалога в базу данных
func (d *DB) SaveDialog(treadId uint64, message json.RawMessage) error {
	if treadId == 0 {
		return fmt.Errorf("получен пустот тред")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	// Вызываем хранимую процедуру для сохранения данных диалога
	if _, err := d.Conn().ExecContext(ctx, "CALL SaveDialog(?, ?)", treadId, message); err != nil {
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

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.Conn().ExecContext(ctx, "CALL UpdateDialogsMeta(?,?)", dialogId, meta); err != nil {
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

// GetOrSetTreadAndResponder получает или создает тред и респондера
func (d *DB) GetOrSetTreadAndResponder(
	userId uint32,
	responderRealId uint64,
	responderName string,
	chatType ChatType,
) (uint64, error) {
	if userId == 0 {
		return 0, fmt.Errorf("получен пустой userId")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	// Создаём временную переменную для выхода
	if _, err := d.Conn().ExecContext(ctx, "SET @out_dialogId = 0;"); err != nil {
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
	if _, err := d.Conn().ExecContext(ctx, "CALL GetOrSetTreadAndResponder(?, ?, ?, ?, @out_dialogId);",
		userId, responderRealId, responderName, chatType); err != nil { // Тип чата TgBot
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
	if err := d.Conn().QueryRowContext(ctx, "SELECT @out_dialogId;").Scan(&dialogId); err != nil {
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

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	var data sql.NullString
	if err := d.Conn().QueryRowContext(ctx, "SELECT GetUserSubscriptionLimites(?)", userId).Scan(&data); err != nil {
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

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.Conn().ExecContext(ctx, "CALL DisableAllUserChannel(?)", userId); err != nil {
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

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.Conn().ExecContext(ctx, "CALL SetChannelEnabled(?,?,?)", userId, chName, status); err != nil {
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

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.Conn().ExecContext(ctx, "CALL PlusOneMessage(?)", userId); err != nil {
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

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	var data sql.NullString
	if err := d.Conn().QueryRowContext(ctx, "SELECT GetNotificationChannel(?)", userId).Scan(&data); err != nil {
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
func (d *DB) GetAllUserModels(userId uint32) ([]create.UserModelRecord, error) {
	if userId == 0 {
		return nil, fmt.Errorf("получен пустой userId")
	}

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	rows, err := d.Conn().QueryContext(ctx,
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
			return nil, fmt.Errorf("тайм-аут (%d с) при получении моделей: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		default:
			return nil, fmt.Errorf("ошибка получения моделей: %w", err)
		}
	}
	defer rows.Close()

	var records []create.UserModelRecord
	for rows.Next() {
		var record create.UserModelRecord
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
				FileIds  []create.Ids `json:"FileIds"`
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

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
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
	_, err := d.Conn().ExecContext(ctx, `
		UPDATE user_gpt 
		SET Ids = ? 
		WHERE Id = ? AND AssistantId = ?
	`, idsValue, modelId, assistId)

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("тайм-аут (%d с) при обновлении user_gpt: %w", sqlTimeToCancel, err)
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
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
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
	err := d.Conn().QueryRowContext(ctx, query, userId).Scan(&data)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return "", fmt.Errorf("тайм-аут (%d с) при получении VectorStorage: %w", sqlTimeToCancel, err)
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

// GetActiveProvider получает тип провайдера активной модели пользователя без создания дочернего контекста дял максимальной производительности
func (d *DB) GetActiveProvider(userId uint32) (create.ProviderType, error) {
	if userId == 0 {
		return 0, fmt.Errorf("получен некорректный userId")
	}

	// Используем родительский контекст напрямую для максимальной производительности
	// Запрашиваем активные модели с лимитом 2, чтобы проверить уникальность за один запрос
	query := `SELECT Provider FROM user_models WHERE UserId = ? AND IsActive = 1 LIMIT 2`
	rows, err := d.Conn().QueryContext(d.Context(), query, userId)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, fmt.Errorf("тайм-аут при получении активной модели: %w", err)
		case errors.Is(err, context.Canceled):
			return 0, fmt.Errorf("операция отменена: %w", err)
		default:
			return 0, fmt.Errorf("GetActiveProvider: query error: %w", err)
		}
	}
	defer rows.Close()

	var providers []uint8
	for rows.Next() {
		var p uint8
		if err := rows.Scan(&p); err != nil {
			return 0, fmt.Errorf("GetActiveProvider: scan error: %w", err)
		}
		providers = append(providers, p)
	}

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("GetActiveProvider: rows iteration error: %w", err)
	}

	if len(providers) == 0 {
		return 0, fmt.Errorf("GetActiveProvider: %w", fmt.Errorf("активная модель не найдена"))
	}

	if len(providers) > 1 {
		return 0, fmt.Errorf("найдено несколько активных моделей (найдено %d)", len(providers))
	}

	return create.ProviderType(providers[0]), nil
}

// ReadUserModelByProvider получает сжатые данные модели пользователя по провайдеру
func (d *DB) ReadUserModelByProvider(userId uint32, provider create.ProviderType) ([]byte, *create.VecIds, error) {
	// Проверяем входные значения
	if userId == 0 {
		return nil, nil, fmt.Errorf("получен некорректный userId")
	}
	if !provider.IsValid() {
		return nil, nil, fmt.Errorf("получен некорректный provider: %d", provider)
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// SQL запрос для получения Data и Ids из user_gpt по провайдеру
	query := `
		SELECT TO_BASE64(ug.Data), ug.Ids
		FROM user_models um
		JOIN user_gpt ug ON um.ModelId = ug.Id
		WHERE um.UserId = ? AND um.Provider = ?`

	var base64Data sql.NullString
	var idsJson sql.NullString

	err := d.Conn().QueryRowContext(ctx, query, userId, uint8(provider)).Scan(&base64Data, &idsJson)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, nil, fmt.Errorf("тайм-аут (%d с) при вызове ReadUserModelByProvider: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil, nil // Модель не найдена, но это не ошибка
		default:
			return nil, nil, fmt.Errorf("ошибка получения данных ReadUserModelByProvider: %w", err)
		}
	}

	// Проверяем на пустой результат или null
	if !base64Data.Valid || base64Data.String == "" {
		return nil, nil, nil // Модель не найдена
	}

	// Инициализируем структуру VecIds по умолчанию с пустыми массивами
	vecIds := &create.VecIds{
		VectorId: []string{},
		FileIds:  []create.Ids{},
	}

	// ВАЖНО: Для Google провайдера (provider=3) поле Ids содержит конфигурацию модели,
	// а не file_ids/vector_id, поэтому НЕ парсим его в VecIds
	// Эмбеддинги для Google хранятся в отдельной таблице vector_embeddings
	if provider != create.ProviderGoogle {
		// Для OpenAI и Mistral парсим Ids в VecIds (file_ids, vector_id)
		if idsJson.Valid && idsJson.String != "" && idsJson.String != "null" {
			if err := json.Unmarshal([]byte(idsJson.String), vecIds); err != nil {
				return nil, nil, fmt.Errorf("ошибка разбора Ids: %w", err)
			}
		}
	}
	// Для Google провайдера vecIds остаётся с пустыми массивами

	// Декодируем base64 данные
	decodedData, err := base64.StdEncoding.DecodeString(base64Data.String)
	if err != nil {
		return nil, nil, fmt.Errorf("ошибка декодирования base64: %w", err)
	}

	return decodedData, vecIds, nil
}

// GetActiveModel получает активную модель пользователя
func (d *DB) GetActiveModel(userId uint32) (*create.UserModelRecord, error) {
	// Проверяем входное значение
	if userId == 0 {
		return nil, fmt.Errorf("получен некорректный userId")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// SQL запрос для получения активной модели
	query := `
		SELECT 
			um.Id,
			ug.AssistantId,
			um.Provider,
			um.IsActive,
			ug.Ids
		FROM user_models um
		JOIN user_gpt ug ON um.ModelId = ug.Id
		WHERE um.UserId = ? AND um.IsActive = 1
		LIMIT 1`

	var modelId uint64
	var assistId string
	var provider uint8
	var isActive bool
	var idsJson sql.NullString

	err := d.Conn().QueryRowContext(ctx, query, userId).Scan(
		&modelId,
		&assistId,
		&provider,
		&isActive,
		&idsJson,
	)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове GetActiveModel: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil // Активная модель не найдена
		default:
			return nil, fmt.Errorf("ошибка получения активной модели: %w", err)
		}
	}

	// Создаем запись модели
	record := &create.UserModelRecord{
		ModelId:  modelId,
		AssistId: assistId,
		Provider: create.ProviderType(provider),
		IsActive: isActive,
		FileIds:  []create.Ids{},
	}

	// Парсим JSON с Ids
	if idsJson.Valid && idsJson.String != "" && idsJson.String != "null" {
		record.AllIds = []byte(idsJson.String)

		var vecIds create.VecIds
		if err := json.Unmarshal([]byte(idsJson.String), &vecIds); err != nil {
			return nil, fmt.Errorf("ошибка разбора Ids: %w", err)
		}
		record.FileIds = vecIds.FileIds
	}

	return record, nil
}

// GetModelByProvider получает АКТИВНУЮ модель пользователя по провайдеру
// Если модель не активна - возвращает nil
func (d *DB) GetModelByProvider(userId uint32, provider create.ProviderType) (*create.UserModelRecord, error) {
	// Проверяем входные значения
	if userId == 0 {
		return nil, fmt.Errorf("получен некорректный userId")
	}
	if !provider.IsValid() {
		return nil, fmt.Errorf("получен некорректный provider: %d", provider)
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// SQL запрос для получения модели по провайдеру
	query := `
		SELECT 
			um.ModelId,
			ug.AssistantId,
			um.Provider,
			um.IsActive,
			ug.Ids
		FROM user_models um
		INNER JOIN user_gpt ug ON um.ModelId = ug.Id
		WHERE um.UserId = ? 
			AND um.Provider = ?
			AND um.IsActive = 1
		LIMIT 1`

	var modelId uint64
	var assistId string
	var providerDb uint8
	var isActive bool
	var idsJson sql.NullString

	err := d.Conn().QueryRowContext(ctx, query, userId, uint8(provider)).Scan(
		&modelId,
		&assistId,
		&providerDb,
		&isActive,
		&idsJson,
	)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове GetModelByProvider: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil // Модель не найдена
		default:
			return nil, fmt.Errorf("ошибка получения модели по провайдеру: %w", err)
		}
	}

	// Создаем запись модели
	record := &create.UserModelRecord{
		ModelId:  modelId,
		AssistId: assistId,
		Provider: create.ProviderType(providerDb),
		IsActive: isActive,
		FileIds:  []create.Ids{},
	}

	// Парсим JSON с Ids
	if idsJson.Valid && idsJson.String != "" && idsJson.String != "null" {
		record.AllIds = []byte(idsJson.String)

		var vecIds create.VecIds
		if err := json.Unmarshal([]byte(idsJson.String), &vecIds); err != nil {
			return nil, fmt.Errorf("ошибка разбора Ids: %w", err)
		}
		record.FileIds = vecIds.FileIds
	}

	return record, nil
}

// GetModelByProviderAnyStatus получает модель пользователя по провайдеру НЕЗАВИСИМО от статуса активности
// В отличие от GetModelByProvider, эта функция не требует IsActive = 1
// Используется для обновления неактивных моделей
func (d *DB) GetModelByProviderAnyStatus(userId uint32, provider create.ProviderType) (*create.UserModelRecord, error) {
	// Проверяем входные значения
	if userId == 0 {
		return nil, fmt.Errorf("получен некорректный userId")
	}
	if !provider.IsValid() {
		return nil, fmt.Errorf("получен некорректный provider: %d", provider)
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// SQL запрос - БЕЗ условия IsActive = 1
	query := `
		SELECT 
			um.ModelId,
			ug.AssistantId,
			um.Provider,
			um.IsActive,
			ug.Ids
		FROM user_models um
		INNER JOIN user_gpt ug ON um.ModelId = ug.Id
		WHERE um.UserId = ? 
			AND um.Provider = ?
		LIMIT 1`

	var modelId uint64
	var assistId string
	var providerDb uint8
	var isActive bool
	var idsJson sql.NullString

	err := d.Conn().QueryRowContext(ctx, query, userId, uint8(provider)).Scan(
		&modelId,
		&assistId,
		&providerDb,
		&isActive,
		&idsJson,
	)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове GetModelByProviderAnyStatus: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil // Модель не найдена
		default:
			return nil, fmt.Errorf("ошибка получения модели по провайдеру: %w", err)
		}
	}

	// Создаем запись модели
	record := &create.UserModelRecord{
		ModelId:  modelId,
		AssistId: assistId,
		Provider: create.ProviderType(providerDb),
		IsActive: isActive,
		FileIds:  []create.Ids{},
	}

	// Парсим JSON с Ids
	if idsJson.Valid && idsJson.String != "" && idsJson.String != "null" {
		record.AllIds = []byte(idsJson.String)

		var vecIds create.VecIds
		if err := json.Unmarshal([]byte(idsJson.String), &vecIds); err != nil {
			return nil, fmt.Errorf("ошибка разбора Ids: %w", err)
		}
		record.FileIds = vecIds.FileIds
	}

	return record, nil
}

// SetActiveModel переключает активную модель пользователя
// Параметры:
//   - userId: ID пользователя
//   - modelId: ID записи из таблицы user_models
//
// Функция снимает IsActive с других моделей пользователя в этой же транзакции
func (d *DB) SetActiveModel(userId uint32, modelId uint64) error {
	if userId == 0 {
		return fmt.Errorf("получен пустой userId")
	}

	if modelId == 0 {
		return fmt.Errorf("получен пустой modelId")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	// Начинаем транзакцию
	tx, err := d.Conn().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ошибка начала транзакции: %w", err)
	}
	defer tx.Rollback()

	// Сначала снимаем IsActive со всех активных моделей этого пользователя
	_, err = tx.ExecContext(ctx,
		"UPDATE user_models SET IsActive = 0 WHERE UserId = ? AND IsActive = 1",
		userId)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при деактивации старых моделей: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка деактивации старых моделей: %w", err)
		}
	}

	// Обновляем IsActive для указанной модели
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

// SetActiveModelByProvider переключает активную модель пользователя для указанного провайдера
// Параметры:
//   - userId: ID пользователя
//   - provider: тип провайдера (ProviderOpenAI, ProviderMistral, ...)
//
// Функция снимает IsActive с других моделей пользователя в этой же транзакции
func (d *DB) SetActiveModelByProvider(userId uint32, provider create.ProviderType) error {
	if userId == 0 {
		return fmt.Errorf("получен пустой userId")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	// Начинаем транзакцию
	tx, err := d.Conn().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ошибка начала транзакции: %w", err)
	}
	defer tx.Rollback()

	// Сначала снимаем IsActive со ВСЕХ активных моделей пользователя (любого провайдера)
	_, err = tx.ExecContext(ctx,
		`UPDATE user_models 
		SET IsActive = 0 
		WHERE UserId = ? AND IsActive = 1`,
		userId)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при деактивации старой модели: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка деактивации старой модели: %w", err)
		}
	}

	// Обновляем IsActive для пользовательской модели указанного провайдера
	result, err := tx.ExecContext(ctx,
		`UPDATE user_models 
		SET IsActive = 1 
		WHERE UserId = ? AND Provider = ? 
		ORDER BY CreatedAt DESC 
		LIMIT 1`,
		userId, uint8(provider))

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
		return fmt.Errorf("пользовательская модель провайдера %s для пользователя %d не найдена", provider.String(), userId)
	}

	// Фиксируем транзакцию
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ошибка фиксации транзакции: %w", err)
	}

	return nil
}

// SetContactAvailability сохраняет доступность контакта в конкретном провайдере
func (d *DB) SetContactAvailability(userID uint32, contact, provider string, isAvailable bool) error {
	// Сначала получаем ContactId из service_contacts
	var contactID int64
	query := `SELECT Id FROM service_contacts WHERE UserId = ? AND Contact = ? LIMIT 1`
	err := d.Conn().QueryRow(query, userID, contact).Scan(&contactID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("контакт %s не найден в service_contacts для пользователя %d", contact, userID)
		}
		return fmt.Errorf("ошибка получения ContactId: %w", err)
	}

	// Сохраняем доступность через ContactId
	insertQuery := `
		INSERT INTO service_contact_availability 
			(ContactId, Provider, IsAvailable, CheckedAt, UpdatedAt)
		VALUES (?, ?, ?, NOW(), NOW())
		ON DUPLICATE KEY UPDATE 
			IsAvailable = VALUES(IsAvailable),
			UpdatedAt = NOW()
	`

	_, err = d.Conn().Exec(insertQuery, contactID, provider, isAvailable)
	if err != nil {
		return fmt.Errorf("ошибка сохранения доступности контакта: %w", err)
	}

	return nil
}

// GetContactAvailability получает доступность контакта во всех провайдерах
func (d *DB) GetContactAvailability(userID uint32, contact string) (map[string]bool, error) {
	query := `
		SELECT ca.Provider, ca.IsAvailable 
		FROM service_contact_availability ca
		INNER JOIN service_contacts c ON ca.ContactId = c.Id
		WHERE c.UserId = ? AND c.Contact = ?
	`

	rows, err := d.Conn().Query(query, userID, contact)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения доступности контакта: %w", err)
	}
	defer rows.Close()

	availability := make(map[string]bool)
	for rows.Next() {
		var provider string
		var isAvailable bool
		if err := rows.Scan(&provider, &isAvailable); err != nil {
			return nil, fmt.Errorf("ошибка чтения данных доступности: %w", err)
		}
		availability[provider] = isAvailable
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка итерации результатов: %w", err)
	}

	return availability, nil
}

// GetContactsAvailableIn получает список контактов доступных в указанном провайдере
func (d *DB) GetContactsAvailableIn(userID uint32, provider string) ([]string, error) {
	query := `
		SELECT DISTINCT c.Contact 
		FROM service_contact_availability ca
		INNER JOIN service_contacts c ON ca.ContactId = c.Id
		WHERE c.UserId = ? 
		  AND ca.Provider = ? 
		  AND ca.IsAvailable = 1
		ORDER BY c.Contact
	`

	rows, err := d.Conn().Query(query, userID, provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения контактов для провайдера %s: %w", provider, err)
	}
	defer rows.Close()

	var contacts []string
	for rows.Next() {
		var contact string
		if err := rows.Scan(&contact); err != nil {
			return nil, fmt.Errorf("ошибка чтения контакта: %w", err)
		}
		contacts = append(contacts, contact)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка итерации результатов: %w", err)
	}

	return contacts, nil
}

// GetContactsInBothProviders получает контакты доступные в обеих указанных платформах
func (d *DB) GetContactsInBothProviders(userID uint32, provider1, provider2 string) ([]string, error) {
	query := `
		SELECT c.Contact
		FROM service_contacts c
		INNER JOIN service_contact_availability ca1 ON c.Id = ca1.ContactId
		INNER JOIN service_contact_availability ca2 ON c.Id = ca2.ContactId
		WHERE c.UserId = ?
		  AND ca1.Provider = ?
		  AND ca1.IsAvailable = 1
		  AND ca2.Provider = ?
		  AND ca2.IsAvailable = 1
		ORDER BY c.Contact
	`

	rows, err := d.Conn().Query(query, userID, provider1, provider2)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения общих контактов: %w", err)
	}
	defer rows.Close()

	var contacts []string
	for rows.Next() {
		var contact string
		if err := rows.Scan(&contact); err != nil {
			return nil, fmt.Errorf("ошибка чтения контакта: %w", err)
		}
		contacts = append(contacts, contact)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка итерации результатов: %w", err)
	}

	return contacts, nil
}

// RemoveModelFromUser удаляет связь между пользователем и моделью в таблице user_models
// Также удаляет саму модель из user_gpt, если это была последняя связь с этой моделью
func (d *DB) RemoveModelFromUser(userId uint32, modelId uint64) error {
	// Проверяем входные значения
	if userId == 0 || modelId == 0 {
		return fmt.Errorf("получены некорректные значения: userId или modelId равны 0")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	// Начинаем транзакцию для атомарности операций
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ошибка начала транзакции: %w", err)
	}
	defer tx.Rollback()

	// Проверяем, существует ли связь пользователя с моделью
	var exists bool
	err = tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM user_models WHERE UserId = ? AND ModelId = ?)",
		userId, modelId).Scan(&exists)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при проверке связи пользователя с моделью: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена при проверке связи: %w", err)
		default:
			return fmt.Errorf("ошибка проверки связи пользователя с моделью: %w", err)
		}
	}

	if !exists {
		return fmt.Errorf("связь между пользователем %d и моделью %d не найдена", userId, modelId)
	}

	// Проверяем, была ли эта модель активной
	var wasActive bool
	err = tx.QueryRowContext(ctx,
		"SELECT IsActive FROM user_models WHERE UserId = ? AND ModelId = ?",
		userId, modelId).Scan(&wasActive)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при проверке активности модели: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена при проверке активности: %w", err)
		default:
			return fmt.Errorf("ошибка проверки активности модели: %w", err)
		}
	}

	// Удаляем связь между пользователем и моделью
	_, err = tx.ExecContext(ctx,
		"DELETE FROM user_models WHERE UserId = ? AND ModelId = ?",
		userId, modelId)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при удалении связи: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена при удалении связи: %w", err)
		default:
			return fmt.Errorf("ошибка удаления связи пользователя с моделью: %w", err)
		}
	}

	// Проверяем, есть ли у этой модели другие связи с пользователями
	var otherUsersCount int
	err = tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM user_models WHERE ModelId = ?",
		modelId).Scan(&otherUsersCount)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при проверке других связей модели: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена при проверке других связей: %w", err)
		default:
			return fmt.Errorf("ошибка проверки других связей модели: %w", err)
		}
	}

	// Если других связей нет, удаляем саму модель из user_gpt
	if otherUsersCount == 0 {
		_, err = tx.ExecContext(ctx, "DELETE FROM user_gpt WHERE Id = ?", modelId)
		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("тайм-аут (%d с) при удалении модели: %w", sqlTimeToCancel, err)
			case errors.Is(err, context.Canceled):
				return fmt.Errorf("операция отменена при удалении модели: %w", err)
			default:
				return fmt.Errorf("ошибка удаления модели: %w", err)
			}
		}
	}

	// Если удалённая модель была активной, нужно активировать другую модель (если есть)
	if wasActive {
		// Получаем первую доступную модель пользователя
		var nextModelId sql.NullInt64
		err = tx.QueryRowContext(ctx,
			"SELECT ModelId FROM user_models WHERE UserId = ? LIMIT 1",
			userId).Scan(&nextModelId)

		// Если есть другая модель, делаем её активной
		if err == nil && nextModelId.Valid {
			_, err = tx.ExecContext(ctx,
				"UPDATE user_models SET IsActive = 1 WHERE UserId = ? AND ModelId = ?",
				userId, nextModelId.Int64)
			if err != nil {
				return fmt.Errorf("ошибка активации следующей модели: %w", err)
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			// Если других моделей нет - отключаем все каналы пользователя
			// Фиксируем транзакцию перед вызовом DisableAllUserChannel
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("ошибка фиксации транзакции: %w", err)
			}

			// Отключаем все каналы, так как у пользователя больше нет моделей
			if err := d.DisableAllUserChannel(userId); err != nil {
				return fmt.Errorf("ошибка отключения каналов пользователя: %w", err)
			}

			return nil
		}
	}

	// Фиксируем транзакцию
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ошибка фиксации транзакции: %w", err)
	}

	return nil
}

func (d *DB) SaveUserModel(
	userId uint32, provider create.ProviderType, name, assistantId string, data []byte, modType uint8, ids json.RawMessage, operator bool) error {
	// Проверяю входные значения
	if userId == 0 || name == "" || assistantId == "" {
		return fmt.Errorf("получены некорректные значения: userId, name или assistantId пусты")
	}
	// Валидация провайдера
	if provider != create.ProviderOpenAI && provider != create.ProviderMistral && provider != create.ProviderGoogle {
		return fmt.Errorf("некорректный provider: %d (допустимы 1=OpenAI, 2=Mistral, 3=Google)", provider)
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	// Начинаем транзакцию для атомарности операций
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ошибка начала транзакции: %w", err)
	}
	defer tx.Rollback()

	// ===================================================================
	// Шаг 1: Сохранение/обновление модели в user_gpt
	// ===================================================================

	// Проверяем, существует ли модель для данного пользователя и провайдера
	var existingModelId sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT ug.Id 
		FROM user_gpt ug
		INNER JOIN user_models um ON ug.Id = um.ModelId
		WHERE um.UserId = ? AND um.Provider = ?
		LIMIT 1
	`, userId, provider).Scan(&existingModelId)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при проверке существующей модели: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка проверки существующей модели: %w", err)
		}
	}

	var modelId int64

	if !existingModelId.Valid {
		// ===================================================================
		// Модели нет - создаём новую в user_gpt
		// ===================================================================
		result, err := tx.ExecContext(ctx, `
			INSERT INTO user_gpt (Name, Model, Provider, AssistantId, Data, Ids)
			VALUES (?, ?, ?, ?, ?, ?)
		`, name, modType, provider, assistantId, data, ids)

		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("тайм-аут (%d с) при создании модели: %w", sqlTimeToCancel, err)
			case errors.Is(err, context.Canceled):
				return fmt.Errorf("операция отменена при создании модели: %w", err)
			default:
				return fmt.Errorf("ошибка создания модели: %w", err)
			}
		}

		// Получаем ID новой записи
		modelId, err = result.LastInsertId()
		if err != nil {
			return fmt.Errorf("ошибка получения ID новой модели: %w", err)
		}

		// ===================================================================
		// Шаг 2: Создание связи в user_models
		// ===================================================================

		// Проверяем, есть ли у пользователя другие модели
		var modelCount int
		err = tx.QueryRowContext(ctx, `
			SELECT COUNT(*) 
			FROM user_models 
			WHERE UserId = ?
		`, userId).Scan(&modelCount)

		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("тайм-аут (%d с) при подсчёте моделей: %w", sqlTimeToCancel, err)
			case errors.Is(err, context.Canceled):
				return fmt.Errorf("операция отменена при подсчёте моделей: %w", err)
			default:
				return fmt.Errorf("ошибка подсчёта моделей: %w", err)
			}
		}

		// Если это первая модель - делаем её активной автоматически
		isActive := 0
		if modelCount == 0 {
			isActive = 1
		}

		// Создаём связь в user_models
		_, err = tx.ExecContext(ctx, `
			INSERT INTO user_models (UserId, ModelId, Provider, IsActive)
			VALUES (?, ?, ?, ?)
		`, userId, modelId, provider, isActive)

		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("тайм-аут (%d с) при создании связи модели: %w", sqlTimeToCancel, err)
			case errors.Is(err, context.Canceled):
				return fmt.Errorf("операция отменена при создании связи: %w", err)
			default:
				return fmt.Errorf("ошибка создания связи модели: %w", err)
			}
		}

		logger.Info("Создана новая модель (provider: %d, modelId: %d, isActive: %v)", provider, modelId, isActive == 1, userId)

	} else {
		// ===================================================================
		// Модель существует - обновляем её в user_gpt
		// ===================================================================
		modelId = existingModelId.Int64

		_, err = tx.ExecContext(ctx, `
			UPDATE user_gpt
			SET Name = ?,
				Model = ?,
				AssistantId = ?,
				Data = ?,
				Ids = ?
			WHERE Id = ?
		`, name, modType, assistantId, data, ids, modelId)

		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("тайм-аут (%d с) при обновлении модели: %w", sqlTimeToCancel, err)
			case errors.Is(err, context.Canceled):
				return fmt.Errorf("операция отменена при обновлении модели: %w", err)
			default:
				return fmt.Errorf("ошибка обновления модели: %w", err)
			}
		}

		logger.Info("Обновлена существующая модель (provider: %d, modelId: %d)", provider, modelId, userId)
	}

	// ===================================================================
	// Шаг 3: Обновление статуса оператора
	// ===================================================================
	enabledInt := 0
	if operator {
		enabledInt = 1
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE operators
		SET Telegram_enabled = ?,
			Changed = 1,
			Timechange = CURRENT_TIMESTAMP()
		WHERE UserId = ?
	`, enabledInt, userId)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при установке статуса оператора: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена при установке статуса оператора: %w", err)
		default:
			return fmt.Errorf("ошибка установки статуса оператора: %w", err)
		}
	}

	// ===================================================================
	// Финал: Фиксируем транзакцию
	// ===================================================================
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ошибка фиксации транзакции: %w", err)
	}

	return nil
}

// ReadUserModel получает данные модели пользователя и идентификаторы файлов
func (d *DB) ReadUserModel(userId uint32) ([]byte, *create.VecIds, error) {
	// Проверяем входное значение
	if userId == 0 {
		return nil, nil, fmt.Errorf("получен некорректный userId")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	// SQL запрос для получения Data и Ids из user_gpt через активную модель
	query := `
		SELECT TO_BASE64(ug.Data), ug.Ids
		FROM user_models um
		JOIN user_gpt ug ON um.ModelId = ug.Id
		WHERE um.UserId = ? AND um.IsActive = 1`

	var base64Data sql.NullString
	var idsJson sql.NullString

	err := d.conn.QueryRowContext(ctx, query, userId).Scan(&base64Data, &idsJson)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, nil, fmt.Errorf("тайм-аут (%d с) при вызове ReadUserModel: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil, nil // Модель не найдена, но это не ошибка
		default:
			return nil, nil, fmt.Errorf("ошибка получения данных ReadUserModel: %w", err)
		}
	}

	// Проверяем на пустой результат или null
	if !base64Data.Valid || base64Data.String == "" {
		return nil, nil, nil // Модель не найдена
	}

	// Инициализируем структуру VecIds по умолчанию с пустыми массивами
	vecIds := &create.VecIds{
		VectorId: []string{},
		FileIds:  []create.Ids{},
	}

	// Проверяем и парсим Ids, если они есть
	if idsJson.Valid && idsJson.String != "" && idsJson.String != "null" {
		if err := json.Unmarshal([]byte(idsJson.String), vecIds); err != nil {
			return nil, nil, fmt.Errorf("ошибка разбора Ids: %w", err)
		}
	}

	// Декодируем base64 данные
	decodedData, err := base64.StdEncoding.DecodeString(base64Data.String)
	if err != nil {
		return nil, nil, fmt.Errorf("ошибка декодирования base64: %w", err)
	}

	return decodedData, vecIds, nil
}

func (d *DB) GetOrSetUserStorageLimit(userID uint32, setStorage int64) (remaining uint64, totalLimit uint64, err error) {
	// Проверяем входное значение
	if userID == 0 {
		return 0, 0, fmt.Errorf("получен некорректный userID")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	// Начинаем транзакцию
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("ошибка начала транзакции: %w", err)
	}
	defer tx.Rollback()

	// Получаем текущие значения с блокировкой строки
	var vLimit, vUsed int64
	err = tx.QueryRowContext(ctx, `
  SELECT StorageLimit, StorageUsed
  FROM subscriptions
  WHERE UserId = ?
  FOR UPDATE`, userID).Scan(&vLimit, &vUsed)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, 0, fmt.Errorf("тайм-аут (%d с) при получении лимитов хранилища: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return 0, 0, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return 0, 0, fmt.Errorf("подписка пользователя не найдена")
		default:
			return 0, 0, fmt.Errorf("ошибка получения лимитов хранилища: %w", err)
		}
	}

	// Вычисляем новое значение занятости
	vNewUsed := vUsed + setStorage

	// Гарантируем границы: [0, StorageLimit]
	if vNewUsed < 0 {
		vNewUsed = 0
	} else if vNewUsed > vLimit {
		vNewUsed = vLimit
	}

	// Обновляем значение StorageUsed
	_, err = tx.ExecContext(ctx, `
  UPDATE subscriptions
  SET StorageUsed = ?
  WHERE UserId = ?`, vNewUsed, userID)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, 0, fmt.Errorf("тайм-аут (%d с) при обновлении использования хранилища: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return 0, 0, fmt.Errorf("операция отменена: %w", err)
		default:
			return 0, 0, fmt.Errorf("ошибка обновления использования хранилища: %w", err)
		}
	}

	// Фиксируем транзакцию
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("ошибка фиксации транзакции: %w", err)
	}

	// Вычисляем оставшееся место и возвращаем результат
	remaining = uint64(vLimit - vNewUsed)
	totalLimit = uint64(vLimit)

	return remaining, totalLimit, nil
}
