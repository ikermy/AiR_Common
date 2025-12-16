package comdb

import (
	"context"
	"log"
	"os"
	"testing"

	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/logger"
	models "github.com/ikermy/AiR_Common/pkg/model/create"
)

func getConfig() *conf.Conf {
	// Переходим в корневую директорию проекта
	if err := os.Chdir("../.."); err != nil {
		log.Fatalf("Failed to change to root directory: %v", err)
	}

	cfg, err := conf.NewConf()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Инициализируем логгер для тестов
	const LogPatch = "/var/log/Marusia_TEST/Comm.log"
	logger.Set(LogPatch)

	return cfg
}

// TestReadUserModels_Integration - интеграционный тест для чтения моделей пользователя 23
// Требует реальное подключение к БД
func TestReadUserModels_Integration(t *testing.T) {
	cfg := getConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Подключаемся к БД
	db := New(ctx, cfg)
	defer db.Close()

	// Создаём Models manager
	modelsManager := models.New(ctx, db, cfg.GPT.OpenAIKey, cfg.GPT.MistralKey)

	// ID пользователя для теста
	const testUserId uint32 = 23

	t.Run("GetUserModels - получить все модели пользователя 23", func(t *testing.T) {
		userModels, err := modelsManager.GetUserModels(testUserId)
		if err != nil {
			t.Fatalf("Ошибка получения моделей: %v", err)
		}

		t.Logf("Найдено моделей: %d", len(userModels))

		for i, model := range userModels {
			t.Logf("\n=== Модель #%d ===", i+1)
			t.Logf("Provider: %s (%d)", model.Provider.String(), model.Provider)
			t.Logf("Name: %s", model.Name)
			t.Logf("Model: %s", model.Model)
			t.Logf("Instructions: %.100s...", model.Instructions)
			t.Logf("MetaAction: %s", model.MetaAction)
			t.Logf("Triggers: %v", model.Triggers)
			t.Logf("FileIds count: %d", len(model.FileIds))
			t.Logf("VectorIds count: %d", len(model.VectorIds))
			t.Logf("S3Enabled: %v", model.S3Enabled)
			t.Logf("Operator: %v", model.Operator)
			t.Logf("Search: %v", model.Search)
			t.Logf("Interpreter: %v", model.Interpreter)
			t.Logf("S3: %v", model.S3)
		}

		if len(userModels) == 0 {
			t.Log("⚠️  У пользователя 23 нет моделей в БД")
		}
	})

	t.Run("GetActiveUserModel - получить активную модель пользователя 23", func(t *testing.T) {
		activeModel, err := modelsManager.GetActiveUserModel(testUserId)
		if err != nil {
			t.Fatalf("Ошибка получения активной модели: %v", err)
		}

		if activeModel == nil {
			t.Log("⚠️  У пользователя 23 нет активной модели")
			return
		}

		t.Log("\n=== Активная модель ===")
		t.Logf("Provider: %s (%d)", activeModel.Provider.String(), activeModel.Provider)
		t.Logf("Name: %s", activeModel.Name)
		t.Logf("Model: %s", activeModel.Model)
		t.Logf("Instructions: %.100s...", activeModel.Instructions)
		t.Logf("FileIds count: %d", len(activeModel.FileIds))
		t.Logf("S3Enabled: %v", activeModel.S3Enabled)
	})

	t.Run("GetUserModelByProvider - получить модель OpenAI пользователя 23", func(t *testing.T) {
		openaiModel, err := modelsManager.GetUserModelByProvider(testUserId, models.ProviderOpenAI)
		if err != nil {
			t.Fatalf("Ошибка получения модели OpenAI: %v", err)
		}

		if openaiModel == nil {
			t.Log("⚠️  У пользователя 23 нет модели OpenAI")
			return
		}

		t.Log("\n=== Модель OpenAI ===")
		t.Logf("Provider: %s", openaiModel.Provider.String())
		t.Logf("Name: %s", openaiModel.Name)
		t.Logf("Model: %s", openaiModel.Model)
		t.Logf("Instructions: %.200s", openaiModel.Instructions)
	})

	t.Run("GetUserModelByProvider - получить модель Mistral пользователя 23", func(t *testing.T) {
		mistralModel, err := modelsManager.GetUserModelByProvider(testUserId, models.ProviderMistral)
		if err != nil {
			t.Fatalf("Ошибка получения модели Mistral: %v", err)
		}

		if mistralModel == nil {
			t.Log("⚠️  У пользователя 23 нет модели Mistral")
			return
		}

		t.Log("\n=== Модель Mistral ===")
		t.Logf("Provider: %s", mistralModel.Provider.String())
		t.Logf("Name: %s", mistralModel.Name)
		t.Logf("Model: %s", mistralModel.Model)
	})

	t.Run("ReadModel - получить активную модель через ReadModel", func(t *testing.T) {
		model, err := modelsManager.ReadModel(testUserId, nil)
		if err != nil {
			t.Fatalf("Ошибка ReadModel: %v", err)
		}

		if model == nil {
			t.Log("⚠️  ReadModel вернул nil (нет активной модели)")
			return
		}

		t.Log("\n=== ReadModel (активная модель) ===")
		t.Logf("Provider: %s", model.Provider.String())
		t.Logf("Name: %s", model.Name)
	})

	t.Run("GetModelAsJSON - получить модель как JSON", func(t *testing.T) {
		jsonData, err := modelsManager.GetModelAsJSON(testUserId, nil)
		if err != nil {
			t.Fatalf("Ошибка GetModelAsJSON: %v", err)
		}

		if string(jsonData) == "{}" {
			t.Log("⚠️  GetModelAsJSON вернул пустой объект")
			return
		}

		t.Log("\n=== GetModelAsJSON ===")
		t.Logf("JSON длина: %d байт", len(jsonData))
		t.Logf("JSON (первые 500 символов): %.500s", string(jsonData))
	})
}

