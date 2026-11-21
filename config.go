package main

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the plugin section under plugins.configs.cpa-users in CPA's config.yaml.
type Config struct {
	Enabled  bool `yaml:"enabled"`
	Priority int  `yaml:"priority"`
	// DBPath is the SQLite file. Relative paths resolve against CPA's working
	// directory, the same base CPA uses for plugins.dir.
	DBPath string `yaml:"db_path"`
	// Timezone decides where "today" and "this month" start for quotas and charts.
	Timezone string `yaml:"timezone"`
	// FilterModelList trims /v1/models for plugin keys to the models they may use.
	// Turning it off stops every non-streaming response from passing through the plugin.
	FilterModelList *bool `yaml:"filter_model_list"`
	// KeyPrefix is prepended to issued keys; it also lets the plugin ignore
	// native CPA keys without hashing them.
	KeyPrefix string `yaml:"key_prefix"`
	// LogRetentionDays bounds the per-request log. Hourly aggregates are kept forever.
	LogRetentionDays int `yaml:"log_retention_days"`
	// PublicBaseURL is the address users reach CPA at, e.g. https://cpa.example.com.
	// The console builds the self-service link and client setup hints from it;
	// inside Oh My CPA the page's own origin is the console, not CPA.
	PublicBaseURL string `yaml:"public_base_url"`
	// CurrencySymbol is shown next to every amount. Prices are entered in this
	// currency; the plugin does not convert.
	CurrencySymbol string `yaml:"currency_symbol"`
	// Price sync imports official list prices (USD) from LiteLLM and
	// OpenRouter. Manual prices always win.
	PriceSync          *bool  `yaml:"price_sync"`
	PriceSyncHours     int    `yaml:"price_sync_hours"`
	PriceProxyURL      string `yaml:"price_proxy_url"`
	PriceLiteLLMURL    string `yaml:"price_litellm_url"`
	PriceOpenRouterURL string `yaml:"price_openrouter_url"`
}

// priceSync reports whether automatic prices may be imported. The datasets
// are in US dollars, so a ledger kept in another currency never imports them.
func (c Config) priceSync() bool { return (c.PriceSync == nil || *c.PriceSync) && c.usdLedger() }

func (c Config) usdLedger() bool {
	switch strings.ToUpper(c.CurrencySymbol) {
	case "$", "US$", "USD":
		return true
	}
	return false
}

func (c Config) syncInterval() time.Duration {
	return time.Duration(c.PriceSyncHours) * time.Hour
}

func defaultConfig() Config {
	on := true
	return Config{
		DBPath:             "plugins/cpa-users.db",
		Timezone:           "Local",
		FilterModelList:    &on,
		KeyPrefix:          "sk-cu-",
		LogRetentionDays:   30,
		CurrencySymbol:     "$",
		PriceSyncHours:     12,
		PriceLiteLLMURL:    defaultLiteLLMURL,
		PriceOpenRouterURL: defaultOpenRouterURL,
	}
}

func parseConfig(raw []byte) (Config, error) {
	cfg := defaultConfig()
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s config: %w", PluginID, err)
		}
	}
	if strings.TrimSpace(cfg.DBPath) == "" {
		cfg.DBPath = defaultConfig().DBPath
	}
	if strings.TrimSpace(cfg.Timezone) == "" {
		cfg.Timezone = "Local"
	}
	if cfg.FilterModelList == nil {
		on := true
		cfg.FilterModelList = &on
	}
	if strings.TrimSpace(cfg.KeyPrefix) == "" {
		cfg.KeyPrefix = defaultConfig().KeyPrefix
	}
	if cfg.LogRetentionDays <= 0 {
		cfg.LogRetentionDays = 30
	}
	cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	cfg.CurrencySymbol = strings.TrimSpace(cfg.CurrencySymbol)
	if cfg.CurrencySymbol == "" {
		cfg.CurrencySymbol = "$"
	}
	if cfg.PriceSyncHours <= 0 {
		cfg.PriceSyncHours = 12
	}
	if cfg.PriceSyncHours < 1 {
		cfg.PriceSyncHours = 1
	}
	cfg.PriceProxyURL = strings.TrimSpace(cfg.PriceProxyURL)
	if strings.TrimSpace(cfg.PriceLiteLLMURL) == "" {
		cfg.PriceLiteLLMURL = defaultLiteLLMURL
	}
	if strings.TrimSpace(cfg.PriceOpenRouterURL) == "" {
		cfg.PriceOpenRouterURL = defaultOpenRouterURL
	}
	if len([]rune(cfg.CurrencySymbol)) > 4 {
		cfg.CurrencySymbol = string([]rune(cfg.CurrencySymbol)[:4])
	}
	return cfg, nil
}

func (c Config) location() *time.Location {
	if strings.EqualFold(c.Timezone, "local") {
		return time.Local
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}

func (c Config) filterModels() bool { return c.FilterModelList == nil || *c.FilterModelList }

func configFields() []configField {
	return []configField{
		{Name: "db_path", Type: "string", Default: "plugins/cpa-users.db", Description: "SQLite 数据文件路径（相对 CPA 工作目录）"},
		{Name: "timezone", Type: "string", Default: "Local", Description: "统计和额度按哪个时区切日/切月，例如 Asia/Shanghai"},
		{Name: "filter_model_list", Type: "boolean", Default: true, Description: "按用户权限裁剪 /v1/models 返回的模型列表"},
		{Name: "key_prefix", Type: "string", Default: "sk-cu-", Description: "签发 key 的前缀"},
		{Name: "log_retention_days", Type: "integer", Default: 30, Description: "请求明细保留天数（按小时汇总的数据永久保留）"},
		{Name: "currency_symbol", Type: "string", Default: "$", Description: "金额显示的货币符号，例如 $ 或 ¥；模型价格按这个货币填写"},
		{Name: "price_sync", Type: "boolean", Default: true, Description: "自动从 LiteLLM / OpenRouter 同步官方价格（美元）；手动填写的价格不会被覆盖"},
		{Name: "price_sync_hours", Type: "integer", Default: 12, Description: "自动同步价格的间隔（小时）"},
		{Name: "price_proxy_url", Type: "string", Default: "", Description: "同步价格时使用的代理，例如 http://mihomo:7891；留空则用环境变量 HTTPS_PROXY"},
		{Name: "price_litellm_url", Type: "string", Default: defaultLiteLLMURL, Description: "LiteLLM 价格表地址（可换成镜像）"},
		{Name: "price_openrouter_url", Type: "string", Default: defaultOpenRouterURL, Description: "OpenRouter 模型列表地址（可换成镜像）"},
		{Name: "public_base_url", Type: "string", Default: "", Description: "用户访问 CPA 的地址，例如 https://cpa.example.com；用于自助查询链接和接入说明，在 Oh My CPA 中必须填写"},
	}
}
