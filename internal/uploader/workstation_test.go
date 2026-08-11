package uploader

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withBlatURL 在测试期间把 cfg.Blat.BaseURL/Token 指到 srv.URL，结束后还原。
// 包级 cfg 是共享状态，必须在每个用例里隔离避免互相污染。
func withBlatURL(t *testing.T, srvURL string) {
	t.Helper()
	saved := cfg.Blat
	cfg.Blat.BaseURL = srvURL
	cfg.Blat.Token = "test-token"
	t.Cleanup(func() { cfg.Blat = saved })
}

func TestGetWorkstationInfo_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/work-stations/get-or-create" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("want POST, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q", got)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["uuid"] != "abc" || body["osver"] != "windows" {
			t.Errorf("body = %v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": 1,
			"data":   map[string]any{"id": 5, "name": "工位5", "uuid": "abc"},
		})
	}))
	defer srv.Close()
	withBlatURL(t, srv.URL)

	got, err := GetWorkstationInfo("abc", "windows")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "工位5" {
		t.Errorf("got %q, want 工位5", got)
	}
}

func TestGetWorkstationInfo_ResultFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": 0})
	}))
	defer srv.Close()
	withBlatURL(t, srv.URL)

	got, err := GetWorkstationInfo("u", "windows")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if got != "" {
		t.Errorf("on error got %q, want empty", got)
	}
}

func TestGetWorkstationInfo_NoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"result": 1})
	}))
	defer srv.Close()
	withBlatURL(t, srv.URL)

	_, err := GetWorkstationInfo("u", "windows")
	if err == nil || !strings.Contains(err.Error(), "data.name") {
		t.Fatalf("want data.name error, got %v", err)
	}
}

func TestGetWorkstationInfo_NoName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": 1,
			"data":   map[string]any{"id": 7},
		})
	}))
	defer srv.Close()
	withBlatURL(t, srv.URL)

	_, err := GetWorkstationInfo("u", "windows")
	if err == nil {
		t.Fatal("want error when data.name missing")
	}
}

func TestGetWorkstationInfo_EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 返回空 body，按规则视为不可解析
	}))
	defer srv.Close()
	withBlatURL(t, srv.URL)

	_, err := GetWorkstationInfo("u", "windows")
	if err == nil {
		t.Fatal("want error on empty body")
	}
}

func TestGetWorkstationInfo_Fatal500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	withBlatURL(t, srv.URL)

	_, err := GetWorkstationInfo("u", "windows")
	if err == nil {
		t.Fatal("want fatal error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want mention 500", err)
	}
}

func TestGetWorkstationInfo_Fatal400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	withBlatURL(t, srv.URL)

	_, err := GetWorkstationInfo("u", "windows")
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("want 400 fatal, got %v", err)
	}
}

func TestGetWorkstationInfo_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // 立即关闭，模拟不可达
	withBlatURL(t, srv.URL)

	_, err := GetWorkstationInfo("u", "windows")
	if err == nil {
		t.Fatal("want network error")
	}
}