// TestReadUserModels_DBMethods - тест низкоуровневых методов БД для пользователя 23
func TestReadUserModels_DBMethods(t *testing.T) {
	cfg := getConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Подключаемся к БД
	db := New(ctx, cfg)
	defer db.Close()

	const testUserId uint32 = 23

	t.Run("GetUserModels - метод БД", func(t *testing.T) {
		records, err := db.GetUserModels(testUserId)
		if err != nil {
			t.Fatalf("Ошибка GetUserModels: %v", err)
		}

		t.Logf("Найдено записей в user_models: %d", len(records))

		for i, record := range records {
			t.Logf("Запись #%d: UserId=%d, ModelId=%d, Provider=%d (%s), IsActive=%v",
				i+1, record.UserId, record.ModelId, record.Provider, record.Provider.String(), record.IsActive)
		}
	})

	t.Run("GetActiveModel - метод БД", func(t *testing.T) {
		record, err := db.GetActiveModel(testUserId)
		if err != nil {
			t.Fatalf("Ошибка GetActiveModel: %v", err)
		}

		if record == nil {
			t.Log("⚠️  Активная модель не найдена")
			return
		}

		t.Logf("Активная модель: ModelId=%d, Provider=%d (%s), IsActive=%v",
			record.ModelId, record.Provider, record.Provider.String(), record.IsActive)
	})

	t.Run("GetModelByProvider - OpenAI", func(t *testing.T) {
		record, err := db.GetModelByProvider(testUserId, models.ProviderOpenAI)
		if err != nil {
			t.Fatalf("Ошибка GetModelByProvider(OpenAI): %v", err)
		}

		if record == nil {
			t.Log("⚠️  Модель OpenAI не найдена")
			return
		}

		t.Logf("OpenAI модель: ModelId=%d, IsActive=%v", record.ModelId, record.IsActive)
	})

	t.Run("ReadUserModelByProvider - OpenAI", func(t *testing.T) {
		compressedData, vecIds, err := db.ReadUserModelByProvider(testUserId, models.ProviderOpenAI)
		if err != nil {
			t.Fatalf("Ошибка ReadUserModelByProvider(OpenAI): %v", err)
		}

		if compressedData == nil {
			t.Log("⚠️  Данные модели OpenAI не найдены")
			return
		}

		t.Logf("OpenAI модель данные:")
		t.Logf("  - CompressedData size: %d байт", len(compressedData))
		if vecIds != nil {
			t.Logf("  - FileIds count: %d", len(vecIds.FileIds))
			t.Logf("  - VectorIds count: %d", len(vecIds.VectorId))
		}

		// Проверяем распаковку через DecompressAndExtractMetadata
		metaAction, triggers, espero, err := DecompressAndExtractMetadata(compressedData)
		if err != nil {
			t.Fatalf("Ошибка распаковки: %v", err)
		}

		t.Logf("  - MetaAction: %s", metaAction)
		t.Logf("  - Triggers: %v", triggers)
		if espero != nil {
			t.Logf("  - Espero: {Limit: %d, Wait: %d, Ignore: %v}", espero.Limit, espero.Wait, espero.Ignore)
		}
	})

	t.Run("ReadUserModelByProvider - Mistral", func(t *testing.T) {
		compressedData, vecIds, err := db.ReadUserModelByProvider(testUserId, models.ProviderMistral)
		if err != nil {
			t.Fatalf("Ошибка ReadUserModelByProvider(Mistral): %v", err)
		}

		if compressedData == nil {
			t.Log("⚠️  Данные модели Mistral не найдены")
			return
		}

		t.Logf("Mistral модель данные:")
		t.Logf("  - CompressedData size: %d байт", len(compressedData))
		if vecIds != nil {
			t.Logf("  - FileIds count: %d", len(vecIds.FileIds))
			t.Logf("  - VectorIds count: %d", len(vecIds.VectorId))
		}
	})
}
