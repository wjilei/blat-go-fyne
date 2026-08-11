package uploader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GetWorkstationInfo 调 BLAT /v1/work-stations/get-or-create 取工位名
// （响应 data.name）。失败返回 "" + error。最多重试 3 次，与 SaveTestData
// 重试策略保持一致；400/401/500 立即放弃，其余错误 500ms 后重试。
//
// 对应 BLAT app.pl:901-938 _get_work_station_info + BlatServer.pm:414-418
// GetOrCreateWorkstation（POST /v1/work-stations/get-or-create，body {uuid, osver}）。
func GetWorkstationInfo(uuid, osver string) (string, error) {
	const maxRetry = 3
	const retryGap = 500 * time.Millisecond

	payload, err := json.Marshal(map[string]string{"uuid": uuid, "osver": osver})
	if err != nil {
		return "", err
	}
	url := cfg.Blat.BaseURL + "/v1/work-stations/get-or-create"

	var lastErr error
	for attempt := 1; attempt <= maxRetry; attempt++ {
		content, err := postJSONForBody(url, payload)
		if err == nil {
			data, _ := content["data"].(map[string]any)
			name, _ := data["name"].(string)
			if name == "" {
				return "", fmt.Errorf("工位接口响应缺少 data.name: %v", content)
			}
			return name, nil
		}
		if _, fatal := err.(*fatalHTTPError); fatal {
			return "", err
		}
		lastErr = err
		if attempt < maxRetry {
			time.Sleep(retryGap)
		}
	}
	return "", lastErr
}

// postJSONForBody 与 postJSON 行为类似（Bearer token、3 类状态码处理），
// 但返回解析后的 content map 给调用方。不复用 postJSON 是因为后者在判定
// result truthy 后直接丢弃 body，workstation 接口需要从 data.name 取值。
func postJSONForBody(url string, payload []byte) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Blat.Token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusInternalServerError:
			return nil, &fatalHTTPError{status: resp.StatusCode}
		default:
			return nil, fmt.Errorf("服务器返回非 2xx 状态码 %d", resp.StatusCode)
		}
	}

	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("工位接口返回空 body")
	}
	var content map[string]any
	if err := json.Unmarshal(body, &content); err != nil {
		return nil, fmt.Errorf("工位接口响应非 JSON: %w", err)
	}
	if v, has := content["result"]; has {
		ok := false
		switch r := v.(type) {
		case bool:
			ok = r
		case float64:
			ok = r != 0
		case string:
			ok = r != "" && r != "0"
		}
		if !ok {
			return nil, fmt.Errorf("工位接口返回 result 为假值: %v", v)
		}
	}
	return content, nil
}
