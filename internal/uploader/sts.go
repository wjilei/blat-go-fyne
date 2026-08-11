package uploader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// stsToken 是 BLAT 后台 GET /v1/ststoken 返回的临时凭证。
// 服务端用 AssumeRole(900) 生成，900s 过期；客户端应在过期前续期。
type stsToken struct {
	AccessKeyID     string    `json:"accessKeyId"`
	AccessKeySecret string    `json:"accessKeySecret"`
	SecurityToken   string    `json:"securityToken"`
	Expiration      time.Time `json:"-"`
	ExpirationRaw   string    `json:"expiration"`
}

// UnmarshalJSON 把 expiration 多种格式解析为 time.Time；解析失败保留零值，便于上层判定。
// 支持 RFC3339 与 Go 默认 time.Time.String() 格式（兼容 BLAT 后台不同版本的返回格式）。
func (t *stsToken) UnmarshalJSON(data []byte) error {
	type alias stsToken
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*t = stsToken(a)
	if a.ExpirationRaw != "" {
		t.Expiration = parseExpiration(a.ExpirationRaw)
	}
	return nil
}

// parseExpiration 按优先级尝试解析时间字符串，支持 RFC3339 与 Go 默认格式。
func parseExpiration(s string) time.Time {
	formats := []string{
		time.RFC3339,                          // "2006-01-02T15:04:05Z07:00"
		"2006-01-02 15:04:05 -0700 MST",      // Go time.Time.String() 无小数秒
		"2006-01-02 15:04:05.999999999 -0700 MST", // Go time.Time.String() 带小数秒
	}
	for _, f := range formats {
		if ts, err := time.Parse(f, s); err == nil {
			return ts
		}
	}
	return time.Time{}
}

// valid 判定 token 是否仍然可信。保守起见，Expiration 解析失败时一律视为已过期，
// 强迫调用方重新拉一次（避免服务端改字段格式后客户端傻用过期 key）。
func (t stsToken) valid(now time.Time) bool {
	if t.AccessKeyID == "" || t.AccessKeySecret == "" || t.SecurityToken == "" {
		return false
	}
	if t.Expiration.IsZero() {
		return false
	}
	// 提前 60s 续期，避免在请求途中 token 失效导致 403。
	return now.Add(60 * time.Second).Before(t.Expiration)
}

// tokenSource 抽象出"获取 STS 凭证"的能力，便于测试中替换。
type tokenSource interface {
	Token(ctx context.Context) (stsToken, error)
}

// defaultTokenSource 是运行时实际使用的 source，默认指向 blatSTSSource。
// 测试可通过赋值替换（uploader_test.go 的 withTestConfig 会用 testTokenSource 包一层）。
var defaultTokenSource tokenSource = newBlatSTSSource()

// testTokenSource 让测试可以观察内部源而不改写全局；目前仅复用同一实现，避免
// 引入额外抽象负担。后续若要做并发续期测试，可在 Source 上加 instrumented 包装。
type testTokenSource struct {
	cfg Config
	src tokenSource
}

func (s *testTokenSource) Token(ctx context.Context) (stsToken, error) {
	return s.src.Token(ctx)
}

// newBlatSTSSource 返回默认的 BLAT 后台 tokenSource。
func newBlatSTSSource() *blatSTSSource {
	return &blatSTSSource{}
}

// blatSTSSource 通过 BLAT 后台 GET /v1/ststoken 拉 STS 临时凭证，
// 内置进程级缓存：到期前 60s 续期；首次取或缓存失效时拉新。
type blatSTSSource struct {
	mu    sync.Mutex
	cache stsToken
}

// Token 返回当前可用 STS；首次或过期时拉一次。
func (s *blatSTSSource) Token(ctx context.Context) (stsToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache.valid(time.Now()) {
		return s.cache, nil
	}
	tok, err := fetchSTSToken(ctx)
	if err != nil {
		return stsToken{}, err
	}
	s.cache = tok
	return tok, nil
}

// fetchSTSToken 调一次 /v1/ststoken，失败原因直接返回（带状态码便于诊断）。
// 200 但 Expiration 不可解析时返回 ErrSTSTokenUntrusted，让上层强制重试一次。
func fetchSTSToken(ctx context.Context) (stsToken, error) {
	if cfg.Blat.BaseURL == "" {
		return stsToken{}, errors.New("uploader: Blat.BaseURL 为空，无法获取 STS")
	}
	url := cfg.Blat.BaseURL + "/v1/ststoken"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return stsToken{}, err
	}
	// 服务端 GlobalRead: true 不强校验 token，但带上更合规，也便于服务端审计。
	if cfg.Blat.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Blat.Token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return stsToken{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return stsToken{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return stsToken{}, fmt.Errorf("uploader: /v1/ststoken 返回 %d: %s", resp.StatusCode, string(body))
	}
	// BLAT 后台 sendOk() 把数据包在 {"data":{...},"result":true} 里，
	// 先解包 data 再反序列化为 stsToken。
	var tok stsToken
	{
		var wrapper struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Data != nil {
			_ = json.Unmarshal(wrapper.Data, &tok)
		}
	}
	// 兼容旧格式：data 解包失败或不存在时，直接反序列化整个 body。
	if tok.AccessKeyID == "" {
		if err := json.Unmarshal(body, &tok); err != nil {
			return stsToken{}, fmt.Errorf("uploader: /v1/ststoken 响应不是合法 JSON: %w", err)
		}
	}
	if !tok.valid(time.Now()) {
		log.Printf("[uploader] STS token 验证失败，服务端原始响应: %s", string(body))
		log.Printf("[uploader] 解析后: ak=%q (len=%d) skEmpty=%v stsEmpty=%v expirationRaw=%q expirationParsed=%v",
			maskKey(tok.AccessKeyID), len(tok.AccessKeyID),
			tok.AccessKeySecret == "", tok.SecurityToken == "",
			tok.ExpirationRaw, tok.Expiration)
		return stsToken{}, ErrSTSTokenUntrusted
	}
	return tok, nil
}

// maskKey 遮蔽密钥，方便日志里安全输出。
func maskKey(k string) string {
	if len(k) <= 4 {
		return "***"
	}
	return k[:2] + "***" + k[len(k)-2:]
}

// ErrSTSTokenUntrusted 表示拿到了 200 响应但 Expiration 不可信（空、解析失败、
// 已过期），调用方应视情况重试。
var ErrSTSTokenUntrusted = errors.New("uploader: STS 响应 Expiration 不可信")