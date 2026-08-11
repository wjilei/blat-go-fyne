package uploader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSTSHandler 把每次请求的次数与最近一次的 Authorization 都记录下来。
// expOffset 用于生成"过期时间=now+offset" 的 expiration 字段；<0 表示立即过期。
func fakeSTSHandler(expOffset time.Duration, calls *atomic.Int32, lastAuth *atomic.Value) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h := r.Header.Get("Authorization")
		lastAuth.Store(h)
		resp := map[string]string{
			"accessKeyId":     "STS.ak",
			"accessKeySecret": "STS.sk",
			"securityToken":   "STS.token",
			"expiration":      time.Now().Add(expOffset).UTC().Format(time.RFC3339),
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
}

// fakeOSSHandler 接收 PUT，记录 method/path/Authorization/x-oss-security-token/body 长度，
// 返回 200 OK。这是模拟 OSS：客户端发的 STS 会出现在 x-oss-security-token 里。
func fakeOSSHandler(auths *[]string, tokens *[]string, bodies *[]int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			n, _ := io.Copy(io.Discard, r.Body)
			*auths = append(*auths, r.Header.Get("Authorization"))
			*tokens = append(*tokens, r.Header.Get("x-oss-security-token"))
			*bodies = append(*bodies, int(n))
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// stsBlatServer 同时托管 /v1/ststoken + OSS 端点（任意 path 的 PUT）。Client.BaseURL 指向它。
// 测试里把 OSS.Endpoint 也指向 srv.URL（fake server 是 HTTP），由 fakeOSSHandler 接收 PUT。
func stsBlatServer(stsHandler, ossHandler http.Handler) *httptest.Server {
	mux := http.NewServeMux()
	mux.Handle("/v1/ststoken", stsHandler)
	mux.Handle("/", ossHandler)
	return httptest.NewServer(mux)
}

// withTestConfig 临时把 cfg 替换成测试配置，结束后恢复；避免污染其他测试。
func withTestConfig(t *testing.T, c Config) {
	t.Helper()
	old := cfg
	cfg = c
	t.Cleanup(func() { cfg = old })
	oldSrc := defaultTokenSource
	defaultTokenSource = &testTokenSource{cfg: c, src: newBlatSTSSource()}
	t.Cleanup(func() { defaultTokenSource = oldSrc })
}

// TestUploadLogOSS_FetchesSTSAndUploads 验证：第一次上传会触发一次 /v1/ststoken，
// 把 STS 临时凭证传到 OSS（通过 x-oss-security-token），而不是长效 AccessKey。
func TestUploadLogOSS_FetchesSTSAndUploads(t *testing.T) {
	var stsCalls atomic.Int32
	var lastAuth atomic.Value
	sts := fakeSTSHandler(15*time.Minute, &stsCalls, &lastAuth)

	var ossAuths, ossTokens []string
	var ossBodies []int
	oss := fakeOSSHandler(&ossAuths, &ossTokens, &ossBodies)

	srv := stsBlatServer(sts, oss)
	defer srv.Close()

	withTestConfig(t, Config{
		OSS:  OSSConfig{Endpoint: srv.URL, LogBucket: "blat-app-log"},
		Blat: BlatConfig{BaseURL: srv.URL, Token: "blat-bearer"},
	})

	if err := UploadLogOSS(context.Background(), "v2/20260101/01/log_120000.lzma", []byte("hello-oss")); err != nil {
		t.Fatalf("UploadLogOSS() error = %v", err)
	}
	if got := stsCalls.Load(); got != 1 {
		t.Errorf("/v1/ststoken 调用次数 = %d, want 1", got)
	}
	if got := lastAuth.Load(); got != "Bearer blat-bearer" {
		t.Errorf("/v1/ststoken Authorization = %v, want %q", got, "Bearer blat-bearer")
	}
	if len(ossBodies) != 1 || ossBodies[0] != len("hello-oss") {
		t.Errorf("OSS PUT 次数/字节 = %v/%v, want 1/%d", len(ossBodies), ossBodies, len("hello-oss"))
	}
	if len(ossTokens) != 1 || ossTokens[0] != "STS.token" {
		t.Errorf("OSS x-oss-security-token = %v, want [STS.token]", ossTokens)
	}
	if len(ossAuths) != 1 || !strings.Contains(ossAuths[0], "OSS") {
		t.Errorf("OSS Authorization header 异常: %v", ossAuths)
	}
}

// TestUploadLogOSS_STSExpiryForcesRefresh 验证：服务端返回已过期的 STS 时，
// 上传路径会重新拉一次而不是用过期 key 上传。
func TestUploadLogOSS_STSExpiryForcesRefresh(t *testing.T) {
	var stsCalls atomic.Int32
	var stsIdx atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ststoken", func(w http.ResponseWriter, r *http.Request) {
		idx := stsIdx.Add(1)
		stsCalls.Add(1)
		// 第一次：已过期；第二次：长有效。
		offset := -time.Minute
		if idx == 2 {
			offset = 15 * time.Minute
		}
		resp := map[string]string{
			"accessKeyId":     fmt.Sprintf("STS.ak%d", idx),
			"accessKeySecret": fmt.Sprintf("STS.sk%d", idx),
			"securityToken":   fmt.Sprintf("STS.token%d", idx),
			"expiration":      time.Now().Add(offset).UTC().Format(time.RFC3339),
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	var ossTokens []string
	oss := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			ossTokens = append(ossTokens, r.Header.Get("x-oss-security-token"))
			w.WriteHeader(http.StatusOK)
		}
	})
	mux.Handle("/", oss)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	withTestConfig(t, Config{
		OSS:  OSSConfig{Endpoint: srv.URL, LogBucket: "blat-app-log"},
		Blat: BlatConfig{BaseURL: srv.URL, Token: "t"},
	})

	if err := UploadLogOSS(context.Background(), "p", []byte("x")); err != nil {
		t.Fatalf("UploadLogOSS() error = %v", err)
	}
	if got := stsCalls.Load(); got < 2 {
		t.Errorf("/v1/ststoken 调用次数 = %d, want >=2 (过期 STS 应触发续期)", got)
	}
	if len(ossTokens) == 0 || ossTokens[len(ossTokens)-1] != "STS.token2" {
		t.Errorf("OSS 收到的最终 token = %v, want 以 STS.token2 结尾（续期后）", ossTokens)
	}
}

