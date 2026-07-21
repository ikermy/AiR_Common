package openai

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// universalRealtimeSystemPrompt — базовый стиль голосового взаимодействия.
// Добавляется в начало RealtimeSystemPrompt перед промптом агента.
const universalRealtimeSystemPrompt = `# Voice Interaction Mode
You are operating in real-time voice mode.

# Response Style
- Give SHORT, NATURAL spoken answers — 1-3 sentences unless detail is explicitly requested.
- NEVER use markdown: no **, no #, no bullet lists, no code blocks.
- Spell out numbers naturally ("twenty three", not "23").
- Use natural transitions: "Sure, let me check that.", "Got it.", "One moment."

# Clarification
- If you need to confirm a name, number, or spelling — repeat it back before proceeding.
- If the user corrects you — acknowledge briefly and confirm the new value.

# Language
- Respond in the same language the user is speaking.

`

// buildRealtimeSystemPrompt строит голосовой промпт для Realtime API.
// Источник истины — config, заполненный buildAgentConfiguration (данные от MCP):
//   - config.SystemPrompt — промпт агента (modelData.Prompt + hint от MCP)
//   - config.Tools — список инструментов от MCP
//
// Алгоритм:
//  1. universalRealtimeSystemPrompt (голосовой стиль)
//  2. Если есть function-tools — универсальные инструкции по их использованию голосом
//  3. Промпт агента из config.SystemPrompt (уже содержит все специфичные инструкции от MCP)
func buildRealtimeSystemPrompt(config *AgentConfig) string {
	var b strings.Builder

	// Шаг 1: голосовой стиль
	b.WriteString(universalRealtimeSystemPrompt)

	// Шаг 2: универсальные голосовые инструкции по инструментам (если они есть)
	hasFunctionTools := false
	for _, t := range config.Tools {
		if tm, ok := t.(map[string]any); ok {
			if tm["type"] == "function" {
				hasFunctionTools = true
				break
			}
		}
	}
	if hasFunctionTools {
		b.WriteString("# Voice Tools\n")
		b.WriteString("- Before any tool call, say ONE short sentence: \"One moment.\" Then call immediately.\n")
		b.WriteString("- After a tool returns — summarise in natural spoken language. DO NOT read raw JSON or URLs aloud.\n")
		b.WriteString("- NEVER say \"I can't check\" if a tool exists — CALL IT.\n")
		b.WriteString("- If a tool returns an error or unavailable — say so briefly and move on. Do NOT retry the same tool.\n")
		b.WriteString("\n")
	}

	// Шаг 3: промпт агента от MCP (содержит личность, задачу, специфичные инструкции)
	// Убираем повторяющиеся пустые строки
	multiNewline := regexp.MustCompile(`\n{3,}`)
	cleaned := strings.TrimSpace(multiNewline.ReplaceAllString(config.SystemPrompt, "\n\n"))
	if cleaned != "" {
		b.WriteString("# Agent\n")
		b.WriteString(cleaned)
		b.WriteString("\n")
	}

	return b.String()
}

// extractFilesForRealtime пытается извлечь файлы из результата любого MCP-инструмента
// по структуре JSON-ответа, без привязки к имени функции.
//
// Поддерживаемые форматы:
//  1. Объект с полем "url" или "Url" → один файл (create_file, save_image и любые другие)
//  2. Массив строк-URL → список файлов (get_s3_files и аналоги)
//  3. Строка, являющаяся JSON-массивом URL → список файлов
//
// Возвращает:
//   - files        — массив объектов {type, Url, file_name, caption} для отправки клиенту
//   - voiceConfirm — текст для модели БЕЗ URL (чтобы не озвучивала ссылки)
func extractFilesForRealtime(rawResult string) (files []map[string]any, voiceConfirm string) {
	raw := strings.TrimSpace(rawResult)
	if raw == "" {
		return nil, ""
	}

	// Попытка 1: объект с "url"/"Url" → один файл
	if strings.HasPrefix(raw, "{") {
		var r map[string]any
		if err := json.Unmarshal([]byte(raw), &r); err == nil {
			url, _ := r["url"].(string)
			if url == "" {
				url, _ = r["Url"].(string)
			}
			if url != "" && isFileURL(url) {
				fileName, _ := r["file_name"].(string)
				if fileName == "" {
					fileName = filepath.Base(url)
				}
				fileType, _ := r["type"].(string)
				if fileType == "" {
					fileType = realtimeFileType(url)
				}
				files = []map[string]any{{
					"type": fileType, "Url": url, "file_name": fileName, "caption": "",
				}}
				voiceConfirm = fmt.Sprintf(`{"status":"ok","file_name":%q,"type":%q}`, fileName, fileType)
				return
			}
		}
	}

	// Попытка 2: массив строк-URL (прямой или обёрнутый в {"output":"[...]"})
	var urlList []string

	if strings.HasPrefix(raw, "[") {
		_ = json.Unmarshal([]byte(raw), &urlList)
	} else if strings.HasPrefix(raw, "{") {
		var wrapper map[string]any
		if err := json.Unmarshal([]byte(raw), &wrapper); err == nil {
			if outputStr, ok := wrapper["output"].(string); ok {
				_ = json.Unmarshal([]byte(outputStr), &urlList)
			}
		}
	}

	if len(urlList) == 0 {
		return nil, ""
	}

	// Фильтруем: только строки, похожие на URL с файловым расширением
	nameParts := make([]string, 0, len(urlList))
	for _, u := range urlList {
		if !isFileURL(u) {
			continue
		}
		fileName := filepath.Base(u)
		fileType := realtimeFileType(u)
		files = append(files, map[string]any{
			"type": fileType, "Url": u, "file_name": fileName, "caption": "",
		})
		nameParts = append(nameParts, fmt.Sprintf("%q", fileName))
	}
	if len(files) == 0 {
		return nil, ""
	}
	voiceConfirm = fmt.Sprintf(`{"status":"ok","count":%d,"file_names":[%s]}`,
		len(files), strings.Join(nameParts, ","))
	return
}

// isFileURL возвращает true если строка похожа на URL с путём к файлу.
func isFileURL(s string) bool {
	return (strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")) &&
		filepath.Ext(filepath.Base(s)) != ""
}

// realtimeFileType определяет тип файла по расширению.
func realtimeFileType(url string) string {
	ext := strings.ToLower(filepath.Ext(filepath.Base(url)))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return "photo"
	case ".mp4", ".mov", ".avi", ".webm":
		return "video"
	case ".mp3", ".wav", ".ogg", ".m4a":
		return "audio"
	default:
		return "doc"
	}
}
