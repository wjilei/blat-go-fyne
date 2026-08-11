// Package uploader 提供测试全部跑完后的上报逻辑：把运行日志压缩上传阿里云 OSS，
// 并把测试记录 POST 到 BLAT 服务器数据库。对应 BLAT Perl 版 BlatServer.pm 的
// hook_stop + send_report_to_oss + HeatSaveTestData。
//
// 上报所需的 OSS 凭据与 BLAT 后台 token 从配置文件加载（config.LoadUploader
// 读取 confs/uploader.yml 后经 uploader.Init 注入），不再硬编码在代码里。
package uploader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/ulikunitz/xz/lzma"

	"blat/internal/core"
	"blat/internal/report"
)

// devTypeHeat 是整机测试记录查询使用的设备类型（对应 HeatGetTestRecord），
// 属逻辑常量，保留在代码里。
const devTypeHeat = "2"

// Config 是上报所需的非敏感配置，由 config.LoadUploader 从 YAML 加载后经 Init 注入。
// OSS 不再含长效 AccessKey/SecretKey——每次上传通过 BlatConfig.BaseURL + Token
// 从 BLAT 后台 GET /v1/ststoken 拉取 STS 临时凭证（见 sts.go）。
type Config struct {
	OSS  OSSConfig
	Blat BlatConfig
}

type OSSConfig struct {
	Endpoint, LogBucket string
}

type BlatConfig struct {
	BaseURL, Token string
}

// 包级配置；Init 前为空，调用 UploadLogOSS / SaveTestData / GetTestRecord 会失败或构造空凭据。
var cfg Config

// Init 用配置文件中的凭据初始化包级配置，必须在上述函数被调用前执行一次。
func Init(c Config) { cfg = c }

// httpClient 是共享的 HTTP 客户端；带超时防止请求挂死导致程序退出时卡住。
var httpClient = &http.Client{Timeout: 15 * time.Second}

// LzmaCompress 用 LZMA1 alone 格式压缩数据（与 BLAT 的 IO::Compress::Lzma 一致）。
// lzma.NewWriter 输出即 .lzma 格式；writer 必须 Close 才会 flush 尾部数据，不能提前丢弃。
func LzmaCompress(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := lzma.NewWriter(&buf)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UploadLogOSS 把压缩后的日志上传到阿里云 OSS，模拟 BLAT Utils.pm 的 save_log_to_oss。
// 最多重试 6 次；每次失败后若距上次失败不足 3 秒，则 sleep 补足到 3 秒再重试，
// 避免触发 OSS 限流造成连续快速失败。
//
// 凭据：每次循环都通过 tokenSource.Token(ctx) 拉 STS 临时凭证，失败重试时
// 也重新拉一次（sts.go 内部缓存会在过期前 60s 续期）。
func UploadLogOSS(ctx context.Context, ossPath string, content []byte) error {
	const maxRetry = 6
	const retryInterval = 3 * time.Second

	if cfg.OSS.Endpoint == "" || cfg.OSS.LogBucket == "" {
		return errors.New("uploader: OSS.Endpoint / OSS.LogBucket 未配置，无法上传")
	}

	var lastErr error
	lastFail := time.Time{}
	for attempt := 1; attempt <= maxRetry; attempt++ {
		err := uploadOnceWithSTS(ctx, ossPath, content)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt == maxRetry {
			break
		}
		if wait := retryInterval - time.Since(lastFail); wait > 0 {
			time.Sleep(wait)
		}
		lastFail = time.Now()
	}
	return lastErr
}

// uploadOnceWithSTS 拉一次 STS，建 OSS client，调 PutObject。
// 失败时一并把 token 标记为"已耗尽"——但当前实现里 sts.blatSTSSource 不会主动
// 失效缓存（避免一次网络抖动就强制所有人重拉），由下次 Token() 自然续期。
func uploadOnceWithSTS(ctx context.Context, ossPath string, content []byte) error {
	tok, err := defaultTokenSource.Token(ctx)
	if err != nil {
		return fmt.Errorf("获取 STS 失败: %w", err)
	}
	client, err := oss.New(
		cfg.OSS.Endpoint,
		tok.AccessKeyID,
		tok.AccessKeySecret,
		oss.SecurityToken(tok.SecurityToken),
	)
	if err != nil {
		return err
	}
	bucket, err := client.Bucket(cfg.OSS.LogBucket)
	if err != nil {
		return err
	}
	return bucket.PutObject(ossPath, bytes.NewReader(content))
}

// fatalHTTPError 表示服务器返回 400/401/500，按 BLAT do_request 的规则应
// 立即失败、不再重试。用类型区分是为了让 SaveTestData 能决定是否继续尝试。
// body 保留响应体原文，方便排查 400 这类参数错误。
type fatalHTTPError struct {
	status int
	body   string
}

func (e *fatalHTTPError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("服务器返回 %d，不再重试", e.status)
	}
	return fmt.Sprintf("服务器返回 %d，不再重试，响应体: %s", e.status, e.body)
}

