package st

import "testing"

func TestSTFromSerial(t *testing.T) {
	cases := []struct {
		serial string
		want   string
		ok     bool
	}{
		{"262601300011", "30", true}, // 无前缀，env.yml 演示序列号 → ST 30
		{"W262601300011", "30", true}, // 大写 W 前缀
		{"w262601300011", "30", true}, // 小写 w 前缀
		{"123456789012", "78", true},  // 边界：第 7-8 位
		{"123456000000", "00", true},  // ST 全 0 也合法
		{"", "", false},               // 空
		{"123456789", "", false},      // 9 位
		{"1234567890123", "", false},  // 13 位
		{"abcdefghijkl", "", false},   // 非数字
		{"2626a1300011", "", false},   // 混入字母
		{"WW262601300011", "", false}, // 双 W
	}
	for _, tc := range cases {
		got, ok := STFromSerial(tc.serial)
		if got != tc.want || ok != tc.ok {
			t.Errorf("STFromSerial(%q) = %q, %v; want %q, %v",
				tc.serial, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPipeFromST(t *testing.T) {
	cases := []struct {
		st   string
		want string
		ok   bool
	}{
		{"07", "", true},  // 二次网PVCB41 无管径
		{"08", "", true},  // 二次网PVCC41 无管径
		{"09", "40", true}, // PFW 40
		{"10", "20", true}, // PTVB0 20
		{"20", "25", true}, // PTVB0 25
		{"11", "20", true}, // PTVB1 20
		{"43", "25", true}, // PTVB1 25
		{"44", "32", true}, // PTVB1 32
		{"13", "20", true}, // PTVF2 20
		{"24", "25", true}, // PTVF2 25
		{"15", "", true},  // 网关PGA 无管径
		{"17", "20", true}, // 测温PTC40 20
		{"26", "", true},  // 4G网关PGC 无管径
		{"41", "40", true}, // PFWV2 40
		{"30", "50", true}, // PFWV2 50
		{"28", "65", true}, // PFWV2 65
		{"31", "80", true}, // PFWV2 80
		{"32", "100", true}, // PFWV2 100
		{"33", "125", true}, // PFWV2 125
		{"50", "150", true}, // PFWV2 150
		{"40", "40", true}, // PSAV1 40
		{"29", "50", true}, // PSAV1 50
		{"34", "65", true}, // PSAV1 65
		{"35", "80", true}, // PSAV1 80
		{"36", "100", true}, // PSAV1 100
		{"37", "125", true}, // PSAV1 125
		{"51", "150", true}, // PSAV1 150
		{"45", "40", true}, // PSAV2 40
		{"46", "50", true}, // PSAV2 50
		{"47", "65", true}, // PSAV2 65
		{"48", "80", true}, // PSAV2 80
		{"49", "100", true}, // PSAV2 100
		{"42", "", true},  // 电子封印PELS 无管径
		{"99", "", false}, // 未知 ST
		{"00", "", false}, // 未知 ST
		{"60", "", false}, // 未知 ST
	}
	for _, tc := range cases {
		got, ok := PipeFromST(tc.st)
		if got != tc.want || ok != tc.ok {
			t.Errorf("PipeFromST(%q) = %q, %v; want %q, %v",
				tc.st, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPipeFromSerial(t *testing.T) {
	// env.yml 演示序列号：262601300011 → ST 30 → PFWV2 管径 50
	pipe, ok := PipeFromSerial("262601300011")
	if !ok || pipe != "50" {
		t.Errorf("PipeFromSerial(262601300011) = %q, %v; want %q, true", pipe, ok, "50")
	}
	// W 前缀
	pipe, ok = PipeFromSerial("W262601300011")
	if !ok || pipe != "50" {
		t.Errorf("PipeFromSerial(W262601300011) = %q, %v; want %q, true", pipe, ok, "50")
	}
	// 无管径族：PELS ST 42
	pipe, ok = PipeFromSerial("262601420011")
	if !ok || pipe != "" {
		t.Errorf("PipeFromSerial(262601420011) = %q, %v; want \"\", true", pipe, ok)
	}
	// 格式非法
	if _, ok := PipeFromSerial("2626013000"); ok {
		t.Error("PipeFromSerial(短序列号) 应返回 ok=false")
	}
	// ST 未知
	if _, ok := PipeFromSerial("262601990011"); ok {
		t.Error("PipeFromSerial(未知 ST 99) 应返回 ok=false")
	}
}

// TestSTUnique 校验映射表内 ST 值全局唯一——反向查找（ST→管径）依赖唯一性，
// 表维护时引入重复 ST 会让结果歧义，此处拦截。
func TestSTUnique(t *testing.T) {
	seen := make(map[string]string, len(productFamilies)*2)
	for fam, pipes := range productFamilies {
		for pipe, code := range pipes {
			if prevFam, dup := seen[code]; dup {
				t.Errorf("ST %s 重复: %s 与 %s（管径 %s）", code, prevFam, fam, pipe)
			}
			seen[code] = fam
		}
	}
}
