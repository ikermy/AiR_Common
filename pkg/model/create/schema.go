package create

import "encoding/json"

// ParseModelSchemaJSON парсит статическую JSON Schema в map[string]any
// для использования в response_schema (Google) и json_schema.schema (OpenAI)
// Универсальный метод для обоих провайдеров
// ПРИМЕЧАНИЕ: Эта статическая схема используется только для некоторых случаев.
// OpenAI модели используют динамическую схему из generateModelSchema (open.go)
func ParseModelSchemaJSON(includeAdditionalProperties bool) map[string]any {
	// Базовая схема БЕЗ additionalProperties (для Google)
	var modelSchemaJSON string

	if includeAdditionalProperties {
		// Для OpenAI - С additionalProperties
		modelSchemaJSON = `{
		"type": "object",
		"properties": {
			"message": {
				"type": "string"
			},
			"action": {
				"type": "object",
				"properties": {
					"send_files": {
						"type": "array",
						"default": [],
						"items": {
							"type": "object",
							"properties": {
								"type": {
									"type": "string",
									"enum": ["photo", "video", "audio", "doc"]
								},
								"url": {
									"type": "string"
								},
								"file_name": {
									"type": "string"
								},
								"caption": {
									"type": "string"
								}
							},
							"required": ["type", "url", "file_name", "caption"],
							"additionalProperties": false
						}
					}
				},
				"required": ["send_files"],
				"additionalProperties": false
			},
			"target": { "type": "boolean" },
			"operator": { "type": "boolean" }
		},
		"required": ["message", "action", "target", "operator"],
		"additionalProperties": false
	}`
	} else {
		// Для Google - БЕЗ additionalProperties (Google не поддерживает это поле)
		modelSchemaJSON = `{
		"type": "object",
		"properties": {
			"message": {
				"type": "string"
			},
			"action": {
				"type": "object",
				"properties": {
					"send_files": {
						"type": "array",
						"default": [],
						"items": {
							"type": "object",
							"properties": {
								"type": {
									"type": "string",
									"enum": ["photo", "video", "audio", "doc"]
								},
								"url": {
									"type": "string"
								},
								"file_name": {
									"type": "string"
								},
								"caption": {
									"type": "string"
								}
							},
							"required": ["type", "url", "file_name", "caption"]
						}
					}
				},
				"required": ["send_files"]
			},
			"target": { "type": "boolean" },
			"operator": { "type": "boolean" }
		},
		"required": ["message", "action", "target", "operator"]
	}`
	}

	var schema map[string]any
	err := json.Unmarshal([]byte(modelSchemaJSON), &schema)
	if err != nil {
		// Это не должно произойти, т.к. modelSchemaJSON - валидный JSON
		//logger.Error("[ParseModelSchemaJSON] Ошибка парсинга ModelSchemaJSON: %v", err)
		return map[string]any{} // Возвращаем пустую схему в крайнем случае
	}
	return schema
}
