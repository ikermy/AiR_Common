package conf

import (
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/viper"
)

type Conf struct {
	DemoAS DemoAssist
	TG     TgConfig
	GPT    GPTConfig
	WEB    WebConfig
	DB     DBConfig
	AU     AUTH
	SMTP   SMTP
	GLOB   GLOB
}

type TgConfig struct {
	Name     string `mapstructure:"bot"`
	Token    string `mapstructure:"token"`
	Id       int64  `mapstructure:"id"`
	Operator string `mapstructure:"operator"`
}

type GPTConfig struct {
	Created string `mapstructure:"created"`
	URL     string `mapstructure:"url"`
	Project string `mapstructure:"project"`
	Key     string `mapstructure:"key"`
}

type WebConfig struct {
	RealUrl string `mapstructure:"real_url"`
	Land    string `mapstructure:"land"`
	Demo    string `mapstructure:"demo"`
	Widget  string `mapstructure:"widget"`
	TgBot   string `mapstructure:"tgbot"`
	TgUser  string `mapstructure:"tguser"`
	Whats   string `mapstructure:"whats"`
	Oper    string `mapstructure:"oper"`
}

type DBConfig struct {
	Host     string `mapstructure:"host"`
	Name     string `mapstructure:"name"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
}

type AUTH struct {
	Session string `mapstructure:"session"`
	Created string `mapstructure:"created"`
	UserKey string `mapstructure:"userkey"`
}

type GLOB struct {
	UserModelTTl int
}

type SMTP struct {
	Host     string `mapstructure:"host"`
	Port     string `mapstructure:"port"`
	Email    string `mapstructure:"mail"`
	Password string `mapstructure:"pass"`
}

type DemoAssist struct {
	URL    string `mapstructure:"demo_assist_url"`
	Key    string `mapstructure:"demo_assist_key"`
	Psycho string `mapstructure:"psycho"`
	Lawyer string `mapstructure:"lawyer"`
	Tech   string `mapstructure:"tech"`
}

func NewConf() (*Conf, error) {
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "cfg.env"
	}

	// Проверяем существование файла
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("файл конфигурации не существует: %s", configPath)
	}

	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("ошибка чтения файла конфигурации: %w", err)
	}

	conf := &Conf{}

	// TG секция
	var tgConfig TgConfig
	if err := v.UnmarshalKey("tg", &tgConfig); err != nil {
		return nil, fmt.Errorf("ошибка разбора секции tg: %w", err)
	}

	// Отдельно обрабатываем id, так как может потребоваться преобразование из строки
	if v.IsSet("tg.id") {
		idStr := v.GetString("tg.id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("некорректное значение tg.id: %w", err)
		}
		tgConfig.Id = id
	} else {
		return nil, fmt.Errorf("не найден параметр tg.id")
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

	// SMTP секция
	var smtpConfig SMTP
	if err := v.UnmarshalKey("smtp", &smtpConfig); err != nil {
		return nil, fmt.Errorf("ошибка разбора секции smtp: %w", err)
	}
	conf.SMTP = smtpConfig

	// Assist секция
	var demoAssist DemoAssist
	if err := v.UnmarshalKey("demoassist", &demoAssist); err != nil {
		return nil, fmt.Errorf("ошибка разбора секции demoassist: %w", err)
	}
	conf.DemoAS = demoAssist

	return conf, nil
}