// SaveTestData 把测试记录 POST 到 BLAT 服务器数据库，模拟 BLAT 的
// _SaveTestData + http_post + do_request（最多重试 3 次）。
// 400/401/500 立即失败；其余失败用 500ms 短 sleep 后重试。
// 入口处把请求 URL 与 payload 打到 stderr，便于排查后台 400。
func SaveTestData(data map[string]any, devType string) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}

	url := cfg.Blat.BaseURL + "/v1/tests/records?dev_type=" + devType
	log.Printf("[uploader] SaveTestData POST %s dev_type=%s payload=%s", url, devType, payload)
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		err := postJSON(url, payload)
		if err == nil {
			return nil
		}
		if _, ok := err.(*fatalHTTPError); ok {
			return err
		}
		lastErr = err
		if attempt < 3 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return lastErr
}

// postJSON 执行单次 POST 并按 BLAT do_request 的规则判定成败：
//   - 2xx 且 body 为空、非 JSON、或 JSON 无 "result" 键 → 成功；
//   - 2xx 且 JSON 带 "result" 键 → 必须为真值才成功；
//   - 400/401/500 → fatalHTTPError，立即放弃；
//   - 其他非 2xx → 普通错误，交由上层重试。
func postJSON(url string, payload []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Blat.Token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if len(bytes.TrimSpace(body)) == 0 {
			return nil
		}
		var j any
		if err := json.Unmarshal(body, &j); err != nil {
			return nil // 非 JSON，对齐 do_request 视为成功
		}
		m, ok := j.(map[string]any)
		if !ok {
			return nil // 合法 JSON 但非对象，无 result 键，视为成功
		}
		v, has := m["result"]
		if !has {
			return nil
		}
		// result 键存在，必须为真值（对齐 Perl 的 truthy 判定）
		switch r := v.(type) {
		case bool:
			if r {
				return nil
			}
		case float64:
			if r != 0 {
				return nil
			}
		case string:
			if r != "" && r != "0" {
				return nil
			}
		}
		return fmt.Errorf("服务器返回 result 为假值: %v", v)
	}

	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusInternalServerError:
		return &fatalHTTPError{status: resp.StatusCode, body: string(body)}
	default:
		return fmt.Errorf("服务器返回非 2xx 状态码 %d，响应体: %s", resp.StatusCode, string(body))
	}
}

// GetTestRecord 查询整机测试记录，对应 BLAT 的 HeatGetTestRecord：
// GET /v1/tests/query/info?dev_type=<heat>&serial_num=<serial>&test_mode=normal&test_result=1。
// 成功返回 content.data 数组的第一条记录（map）；查询失败或没有记录返回 error。
// 400/401/500 立即失败不重试，其余错误最多重试 3 次。
func GetTestRecord(serialNum string) (map[string]any, error) {
	params := url.Values{}
	params.Set("dev_type", devTypeHeat)
	params.Set("serial_num", serialNum)
	params.Set("test_mode", "normal")
	params.Set("test_result", "1")
	full := cfg.Blat.BaseURL + "/v1/tests/query/info?" + params.Encode()

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		content, err := getJSON(full)
		if err == nil {
			if m, ok := content.(map[string]any); ok {
				if arr, ok := m["data"].([]any); ok && len(arr) > 0 {
					if rec, ok := arr[0].(map[string]any); ok {
						return rec, nil
					}
				}
			}
			return nil, fmt.Errorf("未找到序列号 %s 的整机测试记录", serialNum)
		}
		if _, ok := err.(*fatalHTTPError); ok {
			return nil, err
		}
		lastErr = err
		if attempt < 3 {
			time.Sleep(500 * time.Millisecond)
		}
	}
	return nil, lastErr
}

