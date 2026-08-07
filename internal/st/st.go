// Package st 提供产品 ST 值与序列号、管径的映射解析。
//
// 序列号格式 \d{6}\d{2}\d{4}（可选 W/w 前缀）：第 7-8 位（中间 2 位）是
// ST 值（产品型号编码）。ST → 管径 的映射见 productFamilies（两级结构：
// 产品族 => 管径 => ST），管径 "" 表示该族无管径。
package st

import (
	"fmt"
	"regexp"
)

// serialRe 匹配可选 W/w 前缀后接 12 位数字，第 2 组是中间 2 位 ST。
var serialRe = regexp.MustCompile(`^[Ww]?(\d{6})(\d{2})(\d{4})$`)

// productFamilies 产品族 => 管径 => ST。管径 "" 表示该族无管径。
// 新增产品族/管径在此追加；ST 值全表必须唯一（init 构建反向索引时校验，
// 重复直接 panic，防反向查找歧义）。
var productFamilies = map[string]map[string]string{
	"二次网PVCB41":   {"": "07"},
	"二次网PVCC41":   {"": "08"},
	"二次网流量计PFW": {"40": "09"},
	"入户阀PTVB0":    {"20": "10", "25": "20"},
	"入户阀PTVB1":    {"20": "11", "25": "43", "32": "44"},
	"入户阀PTVF2":    {"20": "13", "25": "24"},
	"网关PGA":        {"": "15"},
	"测温PTC40":      {"20": "17"},
	"4G网关PGC":      {"": "26"},
	"二次网热量表PFWV2": {
		"40": "41", "50": "30", "65": "28", "80": "31",
		"100": "32", "125": "33", "150": "50",
	},
	"智能调节阀PSAV1": {
		"40": "40", "50": "29", "65": "34", "80": "35",
		"100": "36", "125": "37", "150": "51",
	},
	"智能调节阀PSAV2": {
		"40": "45", "50": "46", "65": "47", "80": "48", "100": "49",
	},
	"电子封印PELS": {"": "42"},
}

// stToPipe 反向索引：ST => 管径（由 productFamilies 构建）。
var stToPipe = buildSTIndex()

func buildSTIndex() map[string]string {
	idx := make(map[string]string, len(productFamilies)*2)
	for _, pipes := range productFamilies {
		for pipe, code := range pipes {
			if _, dup := idx[code]; dup {
				panic(fmt.Sprintf("ST %s 在映射表中重复", code))
			}
			idx[code] = pipe
		}
	}
	return idx
}

// STFromSerial 从序列号提取 ST 值（第 7-8 位）。支持可选 W/w 前缀；
// 格式不匹配时返回 ok=false。
func STFromSerial(serial string) (string, bool) {
	m := serialRe.FindStringSubmatch(serial)
	if m == nil {
		return "", false
	}
	return m[2], true
}

// PipeFromST 按 ST 值反查管径（字符串形式，如 "50"）。无管径族返回 ""；
// 未知 ST 返回 ok=false。
func PipeFromST(st string) (string, bool) {
	pipe, ok := stToPipe[st]
	return pipe, ok
}

// PipeFromSerial 从序列号解出管径：提取 ST 后查映射表。
func PipeFromSerial(serial string) (string, bool) {
	code, ok := STFromSerial(serial)
	if !ok {
		return "", false
	}
	return PipeFromST(code)
}
