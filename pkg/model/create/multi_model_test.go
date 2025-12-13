package models

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
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

func (db *MockDB) SaveUserModel(userId uint32, _ string, _ string, data []byte, _ uint8, ids json.RawMessage, _ bool) error {
	if db.models[userId] == nil {
		db.models[userId] = make(map[ProviderType]*mockModelData)
	}

	var vecIds VecIds
	if ids != nil {
		_ = json.Unmarshal(ids, &vecIds)
	}

	// Определяем провайдера из распакованных данных
	// Распаковываем данные чтобы понять провайдера
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err == nil {
		defer func() { _ = reader.Close() }()
		decompressed, _ := io.ReadAll(reader)
		var modelData map[string]interface{}
		if json.Unmarshal(decompressed, &modelData) == nil {
			if providerStr, ok := modelData["provider"].(string); ok {
				var provider ProviderType
				if providerStr == "mistral" {
					provider = ProviderMistral
				} else {
					provider = ProviderOpenAI
				}

				db.models[userId][provider] = &mockModelData{
					data:   data,
					vecIds: &vecIds,
				}
				return nil
			}
		}
	}

	// Fallback - OpenAI
	db.models[userId][ProviderOpenAI] = &mockModelData{
		data:   data,
		vecIds: &vecIds,
	}
	return nil
}

func (db *MockDB) ReadUserModel(userId uint32) ([]byte, *VecIds, error) {
	if db.models[userId] == nil {
		return nil, nil, nil
	}

	// Возвращаем первую найденную модель (используется только для обратной совместимости)
	// Новый код должен использовать GetActiveModel + ReadUserModelByProvider
	for _, modelData := range db.models[userId] {
		return modelData.data, modelData.vecIds, nil
	}

	return nil, nil, nil
}

func (db *MockDB) ReadUserModelByProvider(userId uint32, provider ProviderType) ([]byte, *VecIds, error) {
	if db.models[userId] == nil {
		return nil, nil, nil
	}

	if modelData := db.models[userId][provider]; modelData != nil {
		return modelData.data, modelData.vecIds, nil
	}

	return nil, nil, nil
}

func (db *MockDB) DeleteUserGPT(userId uint32) error {
	delete(db.models, userId)
	delete(db.userModels, userId)
	delete(db.activeModels, userId)
	return nil
}

func (db *MockDB) GetUserGPT(_ uint32) (json.RawMessage, error) {
	return json.RawMessage("{}"), nil
}

func (db *MockDB) GetUserVectorStorage(_ uint32) (string, error) {
	return "", nil
}

func (db *MockDB) GetOrSetUserStorageLimit(_ uint32, _ int64) (remaining uint64, totalLimit uint64, err error) {
	return 1000000, 10000000, nil
}

func (db *MockDB) GetUserModels(userId uint32) ([]UserModelRecord, error) {
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

func (db *MockDB) AddModelToUser(userId uint32, modelId uint64, provider ProviderType, isActive bool) error {
	record := UserModelRecord{
		UserId:   userId,
		ModelId:  modelId,
		Provider: provider,
		IsActive: isActive,
	}

	db.userModels[userId] = append(db.userModels[userId], record)

	if isActive {
		db.activeModels[userId] = modelId
	}

	return nil
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

func (db *MockDB) RemoveModelFromUser(userId uint32, modelId uint64) error {
	for i, record := range db.userModels[userId] {
		if record.ModelId == modelId {
			db.userModels[userId] = append(db.userModels[userId][:i], db.userModels[userId][i+1:]...)
			break
		}
	}
	return nil
}

func (db *MockDB) GetModelIdByUserAndProvider(userId uint32, provider ProviderType) (uint64, error) {
	// Сначала ищем существующую модель по провайдеру
	for _, record := range db.userModels[userId] {
		if record.Provider == provider {
			return record.ModelId, nil
		}
	}

	// Если модели нет - создаём новый ID
	db.nextModelId++
	return db.nextModelId, nil
}

// TestAutoActiveModel проверяет автоматическую установку первой модели как активной
func TestAutoActiveModel(t *testing.T) {
	mockDB := NewMockDB()
	models := &Models{db: mockDB}

	userId := uint32(1)
	modelData := &UniversalModelData{
		Provider:     ProviderOpenAI,
		ModelID:      "asst_test123",
		ModelName:    "Test Model",
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
	models := &Models{db: mockDB}

	userId := uint32(2)

	// Создаём первую модель OpenAI
	model1 := &UniversalModelData{
		Provider:  ProviderOpenAI,
		ModelID:   "asst_openai",
		ModelName: "OpenAI Model",
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
		ModelName: "Mistral Model",
		ModelType: 1,
	}

	// Используем Models.SaveModel для правильного сжатия данных
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
	models := &Models{db: mockDB}

	userId := uint32(3)

	// Создаём модель OpenAI
	model1 := &UniversalModelData{
		Provider:  ProviderOpenAI,
		ModelID:   "asst_1",
		ModelName: "OpenAI Model",
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
		ModelName: "Mistral Model",
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
	models := &Models{db: mockDB}

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
