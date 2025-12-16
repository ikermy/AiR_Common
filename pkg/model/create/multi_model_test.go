package models

import (
	"encoding/json"
	"testing"
)

// MockDB - мок-реализация DB интерфейса для тестирования
type MockDB struct {
	models       map[uint32]map[ProviderType]*mockModelData
	userModels   map[uint32][]UserModelRecord
	activeModels map[uint32]uint64 // userId -> modelId активной модели
	nextModelId  uint64
}

type mockModelData struct {
	data   []byte
	vecIds *VecIds
}

func NewMockDB() *MockDB {
	return &MockDB{
		models:       make(map[uint32]map[ProviderType]*mockModelData),
		userModels:   make(map[uint32][]UserModelRecord),
		activeModels: make(map[uint32]uint64),
		nextModelId:  0,
	}
}

func (db *MockDB) SaveUserModel(userId uint32, _ string, _ string, data []byte, _ uint8, ids json.RawMessage, _ bool, provider ProviderType) error {
	if db.models[userId] == nil {
		db.models[userId] = make(map[ProviderType]*mockModelData)
	}

	var vecIds VecIds
	if ids != nil {
		_ = json.Unmarshal(ids, &vecIds)
	}

	// Сохраняем данные модели
	db.models[userId][provider] = &mockModelData{
		data:   data,
		vecIds: &vecIds,
	}

	// Эмулируем создание связи в user_models (как в реальной БД)
	existingRecord := false
	for _, record := range db.userModels[userId] {
		if record.Provider == provider {
			existingRecord = true
			break
		}
	}

	// Если связи нет - создаём её
	if !existingRecord {
		// Генерируем новый ID модели
		db.nextModelId++
		modelId := db.nextModelId

		// Проверяем, есть ли у пользователя другие модели
		isFirstModel := len(db.userModels[userId]) == 0

		// Создаём запись в user_models
		record := UserModelRecord{
			UserId:   userId,
			ModelId:  modelId,
			Provider: provider,
			IsActive: isFirstModel, // Первая модель автоматически активная
		}

		db.userModels[userId] = append(db.userModels[userId], record)

		if isFirstModel {
			db.activeModels[userId] = modelId
		}
	}

	return nil
}

func (db *MockDB) ReadUserModelByProvider(userId uint32, provider ProviderType) ([]byte, *VecIds, error) {
	if db.models[userId] == nil {
		return nil, nil, nil
	}

	return nil, nil, nil
}

func (db *MockDB) RemoveModelFromUser(userId uint32, modelId uint64) error {
	// Удаляем связь из user_models
	if models, exists := db.userModels[userId]; exists {
		for i, m := range models {
			if m.ModelId == modelId {
				db.userModels[userId] = append(models[:i], models[i+1:]...)
				break
			}
		}
		// Если больше нет моделей - удаляем пользователя
		if len(db.userModels[userId]) == 0 {
			delete(db.userModels, userId)
		}
	}

	// Проверяем, используется ли модель другими пользователями
	modelUsed := false
	for _, models := range db.userModels {
		for _, m := range models {
			if m.ModelId == modelId {
				modelUsed = true
				break
			}
		}
		if modelUsed {
			break
		}
	}

	// Если модель не используется - удаляем из models
	if !modelUsed {
		delete(db.models, userId)
	}

	// Удаляем из активных если была активной
	if activeModelId, exists := db.activeModels[userId]; exists && activeModelId == modelId {
		delete(db.activeModels, userId)
	}

	return nil
}

func (db *MockDB) GetUserVectorStorage(_ uint32) (string, error) {
	return "", nil
}

func (db *MockDB) GetOrSetUserStorageLimit(_ uint32, _ int64) (remaining uint64, totalLimit uint64, err error) {
	return 1000000, 10000000, nil
}

func (db *MockDB) GetAllUserModels(userId uint32) ([]UserModelRecord, error) {
	return db.userModels[userId], nil
}

func (db *MockDB) GetActiveModel(userId uint32) (*UserModelRecord, error) {
	activeModelId := db.activeModels[userId]
	if activeModelId == 0 {
		return nil, nil
	}

	for _, record := range db.userModels[userId] {
		if record.ModelId == activeModelId {
			return &record, nil
		}
	}
	return nil, nil
}

func (db *MockDB) GetModelByProvider(userId uint32, provider ProviderType) (*UserModelRecord, error) {
	for _, record := range db.userModels[userId] {
		if record.Provider == provider {
			return &record, nil
		}
	}
	return nil, nil
}

func (db *MockDB) SetActiveModel(userId uint32, modelId uint64) error {
	// Снимаем IsActive со всех моделей
	for i := range db.userModels[userId] {
		db.userModels[userId][i].IsActive = false
	}

	// Устанавливаем на целевую
	for i := range db.userModels[userId] {
		if db.userModels[userId][i].ModelId == modelId {
			db.userModels[userId][i].IsActive = true
			db.activeModels[userId] = modelId
			return nil
		}
	}

	return nil
}

// TestAutoActiveModel проверяет автоматическую установку первой модели как активной
func TestAutoActiveModel(t *testing.T) {
	mockDB := NewMockDB()
	models := &UniversalModel{db: mockDB}

	userId := uint32(1)
	modelData := &UniversalModelData{
		Provider:     ProviderOpenAI,
		ModelID:      "asst_test123",
		ModelName:    "Test UniversalModel",
		ModelType:    1,
		Instructions: "Test instructions",
	}

	err := models.SaveModel(userId, modelData)
	if err != nil {
		t.Fatalf("Ошибка сохранения модели: %v", err)
	}

	// Проверяем что модель стала активной
	activeModel, err := mockDB.GetActiveModel(userId)
	if err != nil {
		t.Fatalf("Ошибка получения активной модели: %v", err)
	}

	if activeModel == nil {
		t.Fatal("Активная модель не установлена автоматически")
	}

	if !activeModel.IsActive {
		t.Error("Первая модель должна быть активной автоматически")
	}
}

