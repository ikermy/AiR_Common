package common

import (
	"fmt"
	"github.com/spf13/viper"
	"os"
)

type Conf struct {
	TG   TgConfig // Должно быть отдельно получать в процессе инициализации пользователя
	GPT  GPTConfig
	WEB  WebConfig
	DB   DBConfig
	AU   AUTH
	GLOB GLOB
}

type TgConfig struct {
	Token string
}

type GPTConfig struct {
	Key string
}

type WebConfig struct {
	TgBot  string
	TgUser string
}

type DBConfig struct {
	Host     string
	Name     string
	User     string
	Password string
}

type AUTH struct {
	Session string
}

type GLOB struct {
	UserModelTTl int
}

func NewConf() (*Conf, error) {
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = ".env"
	}

	// Проверяем, существует ли файл по указанному пути
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("файл конфигурации не существует: %s\n", configPath)
	}

	viper.SetConfigFile(configPath)
	viper.SetConfigType("hcl") // Указываем формат файла конфигурации

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("ошибка чтения файла конфигурации: %w", err)
	}

	var (
		tgConf   TgConfig
		gptConf  GPTConfig
		webConf  WebConfig
		dbConf   DBConfig
		authConf AUTH
		globConf GLOB
	)

	// Извлекаем массив карт для ТГ
	var tgConfigs []map[string]interface{}
	if err := viper.UnmarshalKey("tg", &tgConfigs); err != nil {
		return nil, fmt.Errorf("невозможно прочитать структуру файла конфигурации ТГ: %w", err)
	}

	// Предполагаем, что первая карта в массиве содержит наши настройки
	if len(tgConfigs) > 0 {
		tgConf.Token = tgConfigs[0]["token"].(string)
	} else {
		return nil, fmt.Errorf("не найдены параметры конфигурации ТГ")
	}

	// Извлекаем массив карт для GPT
	var gptConfigs []map[string]interface{}
	if err := viper.UnmarshalKey("gpt", &gptConfigs); err != nil {
		return nil, fmt.Errorf("невозможно прочитать структуру файла конфигурации GPT: %w", err)
	}

	// Предполагаем, что первая карта в массиве содержит наши настройки
	if len(gptConfigs) > 0 {
		//gptConf.Project = gptConfigs[0]["project"].(string)
		gptConf.Key = gptConfigs[0]["key"].(string)
		//gptConf.Url = gptConfigs[0]["url"].(string)
	} else {
		return nil, fmt.Errorf("не найдены параметры конфигурации GPT")
	}

	// Извлекаем массив карт для Web
	var webConfigs []map[string]interface{}
	if err := viper.UnmarshalKey("web", &webConfigs); err != nil {
		return nil, fmt.Errorf("невозможно прочитать структуру файла конфигурации Web: %w", err)
	}
	// Предполагаем, что первая карта в массиве содержит наши настройки
	if len(webConfigs) > 0 {
		webConf.TgBot = webConfigs[0]["tgbot"].(string)
		webConf.TgUser = webConfigs[0]["tguser"].(string)
	} else {
		return nil, fmt.Errorf("не найдены параметры конфигурации Web")
	}

	// Извлекаем массив карт для DB
	var dbConfigs []map[string]interface{}
	if err := viper.UnmarshalKey("db", &dbConfigs); err != nil {
		return nil, fmt.Errorf("невозможно прочитать структуру файла конфигурации DB: %w", err)
	}

	// Предполагаем, что первая карта в массиве содержит наши настройки
	if len(dbConfigs) > 0 {
		dbConf.Host = dbConfigs[0]["host"].(string)
		dbConf.Name = dbConfigs[0]["name"].(string)
		dbConf.User = dbConfigs[0]["user"].(string)
		dbConf.Password = dbConfigs[0]["password"].(string)
	} else {
		return nil, fmt.Errorf("не найдены параметры конфигурации DB")
	}

	// Извлекаем массив карт для Auth
	var authConfigs []map[string]interface{}
	if err := viper.UnmarshalKey("auth", &authConfigs); err != nil {
		return nil, fmt.Errorf("невозможно прочитать структуру файла конфигурации Auth: %w", err)
	}

	// Предполагаем, что первая карта в массиве содержит наши настройки
	if len(authConfigs) > 0 {
		authConf.Session = authConfigs[0]["session"].(string)
	} else {
		return nil, fmt.Errorf("не найдены параметры конфигурации Auth")
	}

	// Извлекаем массив карт для Auth
	var globConfigs []map[string]interface{}
	if err := viper.UnmarshalKey("glob", &globConfigs); err != nil {
		return nil, fmt.Errorf("невозможно прочитать структуру файла конфигурации Glob: %w", err)
	}

	// Предполагаем, что первая карта в массиве содержит наши настройки
	if len(globConfigs) > 0 {
		globConf.UserModelTTl = globConfigs[0]["usermodttl"].(int)
	} else {
		return nil, fmt.Errorf("не найдены параметры конфигурации Glob")
	}

	return &Conf{
		TG:   tgConf,
		GPT:  gptConf,
		WEB:  webConf,
		DB:   dbConf,
		AU:   authConf,
		GLOB: globConf,
	}, nil
}