// TestUploadLogOSS_STSServerErrorRetried 验证：/v1/ststoken 首次返回 500 时，
// 上传路径会重试，直到拿到有效 STS 再上传。
func TestUploadLogOSS_STSServerErrorRetried(t *testing.T) {
	var stsCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ststoken", func(w http.ResponseWriter, r *http.Request) {
		idx := stsCalls.Add(1)
		if idx == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		resp := map[string]string{
			"accessKeyId":     "STS.ak",
			"accessKeySecret": "STS.sk",
			"securityToken":   "STS.token",
			"expiration":      time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339),
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	oss := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
		}
	})
	mux.Handle("/", oss)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	withTestConfig(t, Config{
		OSS:  OSSConfig{Endpoint: srv.URL, LogBucket: "blat-app-log"},
		Blat: BlatConfig{BaseURL: srv.URL, Token: "t"},
	})

	if err := UploadLogOSS(context.Background(), "p", []byte("y")); err != nil {
		t.Fatalf("UploadLogOSS() error = %v", err)
	}
	if got := stsCalls.Load(); got < 2 {
		t.Errorf("/v1/ststoken 调用次数 = %d, want >=2 (首次 500 应重试)", got)
	}
}

// TestUploadLogOSS_PutObjectErrorPropagates 验证：当 OSS PUT 持续失败，
// UploadLogOSS 最终返回非 nil 错误（不静默吞掉）。
func TestUploadLogOSS_PutObjectErrorPropagates(t *testing.T) {
	sts := fakeSTSHandler(15*time.Minute, new(atomic.Int32), new(atomic.Value))
	oss := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	mux := http.NewServeMux()
	mux.Handle("/v1/ststoken", sts)
	mux.Handle("/", oss)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	withTestConfig(t, Config{
		OSS:  OSSConfig{Endpoint: srv.URL, LogBucket: "blat-app-log"},
		Blat: BlatConfig{BaseURL: srv.URL, Token: "t"},
	})

	if err := UploadLogOSS(context.Background(), "p", []byte("y")); err == nil {
		t.Fatal("UploadLogOSS() error = nil, want OSS 持续失败时返回 error")
	}
}