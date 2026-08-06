package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// UploaderConfig 是上报功能（OSS 日志上传 + BLAT 后台存库）的凭据配置，
// 从 confs/uploader.yml 加载。该文件含敏感凭据，不入库（见 .gitignore）。
type UploaderConfig struct {
	OSS  OSSConfig  `yaml:"oss"`
	Blat BlatConfig `yaml:"blat"`
}

type OSSConfig struct {
	AccessID  string `yaml:"access_id"`
	SecretKey string `yaml:"secret_key"`
	Host      string `yaml:"host"`
	LogBucket string `yaml:"log_bucket"`
}

type BlatConfig struct {
	BaseURL string `yaml:"base_url"`
	Token   string `yaml:"token"`
}

// LoadUploader 从 path 读取 YAML 并解析为 UploaderConfig。
// 文件不存在返回 error（凭据是必需的，缺文件时调用方应报错退出）。
func LoadUploader(path string) (*UploaderConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read uploader config %s: %w", path, err)
	}
	var cfg UploaderConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse uploader config %s: %w", path, err)
	}
	return &cfg, nil
}