// TestSwitchActiveModel проверяет переключение активной модели
func TestSwitchActiveModel(t *testing.T) {
	mockDB := NewMockDB()
	models := &UniversalModel{db: mockDB}

	userId := uint32(2)

	// Создаём первую модель OpenAI
	model1 := &UniversalModelData{
		Provider:  ProviderOpenAI,
		ModelID:   "asst_openai",
		ModelName: "OpenAI UniversalModel",
		ModelType: 1,
	}

	err := models.SaveModel(userId, model1)
	if err != nil {
		t.Fatalf("Ошибка сохранения модели OpenAI: %v", err)
	}

	// Создаём вторую модель Mistral
	model2 := &UniversalModelData{
		Provider:  ProviderMistral,
		ModelID:   "ag_mistral",
		ModelName: "Mistral UniversalModel",
		ModelType: 1,
	}

	// Используем UniversalModel.SaveModel для правильного сжатия данных
	err = models.SaveModel(userId, model2)
	if err != nil {
		t.Fatalf("Ошибка сохранения модели Mistral: %v", err)
	}

	// Переключаем активную модель на Mistral
	err = models.SetActiveUserModel(userId, 2)
	if err != nil {
		t.Fatalf("Ошибка переключения активной модели: %v", err)
	}

	// Проверяем что Mistral стала активной
	activeModel, err := mockDB.GetActiveModel(userId)
	if err != nil {
		t.Fatalf("Ошибка получения активной модели: %v", err)
	}

	if activeModel == nil {
		t.Fatal("Активная модель не найдена после переключения")
	}

	if activeModel.Provider != ProviderMistral {
		t.Errorf("Ожидалась активная модель Mistral, получена %s", activeModel.Provider)
	}
}

// TestGetUserModels проверяет получение всех моделей пользователя
func TestGetUserModels(t *testing.T) {
	mockDB := NewMockDB()
	models := &UniversalModel{db: mockDB}

	userId := uint32(3)

	// Создаём модель OpenAI
	model1 := &UniversalModelData{
		Provider:  ProviderOpenAI,
		ModelID:   "asst_1",
		ModelName: "OpenAI UniversalModel",
		ModelType: 1,
	}

	err := models.SaveModel(userId, model1)
	if err != nil {
		t.Fatalf("Ошибка сохранения модели OpenAI: %v", err)
	}

	// Создаём модель Mistral
	model2 := &UniversalModelData{
		Provider:  ProviderMistral,
		ModelID:   "ag_1",
		ModelName: "Mistral UniversalModel",
		ModelType: 1,
	}

	err = models.SaveModel(userId, model2)
	if err != nil {
		t.Fatalf("Ошибка сохранения модели Mistral: %v", err)
	}

	// Получаем все модели
	userModels, err := models.GetUserModels(userId)
	if err != nil {
		t.Fatalf("Ошибка получения моделей: %v", err)
	}

	if len(userModels) != 2 {
		t.Errorf("Ожидалось 2 модели, получено %d", len(userModels))
	}
}

// TestReadModelWithProvider проверяет чтение модели по провайдеру
func TestReadModelWithProvider(t *testing.T) {
	mockDB := NewMockDB()
	models := &UniversalModel{db: mockDB}

	userId := uint32(4)

	// Создаём модель OpenAI
	model1 := &UniversalModelData{
		Provider:     ProviderOpenAI,
		ModelID:      "asst_openai",
		ModelName:    "OpenAI",
		ModelType:    1,
		Instructions: "OpenAI instructions",
	}

	err := models.SaveModel(userId, model1)
	if err != nil {
		t.Fatalf("Ошибка сохранения модели OpenAI: %v", err)
	}

	// Создаём модель Mistral
	model2 := &UniversalModelData{
		Provider:     ProviderMistral,
		ModelID:      "ag_mistral",
		ModelName:    "Mistral",
		ModelType:    1,
		Instructions: "Mistral instructions",
	}

	err = models.SaveModel(userId, model2)
	if err != nil {
		t.Fatalf("Ошибка сохранения модели Mistral: %v", err)
	}

	// Получаем модель OpenAI
	providerOpenAI := ProviderOpenAI
	modelOpenAI, err := models.ReadModel(userId, &providerOpenAI)
	if err != nil {
		t.Fatalf("Ошибка чтения модели OpenAI: %v", err)
	}

	if modelOpenAI == nil || modelOpenAI.Provider != ProviderOpenAI {
		t.Error("Не удалось получить модель OpenAI по провайдеру")
	}

	// Получаем модель Mistral
	providerMistral := ProviderMistral
	modelMistral, err := models.ReadModel(userId, &providerMistral)
	if err != nil {
		t.Fatalf("Ошибка чтения модели Mistral: %v", err)
	}

	if modelMistral == nil || modelMistral.Provider != ProviderMistral {
		t.Error("Не удалось получить модель Mistral по провайдеру")
	}

	// Получаем активную модель (без указания провайдера)
	activeModel, err := models.ReadModel(userId, nil)
	if err != nil {
		t.Fatalf("Ошибка чтения активной модели: %v", err)
	}

	if activeModel == nil || activeModel.Provider != ProviderOpenAI {
		t.Error("Активная модель должна быть OpenAI")
	}
}