// getJSON 执行单次 GET 并按 BLAT do_request 的规则判定成败：
//   - 2xx 且 JSON 带 "result" 键 → 必须为真值才成功；
//   - 2xx 其余情况 → 成功，返回解析后的 content（可能是 map/数组/nil）；
//   - 400/401/500 → fatalHTTPError，立即放弃；
//   - 其他非 2xx → 普通错误，交由上层重试。
func getJSON(url string) (any, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
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

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var j any
		if err := json.Unmarshal(body, &j); err != nil {
			return nil, fmt.Errorf("响应不是合法 JSON: %w", err)
		}
		if m, ok := j.(map[string]any); ok {
			if v, has := m["result"]; has {
				// result 键存在，必须为真值（对齐 Perl 的 truthy 判定）
				switch r := v.(type) {
				case bool:
					if r {
						return j, nil
					}
				case float64:
					if r != 0 {
						return j, nil
					}
				case string:
					if r != "" && r != "0" {
						return j, nil
					}
				}
				return nil, fmt.Errorf("服务器返回 result 为假值: %v", v)
			}
		}
		return j, nil
	}

	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusInternalServerError:
		return nil, &fatalHTTPError{status: resp.StatusCode}
	default:
		return nil, fmt.Errorf("服务器返回非 2xx 状态码 %d", resp.StatusCode)
	}
}

// HookStopReporter 实现 report.Reporter 接口：在计划全部跑完（OnPlanStop）时
// 触发 hook_stop 上报逻辑，其余生命周期事件忽略。
type HookStopReporter struct {
	env       *core.Env
	logSrc    func() string // 日志内容来源；可为 nil
	skipOSS   bool          // --debug：不实际上传/保存，把上报数据打印到日志
	startTime time.Time     // OnPlanStart 记录的计划开始时间，用于 last_start_time
}

// NewHookStop 创建上报用的 Reporter。logSrc 在计划结束时才被调用，
// 因此能取到包含本次运行完整日志的最新内容。skipOSS 为 true 时
// （--debug）不触网：跳过 OSS 上传与数据库保存，只把要上报的数据
// （含原始日志）打印到日志供排查。
func NewHookStop(env *core.Env, logSrc func() string, skipOSS bool) *HookStopReporter {
	return &HookStopReporter{env: env, logSrc: logSrc, skipOSS: skipOSS}
}

func (h *HookStopReporter) OnPlanStart(total int, startTime time.Time) {
	h.startTime = startTime
}
func (h *HookStopReporter) OnCaseStart(seq int, cr report.CaseReport) {}
func (h *HookStopReporter) OnCaseStop(seq int, cr report.CaseReport)  {}

// OnPlanStop 在测试全部跑完后触发日志上传与测试记录存库。
func (h *HookStopReporter) OnPlanStop(sum report.Summary) {
	h.hookStop(sum)
}

