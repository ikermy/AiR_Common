package create

// ToolType определяет тип инструмента (провайдер-агностичный)
type ToolType string

const (
	// ToolTypeFunction тип инструмента - функция
	ToolTypeFunction ToolType = "function"
)

// ToolFunctionDefinition определение функции для инструмента
// Провайдер-агностичный аналог openai.FunctionDefinition
type ToolFunctionDefinition struct {
	Name        string      `json:"name"`                 // Имя функции
	Description string      `json:"description"`          // Описание функции
	Parameters  interface{} `json:"parameters,omitempty"` // JSON Schema параметров функции
}

// ProviderTool представляет инструмент для AI провайдера
// Провайдер-агностичный аналог openai.Tool
type ProviderTool struct {
	Type     ToolType                `json:"type"`     // Тип инструмента ("function")
	Function *ToolFunctionDefinition `json:"function"` // Определение функции
}
