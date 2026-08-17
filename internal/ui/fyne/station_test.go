package fyneui

import (
	"testing"
)

func TestValidateStationSN(t *testing.T) {
	cases := []struct {
		sn    string
		valid bool
	}{
		{"123456789012", true},
		{"000000000000", true},
		{"12345678901", false},  // 少一位
		{"1234567890123", false}, // 多一位
		{"12345678901A", false},  // 含字母
		{"W12345678901", false},  // 含 W 前缀（蓝牙单跑模式，面板禁用）
		{"", false},
		{"12345678901 ", false}, // 尾部空白不算有效
	}
	for _, c := range cases {
		err := validateStationSN(c.sn)
		if c.valid && err != nil {
			t.Errorf("validateStationSN(%q) want nil, got %v", c.sn, err)
		}
		if !c.valid && err == nil {
			t.Errorf("validateStationSN(%q) want error, got nil", c.sn)
		}
	}
}