// hookStop 对应 BLAT 的 hook_stop + send_report_to_oss + HeatSaveTestData：
// 默认把日志压缩上传 OSS、测试记录 POST 到 BLAT 服务器；
// --debug（skipOSS）时不实际上传/保存，只把要上报的数据打印到日志供排查。
// 任一步失败都只记日志，不 panic，避免上报问题影响主流程退出。
func (h *HookStopReporter) hookStop(sum report.Summary) {
	log := ""
	if h.logSrc != nil {
		log = h.logSrc()
	}
	reporter := buildReporter(sum, h.env.Vars, h.startTime, log)

	// 把请求字段摘要打到 UI 日志（完整 payload 已在 SaveTestData 用
	// log.Printf 写到 stderr，但 GUI 模式看不到），便于下次后端 4xx
	// 时不用切终端就能定位是哪个字段出问题。log 字段只打字节数不打内容。
	if h.env.Log != nil {
		h.env.Log.Info(fmt.Sprintf(
			"SaveTestData POST %s?dev_type=%s test_result=%v serial_num=%v last_start_time=%v log_bytes=%d",
			cfg.Blat.BaseURL+"/v1/tests/records", devTypeHeat,
			reporter["test_result"], reporter["serial_num"], reporter["last_start_time"], len(log)))
	}

	if h.skipOSS {
		// debug 模式：不触网，把要保存的数据（含原始日志）打印到日志。
		payload, err := json.MarshalIndent(reporter, "", "  ")
		if err != nil {
			h.env.Log.Error("debug 序列化上报数据失败: " + err.Error())
			return
		}
		h.env.Log.Info("debug 模式，不保存测试记录，上报数据如下:\n" + string(payload))
		return
	}

	// send_report_to_oss：日志压缩为 .lzma 后上传，路径按 日期/工位/时间 组织
	workstation := toStr(h.env.Vars["TEST_WORKSTATION"], "")
	now := time.Now()
	date := now.Format("20060102")
	logTime := now.Format("150405")
	ossPath := fmt.Sprintf("v2/%s/%s/log_%s.lzma", date, workstation, logTime)

	compressed, err := LzmaCompress([]byte(log))
	if err != nil {
		h.env.Log.Error("日志压缩失败: " + err.Error())
	} else if err := UploadLogOSS(context.Background(), ossPath, compressed); err != nil {
		h.env.Log.Error("日志上传OSS失败: " + err.Error())
	} else {
		h.env.Log.Info("日志已上传OSS: " + ossPath)
		// 日志已上传到 OSS，把 reporter 的 log 字段替换成 OSS 路径，
		// 避免把整段日志正文再次塞进数据库请求体（既冗余又容易触发 WAF/body size 限制）。
		reporter["log"] = ossPath
	}

	// HeatSaveTestData：把测试记录写入 BLAT 服务器数据库
	if err := SaveTestData(reporter, devTypeHeat); err != nil {
		h.env.Log.Error("保存数据失败: " + err.Error())
	} else {
		h.env.Log.Info("测试记录已保存")
	}
}

// buildReporter 组装 POST 到 BLAT 服务器的字段，JSON 键名与 BLAT hook_stop
// 完全一致。大多数字段来自 HeatNote（扫码信息），缺省时回退到环境变量。
// startTime 是 OnPlanStart 时记录的本次计划开始时间，作为 last_start_time 上报
// （后端 CreateHeatTestDataRequest.LastStartTime 要求 int64 unix 秒）。
func buildReporter(sum report.Summary, vars map[string]any, startTime time.Time, log string) map[string]any {
	hn, _ := vars["HeatNote"].(map[string]any)
	if hn == nil {
		hn = map[string]any{}
	}
	testResult := 0
	if sum.Result == "pass" {
		testResult = 1
	}
	// 后端 int64；startTime 为零值时留 0（omitempty 不输出）。原来取
	// hn["start_time"] 可能是 string，后端无法反序列化，所以直接用
	// OnPlanStart 时间戳，避免再依赖扫码字段。
	var lastStart int64
	if !startTime.IsZero() {
		lastStart = startTime.Unix()
	}
	return map[string]any{
		"test_result":     testResult,
		"serial_num":      toStr(hn["serial"], toStr(hn["mac"], "")),
		"user":            toStr(hn["user"], toStr(vars["user"], "")),
		"model":           toStr(hn["product"], toStr(hn["model"], "")),
		"test_mode":       toStr(hn["test_mode"], ""),
		"test_type":       "normal",
		"pn":              toStr(hn["pn"], ""),
		"lot":             toStr(hn["lot"], ""),
		"log":             log,
		"last_start_time": lastStart,
		"tool_version":    "",
		"workstation":     toStr(vars["TEST_WORKSTATION"], ""),
		"tenant_id":       toStr(hn["tenant_id"], ""),
		"fail_reason":     sum.Reason,
	}
}

// toStr 返回 v 的字符串值；v 不是 string（含 nil）时返回 def。
func toStr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

// toInt 把 int/int64/float64 转为 int；nil 或其他类型返回 0。
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}
