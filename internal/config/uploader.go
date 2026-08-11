package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// UploaderConfig 是上报功能（OSS 日志上传 + BLAT 后台存库）的非敏感配置，
// 从 confs/uploader.yml 加载。该文件不再包含 OSS 长效 AccessKey/SecretKey：
// 客户端在需要上传时通过 BLAT 后台 GET /v1/ststoken 拉取 STS 临时凭证（见
// uploader/sts.go），避免把长效 key 落到客户端磁盘。BlatConfig.Token 仍由
// 操作员维护（用于调用其它后台接口 + 携带到 /v1/ststoken）。
type UploaderConfig struct {
	OSS  OSSConfig  `yaml:"oss"`
	Blat BlatConfig `yaml:"blat"`
}

type OSSConfig struct {
	// Endpoint 是 OSS endpoint 完整 URL，含 scheme（如 https://oss-cn-hangzhou.aliyuncs.com）。
	// 之所以存完整 URL 而不是仅域名：测试场景可能指向 httptest.Server 的 http 地址，
	// 完整 URL 让生产/测试用同一字段、不引入额外 scheme 配置。
	Endpoint string `yaml:"endpoint"`
	// LogBucket 是上报日志的目标 bucket。客户端要访问的是 blat-app-log，
	// 由 BLAT 后台 STS 角色 aliyunosstokengeneratorrole 授权。
	LogBucket string `yaml:"log_bucket"`
}

type BlatConfig struct {
	BaseURL string `yaml:"base_url"`
	Token   string `yaml:"token"`
}

// LoadUploader 从 path 读取 YAML 并解析为 UploaderConfig。
// 文件不存在返回 error（配置是必需的，缺文件时调用方应报错退出）。
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