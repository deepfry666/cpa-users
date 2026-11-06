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
}

func defaultConfig() Config {
	on := true
	return Config{
		DBPath:           "plugins/cpa-users.db",
		Timezone:         "Local",
		FilterModelList:  &on,
		KeyPrefix:        "sk-cu-",
		LogRetentionDays: 30,
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
		{Name: "public_base_url", Type: "string", Default: "", Description: "用户访问 CPA 的地址，例如 https://cpa.example.com；用于自助查询链接和接入说明，在 Oh My CPA 中必须填写"},
	}
}
