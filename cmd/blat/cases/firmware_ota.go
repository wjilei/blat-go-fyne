package cases

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// otaFileInfo 是 OTA 固件文件名解析结果（对应 Perl
// BLAT::Common::Utils::parseOtaFile L462-487）。
type otaFileInfo struct {
	Prefix    string
	HardVer   int
	SoftVer   int
	ProtoType int
	OK        bool
}

// programInfoMap 对齐 Perl Utils.pm %ProgramInfoMap（前缀 → 硬版本 / 协议类型）。
// 注释里 UnitValve CBOR_TYPE=0xf9、UnitHeatMeter CBOR_TYPE=0xf8、Seal
// CBOR_TYPE=0xf7（与嵌入式工程 CMakeFiles.txt 里定义一致）。
var programInfoMap = map[string]struct {
	HardVer   int
	ProtoType int
}{
	"homeValveMulti_P1": {20, 0xf9},
	"homeValve_PSAV_V2": {8, 0xf9},
	"homeValve_PFW_V2":  {7, 0xf8},
	"homeGateway":       {4, 0},
	"homeGateway_4G":    {5, 0},
	"tempMeasure_P2":    {11, 0xfd},
	"iotSeal":           {10, 0xf7},
	"iotSeal_P2":        {12, 0xf7},
}

// otaNameRe 对应 Perl `^(.*?)_\d{8}_v(\d+)_ota\.bin`（L478，非贪婪前缀让
// homeValveMulti_P1 这类自带下划线的前缀不被日期截断）。Go 侧锚定结尾
// （oracle 审查强化）：必须完整匹配 `前缀_YYYYMMDD_v版本_ota.bin`，拒绝
// .bak/.tmp 等后缀，防止刷错固件文件。
var otaNameRe = regexp.MustCompile(`^(.*?)_(\d{8})_v(\d+)_ota\.bin$`)

// parseOtaFileName 解析 OTA 文件名：要求 `前缀_YYYYMMDD_v版本_ota.bin` 且前缀在
// programInfoMap 中（Perl 未命中会 die；Go 返回 OK=false）。oracle 审查
// 强化：日期必须是真实日期（YYYYMMDD，Perl 只查 \d{8}）、版本必须 0..255
// （固件版本号是单字节，拒绝 v256/v300）。输入可为完整路径，内部先取
// basename（Perl path($f)->basename）。
func parseOtaFileName(firmwarePath string) otaFileInfo {
	name := filepath.Base(firmwarePath)
	m := otaNameRe.FindStringSubmatch(name)
	if m == nil {
		return otaFileInfo{OK: false}
	}
	info, ok := programInfoMap[m[1]]
	if !ok {
		return otaFileInfo{OK: false}
	}
	// 日期必须是真实日历日期（time.Parse 拒绝 20261301 这类非法月/日）
	if _, err := time.Parse("20060102", m[2]); err != nil {
		return otaFileInfo{OK: false}
	}
	softVer, err := strconv.Atoi(m[3])
	if err != nil {
		return otaFileInfo{OK: false}
	}
	if softVer < 0 || softVer > 255 {
		return otaFileInfo{OK: false}
	}
	return otaFileInfo{
		Prefix:    m[1],
		HardVer:   info.HardVer,
		SoftVer:   softVer,
		ProtoType: info.ProtoType,
		OK:        true,
	}
}

// hardVerPrefix 对齐 Perl _validate_upgrade_firmware 的 %hardver_prefix
// （UserValve.pm L1817-1826）。
var hardVerPrefix = map[int]string{
	7:  "homeValve_PFW_V2",
	8:  "homeValve_PSAV_V2",
	4:  "homeGateway",
	5:  "homeGateway_4G",
	20: "homeValveMulti_P1",
	10: "iotSeal",
	11: "tempMeasure_P2",
	12: "iotSeal_P2",
}

// validateUpgradeFirmware 对齐 Perl _validate_upgrade_firmware L1809-1845：
// 同时校验固件文件版本号（soft_ver == softVerTarget）与文件名前缀
// （hardver 期望的前缀）。返回 (skip, err)：skip=true 表示"未知设备硬件
// 版本"，仅告警跳过（用例视作成功）；err!=nil 表示应判定失败。
//
// 分支顺序对齐 Perl L1832-1843：先判版本，再判未知硬版本，最后判前缀。
func validateUpgradeFirmware(deviceHardVer int, softVerTarget int, ota otaFileInfo) (bool, error) {
	expectPre, known := hardVerPrefix[deviceHardVer]
	fwVerOK := ota.OK && ota.SoftVer == softVerTarget
	prefixOK := known && ota.OK && ota.Prefix == expectPre
	if fwVerOK && prefixOK {
		return false, nil
	}
	if !fwVerOK {
		ver := ""
		if ota.OK {
			ver = strconv.Itoa(ota.SoftVer)
		}
		return false, fmt.Errorf("固件文件版本号 (v%s) 与工单软件版本 (%d) 不一致", ver, softVerTarget)
	}
	if !known {
		return true, nil // 未知硬件版本 → warn+skip
	}
	return false, fmt.Errorf("固件文件名前缀 (%s) 与设备硬件版本 HardVer=%d 不匹配 (期望 %s)", ota.Prefix, deviceHardVer, expectPre)
}
