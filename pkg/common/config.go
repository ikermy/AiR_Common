package common

import (
	"fmt"
	"github.com/spf13/viper"
	"os"
	"strconv"
)

type Conf struct {
	TG   TgConfig
	GPT  GPTConfig
	WEB  WebConfig
	DB   DBConfig
	AU   AUTH
	GLOB GLOB
}

type TgConfig struct {
	Token string `mapstructure:"token"`
}

type GPTConfig struct {
	URL     string `mapstructure:"url"`
	Project string `mapstructure:"project"`
	Key     string `mapstructure:"key"`
}

type WebConfig struct {
	TgBot  string `mapstructure:"tgbot"`
	TgUser string `mapstructure:"tguser"`
}

type DBConfig struct {
	Host     string `mapstructure:"host"`
	Name     string `mapstructure:"name"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
}

type AUTH struct {
	Session string `mapstructure:"session"`
}

type GLOB struct {
	UserModelTTl int
}

func NewConf() (*Conf, error) {
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = ".env" // Изменено на .yaml файл
	}

	// Проверяем существование файла
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("файл конфигурации не существует: %s", configPath)
	}

	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml") // Изменен тип на yaml

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("ошибка чтения файла конфигурации: %w", err)
	}

	conf := &Conf{}

	// TG секция
	var tgConfig TgConfig
	if err := v.UnmarshalKey("tg", &tgConfig); err != nil {
		return nil, fmt.Errorf("ошибка разбора секции tg: %w", err)
	}
	conf.TG = tgConfig

	// GPT секция
	var gptConfig GPTConfig
	if err := v.UnmarshalKey("gpt", &gptConfig); err != nil {
		return nil, fmt.Errorf("ошибка разбора секции gpt: %w", err)
	}
	conf.GPT = gptConfig

	// WEB секция
	var webConfig WebConfig
	if err := v.UnmarshalKey("web", &webConfig); err != nil {
		return nil, fmt.Errorf("ошибка разбора секции web: %w", err)
	}
	conf.WEB = webConfig

	// DB секция
	var dbConfig DBConfig
	if err := v.UnmarshalKey("db", &dbConfig); err != nil {
		return nil, fmt.Errorf("ошибка разбора секции db: %w", err)
	}
	conf.DB = dbConfig

	// AUTH секция
	var authConfig AUTH
	if err := v.UnmarshalKey("auth", &authConfig); err != nil {
		return nil, fmt.Errorf("ошибка разбора секции auth: %w", err)
	}
	conf.AU = authConfig

	// GLOB секция - обрабатываем отдельно из-за преобразования строки в число
	if v.IsSet("glob.usermodttl") {
		usermodttlStr := v.GetString("glob.usermodttl")
		usermodttl, err := strconv.Atoi(usermodttlStr)
		if err != nil {
			return nil, fmt.Errorf("некорректное значение usermodttl: %w", err)
		}
		conf.GLOB = GLOB{UserModelTTl: usermodttl}
	} else {
		return nil, fmt.Errorf("не найден параметр glob.usermodttl")
	}

	return conf, nil
}
