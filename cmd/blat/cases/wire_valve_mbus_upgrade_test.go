package cases

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"blat/internal/core"
	"blat/internal/device/mbus"
)

// ---- OTA 文件名解析（对应 Perl Utils.pm parseOtaFile L462-487）----

func TestParseOtaFileName(t *testing.T) {
	// 标准文件名：前缀 + 日期 + v版本 + _ota.bin
	info := parseOtaFileName("homeValve_PSAV_V2_20260807_v60_ota.bin")
	if !info.OK {
		t.Fatal("合法 OTA 文件名应解析成功")
	}
	if info.Prefix != "homeValve_PSAV_V2" || info.HardVer != 8 || info.SoftVer != 60 || info.ProtoType != 0xf9 {
		t.Fatalf("解析结果 = %+v, 期望 homeValve_PSAV_V2/8/60/0xf9", info)
	}

	// 前缀自带下划线（homeValveMulti_P1）：日期后缀不能破坏前缀（Perl
	// 非贪婪 `^(.*?)_\d{8}_v(\d+)_ota\.bin`，L478）
	info = parseOtaFileName("homeValveMulti_P1_20260807_v60_ota.bin")
	if !info.OK {
		t.Fatal("homeValveMulti_P1 应解析成功")
	}
	if info.Prefix != "homeValveMulti_P1" || info.HardVer != 20 || info.SoftVer != 60 || info.ProtoType != 0xf9 {
		t.Fatalf("解析结果 = %+v, 期望 homeValveMulti_P1/20/60/0xf9", info)
	}

	// 缺日期（不满足 `_\d{8}_`）→ OK=false
	if info := parseOtaFileName("homeValve_PSAV_V2_v5_ota.bin"); info.OK {
		t.Fatal("缺日期的文件名应解析失败")
	}

	// 前缀不在 programInfoMap（Perl 会 die；Go 返回 OK=false）
	if info := parseOtaFileName("homeValveMulti_P10_20260807_v5_ota.bin"); info.OK {
		t.Fatal("未知前缀应解析失败")
	}

	// 完整路径要取 basename 再解析
	info = parseOtaFileName(`C:\firmware\homeValve_PSAV_V2_20260807_v5_ota.bin`)
	if !info.OK || info.Prefix != "homeValve_PSAV_V2" {
		t.Fatalf("完整路径解析 = %+v, 期望取 basename 后 OK", info)
	}

	// ---- oracle 审查强化：完整匹配 + 真实日期 + 版本 0..255 ----
	// 拒绝 .bak/.tmp 等后缀（完整匹配 `_ota.bin$`，防刷错固件）
	if info := parseOtaFileName("homeValve_PSAV_V2_20260807_v5_ota.bin.bak"); info.OK {
		t.Fatal(".bak 后缀应解析失败")
	}
	if info := parseOtaFileName("homeValve_PSAV_V2_20260807_v5_ota.tmp"); info.OK {
		t.Fatal(".tmp 后缀应解析失败")
	}
	// 日期必须是真实日期（YYYYMMDD，拒绝 20261301 这类非法月）
	if info := parseOtaFileName("homeValve_PSAV_V2_20261301_v5_ota.bin"); info.OK {
		t.Fatal("非法日期 20261301 应解析失败")
	}
	// 版本必须 0..255（拒绝 v256/v300，固件版本号是单字节）
	if info := parseOtaFileName("homeValve_PSAV_V2_20260807_v256_ota.bin"); info.OK {
		t.Fatal("v256 应解析失败")
	}
}

func TestValidateUpgradeFirmware(t *testing.T) {
	ota := otaFileInfo{Prefix: "homeValve_PSAV_V2", HardVer: 8, SoftVer: 5, ProtoType: 0xf9, OK: true}

	// 版本与前缀都匹配 → 不跳过、无错误
	skip, err := validateUpgradeFirmware(8, 5, ota)
	if skip || err != nil {
		t.Fatalf("匹配应返回 (false, nil), 实际 (%v, %v)", skip, err)
	}

	// 版本不一致 → 错误含「版本号」
	otaVer := ota
	otaVer.SoftVer = 9
	skip, err = validateUpgradeFirmware(8, 5, otaVer)
	if skip || err == nil || !strings.Contains(err.Error(), "版本号") {
		t.Fatalf("版本不一致应报错含「版本号」, got (%v, %v)", skip, err)
	}

	// 前缀不匹配（HardVer=7 期望 homeValve_PFW_V2）→ 错误含「前缀」
	skip, err = validateUpgradeFirmware(7, 5, ota)
	if skip || err == nil || !strings.Contains(err.Error(), "前缀") {
		t.Fatalf("前缀不匹配应报错含「前缀」, got (%v, %v)", skip, err)
	}

	// 未知硬件版本 → skip=true（告警跳过，不视为失败）
	skip, err = validateUpgradeFirmware(99, 5, ota)
	if !skip || err != nil {
		t.Fatalf("未知硬件版本应 (true, nil), 实际 (%v, %v)", skip, err)
	}

	// 无法解析的 ota → 报错
	skip, err = validateUpgradeFirmware(8, 5, otaFileInfo{OK: false})
	if skip || err == nil {
		t.Fatalf("不可解析 ota 应报错, 实际 (%v, %v)", skip, err)
	}
}

// ---- findFFPadding（对齐 Perl L2168-2207 两趟扫描）----

func TestFindFFPadding_AllFF(t *testing.T) {
	// 全文件全 0xFF：整体跳过（返回 (0, totalBlocks)）
	firmware := bytes.Repeat([]byte{0xff}, 128)
	padStart, lastReal := findFFPadding(firmware, 1, 128)
	if padStart != 0 || lastReal != 1 {
		t.Fatalf("全 FF 固件 = (%d, %d), 期望 (0, 1)（全部跳过）", padStart, lastReal)
	}
}

func TestFindFFPadding_MiddlePadding(t *testing.T) {
	// 3 块：block0=AB+126ff（非全FF）、block1=128ff（全FF padding）、
	// block2=CD（非全FF 尾部）。第一趟从末尾回退：block2 非全FF → lastReal=2，
	// block1 全FF → break；第二趟从 block1 往前：block1 全FF → paddingStart=1，
	// block0 非全FF → break。
	var data []byte
	data = append(data, []byte("AB")...)
	data = append(data, bytes.Repeat([]byte{0xff}, 126)...) // block0
	data = append(data, bytes.Repeat([]byte{0xff}, 128)...) // block1 padding
	data = append(data, []byte("CD")...)                    // block2
	padStart, lastReal := findFFPadding(data, 3, 128)
	if padStart != 1 || lastReal != 2 {
		t.Fatalf("中间 padding = (%d, %d), 期望 (1, 2)", padStart, lastReal)
	}
}

func TestFindFFPadding_NoPadding(t *testing.T) {
	// 单块非全FF：第一趟把 lastReal 收敛到首块 0，paddingStart 保持
	// totalBlocks=1，得到 (1, 0)。跳过区间 [paddingStart, lastReal) 为空
	//（paddingStart >= lastReal），不会跳过任何块——与 Perl 一致（Perl 里
	// `$padding_start < $last_real_start` 为假，L2203）。
	data := append([]byte("AB"), bytes.Repeat([]byte{0xff}, 126)...)
	padStart, lastReal := findFFPadding(data, 1, 128)
	if padStart < lastReal {
		t.Fatalf("无 padding 时跳过区间应为空（padStart>=lastReal）, 实际 (%d, %d)", padStart, lastReal)
	}
}

func TestFindFFPadding_ThreeTrailingNonFF(t *testing.T) {
	// 末尾多个连续非全 FF 块（块 2..4 是尾部校验数据，Perl 注释
	// "固件结构: |真实数据|全 0xFF 填充|末尾非全 FF 校验值等|"）。
	// 第一趟从末尾逐块回退：i=4,3,2 均为非全FF（lastReal 依次收窄到 2），
	// i=1 全FF → break；第二趟从 block1 往前：block1 全FF → paddingStart=1。
	var data []byte
	data = append(data, []byte("AB")...)
	data = append(data, bytes.Repeat([]byte{0xff}, 126)...) // block0
	data = append(data, bytes.Repeat([]byte{0xff}, 128)...) // block1 padding
	data = append(data, []byte("CD")...)
	data = append(data, bytes.Repeat([]byte{0xff}, 126)...) // block2
	data = append(data, []byte("EF")...)
	data = append(data, bytes.Repeat([]byte{0xff}, 126)...) // block3
	data = append(data, []byte("GH")...)
	data = append(data, bytes.Repeat([]byte{0xff}, 110)...) // block4（末块 120B）
	padStart, lastReal := findFFPadding(data, 5, 128)
	if padStart != 1 || lastReal != 2 {
		t.Fatalf("多尾部非全FF块 = (%d, %d), 期望 (1, 2)", padStart, lastReal)
	}
}

// ---- mbus_upgrade_firmware case（对应 Perl wire_valve.pm L2097-2245）----

// writeFirmware 在 t.TempDir() 写一个临时固件文件，返回完整路径。
func writeFirmware(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("写临时固件失败: %v", err)
	}
	return path
}

// noSleep 测试注入的 sleep：不做真实 8s 等待。
var noSleep = func(context.Context, time.Duration) error { return nil }

// newConnectedMockMBus 构造已连接 + 已设置软/硬件版本的 mock mbus 设备。
func newConnectedMockMBus(t *testing.T, softVer, hardVer uint8) *mbus.Device {
	t.Helper()
	dev := mbus.NewMockDevice()
	if err := dev.Connect(context.Background(), "COM9"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	dev.SetMockInfo(mbus.MbusInfo{SoftVer: softVer, HardVer: hardVer})
	return dev
}

// 升级文件路径不存在 → 跳过成功（Warn 日志），不发任何块。
func TestUpgradeFirmware_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not_exist.bin")
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": missing}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	log := &recordLog{}
	dev := newConnectedMockMBus(t, 3, 8)
	if err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, log)); err != nil {
		t.Fatalf("固件缺失应跳过成功, 实际: %v", err)
	}
	if !log.contains("升级文件不存在或未配置") {
		t.Fatalf("日志应记录「升级文件不存在或未配置」, 实际: %v", log.msgs)
	}
}

// 设备已是目标版本 → 跳过升级，不发块。
func TestUpgradeFirmware_AlreadyTarget(t *testing.T) {
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", []byte("ABC"))
	dev := newConnectedMockMBus(t, 5, 8) // SoftVer=5 == 目标
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	log := &recordLog{}
	if err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, log)); err != nil {
		t.Fatalf("Run 意外错误: %v", err)
	}
	if got := dev.MockUpgradeBlocks(); len(got) != 0 {
		t.Fatalf("已是目标版本不应发送任何块, 实际 %d 块", len(got))
	}
	if !log.contains("已是目标版本") {
		t.Fatalf("日志应含「已是目标版本」, 实际: %v", log.msgs)
	}
}

// happy path 单块：3 字节固件发一块（BlockSize=3, IsEnd=true, Data=ABC）。
func TestUpgradeFirmware_HappyPathSingleBlock(t *testing.T) {
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", []byte("ABC"))
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	if err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{})); err != nil {
		t.Fatalf("Run 意外错误: %v", err)
	}
	blocks := dev.MockUpgradeBlocks()
	if len(blocks) != 1 {
		t.Fatalf("发送块数 = %d, 期望 1", len(blocks))
	}
	b := blocks[0]
	if b.BlockID != 0 || b.BlockSize != 3 || !b.IsEnd || b.HardVer != 8 || b.SoftVer != 5 || !bytes.Equal(b.Data, []byte("ABC")) {
		t.Fatalf("升级块 = %+v, 期望 BlockID=0 BlockSize=3 IsEnd=true HardVer=8 SoftVer=5 Data=ABC", b)
	}
}

// 跳过中间全 0xFF padding 块：只发 block0 与 block2，block_id 仍连续（0,1）。
func TestUpgradeFirmware_SkipsMiddleFFPadding(t *testing.T) {
	var data []byte
	data = append(data, []byte("AB")...)
	data = append(data, bytes.Repeat([]byte{0xff}, 126)...) // block0
	data = append(data, bytes.Repeat([]byte{0xff}, 128)...) // block1 padding
	data = append(data, []byte("CD")...)                    // block2
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", data)
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	if err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{})); err != nil {
		t.Fatalf("Run 意外错误: %v", err)
	}
	blocks := dev.MockUpgradeBlocks()
	if len(blocks) != 2 {
		t.Fatalf("发送块数 = %d, 期望 2（跳过中间 padding）", len(blocks))
	}
	if blocks[0].BlockID != 0 || blocks[1].BlockID != 1 {
		t.Fatalf("块序号应连续 0,1, 实际 %d,%d", blocks[0].BlockID, blocks[1].BlockID)
	}
	if blocks[1].BlockSize != 2 || !blocks[1].IsEnd || !bytes.Equal(blocks[1].Data, []byte("CD")) {
		t.Fatalf("第二块 = %+v, 期望 BlockSize=2 IsEnd=true Data=CD", blocks[1])
	}
}

// 固件文件名版本号与工单目标版本不一致 → 校验失败，不发块。
func TestUpgradeFirmware_VersionMismatch(t *testing.T) {
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v9_ota.bin", []byte("ABC"))
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{}))
	if err == nil || !strings.Contains(err.Error(), "版本号") {
		t.Fatalf("版本不一致应报错含「版本号」, got %v", err)
	}
	if got := len(dev.MockUpgradeBlocks()); got != 0 {
		t.Fatalf("校验失败不应发送块, 实际 %d", got)
	}
}

// 未知设备硬件版本 → 告警跳过（视为成功），不发块。
func TestUpgradeFirmware_UnknownHardverSkip(t *testing.T) {
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", []byte("ABC"))
	dev := newConnectedMockMBus(t, 3, 99) // 未知 HardVer
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	log := &recordLog{}
	if err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, log)); err != nil {
		t.Fatalf("未知硬件版本应跳过成功, 实际: %v", err)
	}
	if got := len(dev.MockUpgradeBlocks()); got != 0 {
		t.Fatalf("跳过不应发送块, 实际 %d", got)
	}
	if !log.contains("未知设备硬件版本") {
		t.Fatalf("日志应含「未知设备硬件版本」, 实际: %v", log.msgs)
	}
}

// 设备返回发送失败 → 错误包装「升级块 0 发送失败」。
func TestUpgradeFirmware_SendFailure(t *testing.T) {
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", []byte("ABC"))
	dev := newConnectedMockMBus(t, 3, 8)
	dev.SetMockUpgradeError(errors.New("boom"))
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{}))
	if err == nil || !strings.Contains(err.Error(), "升级块 0 发送失败") {
		t.Fatalf("发送失败应报错含「升级块 0 发送失败」, got %v", err)
	}
}

// 未配置 plan 参数时从 HeatNote.mbus.firmware 读升级文件路径。
func TestUpgradeFirmware_PathFromHeatNoteConfig(t *testing.T) {
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", []byte("ABC"))
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(nil); err != nil { // firmwarePath 留空
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	env := newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{})
	hn := env.Vars["HeatNote"].(map[string]any)
	hn["mbus"] = map[string]any{"port": "COM9", "firmware": path}
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("从 HeatNote.mbus.firmware 读路径应成功, 实际: %v", err)
	}
	if got := len(dev.MockUpgradeBlocks()); got != 1 {
		t.Fatalf("应发送 1 块, 实际 %d", got)
	}
}

// 未配置目标软件版本时从固件文件名 OTA 解析。
func TestUpgradeFirmware_TargetDerivedFromFilename(t *testing.T) {
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", []byte("ABC"))
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path}); err != nil { // 无软件版本
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	if err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{})); err != nil {
		t.Fatalf("Run 意外错误: %v", err)
	}
	blocks := dev.MockUpgradeBlocks()
	if len(blocks) != 1 {
		t.Fatalf("发送块数 = %d, 期望 1", len(blocks))
	}
	if blocks[0].SoftVer != 5 {
		t.Fatalf("目标版本应从文件名推导为 5, 实际 %d", blocks[0].SoftVer)
	}
}

// ---- oracle 审查修复项测试 ----

// 空文件 → 必须报错，不得成功（无有效数据块）。
func TestUpgradeFirmware_EmptyFile(t *testing.T) {
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", nil)
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{}))
	if err == nil || !strings.Contains(err.Error(), "无有效数据块") {
		t.Fatalf("空文件应报错「无有效数据块」, got %v", err)
	}
	if got := len(dev.MockUpgradeBlocks()); got != 0 {
		t.Fatalf("空文件不应发送块, 实际 %d", got)
	}
}

// 整文件全 0xFF → 必须报错，不得静默跳过全部块后成功。
func TestUpgradeFirmware_AllFF(t *testing.T) {
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", bytes.Repeat([]byte{0xff}, 128))
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{}))
	if err == nil || !strings.Contains(err.Error(), "无有效数据块") {
		t.Fatalf("全 0xFF 文件应报错「无有效数据块」, got %v", err)
	}
	if got := len(dev.MockUpgradeBlocks()); got != 0 {
		t.Fatalf("全 0xFF 文件不应发送块, 实际 %d", got)
	}
}

// 尾部全 FF padding：文件末块被跳过，最后实际发送块（block0）必须
// IsEnd=true（oracle 审查：Perl 按文件末块判 is_end，尾部 padding 时最后
// 实发块 is_end 恒 0，设备永远收不到结束标志）。
func TestUpgradeFirmware_TrailingFFPaddingIsEnd(t *testing.T) {
	var data []byte
	data = append(data, []byte("AB")...)
	data = append(data, bytes.Repeat([]byte{0xff}, 126)...) // block0 真实数据
	data = append(data, bytes.Repeat([]byte{0xff}, 128)...) // block1 尾部 padding
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", data)
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	if err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{})); err != nil {
		t.Fatalf("Run 意外错误: %v", err)
	}
	blocks := dev.MockUpgradeBlocks()
	if len(blocks) != 1 {
		t.Fatalf("发送块数 = %d, 期望 1（跳过尾部 padding）", len(blocks))
	}
	if !blocks[0].IsEnd {
		t.Fatalf("最后实际发送块必须 IsEnd=true, 实际 %+v", blocks[0])
	}
	if blocks[0].BlockID != 0 || blocks[0].BlockSize != 128 {
		t.Fatalf("升级块 = %+v, 期望 BlockID=0 BlockSize=128", blocks[0])
	}
}

// 未配置目标版本且 OTA 文件名无法解析 → 必须报错，不能以 SoftVer=0 误判
// "已是目标版本"跳过（oracle 审查）。
func TestUpgradeFirmware_OtaParseFailureNoTarget(t *testing.T) {
	// 文件名缺日期无法解析
	path := writeFirmware(t, "homeValve_PSAV_V2_v5_ota.bin", []byte("ABC"))
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path}); err != nil { // 无软件版本
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{}))
	if err == nil {
		t.Fatal("未配置目标版本且 OTA 解析失败应报错")
	}
	if got := len(dev.MockUpgradeBlocks()); got != 0 {
		t.Fatalf("不应发送块, 实际 %d", got)
	}
}

// 已配置目标版本但文件名无法解析 → 校验失败报错（版本号不一致），不发送块。
func TestUpgradeFirmware_BadFilenameWithTarget(t *testing.T) {
	path := writeFirmware(t, "random_file.bin", []byte("ABC"))
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{}))
	if err == nil {
		t.Fatal("已配置目标版本但文件名无法解析应报错")
	}
	if got := len(dev.MockUpgradeBlocks()); got != 0 {
		t.Fatalf("校验失败不应发送块, 实际 %d", got)
	}
}

// Configure 必须拒绝非法版本号：负数、超 255、非整数 float。
func TestUpgradeFirmware_Configure_InvalidVersion(t *testing.T) {
	for _, bad := range []map[string]any{
		{"软件版本": -1},
		{"软件版本": 256},
		{"软件版本": 300},
		{"软件版本": 5.7}, // 非整数 float
		{"soft_ver": 256},
		{"soft_ver": -5},
	} {
		c := &WireValveMBusUpgradeFirmwareCase{}
		if err := c.Configure(bad); err == nil {
			t.Errorf("Configure(%v) 应返回错误", bad)
		}
	}
}

// Configure 接受整数值 float64（YAML 常见解析结果）。
func TestUpgradeFirmware_Configure_AcceptFloatInt(t *testing.T) {
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"软件版本": float64(5)}); err != nil {
		t.Fatalf("整数 float64 应被接受: %v", err)
	}
	if !c.hasSoftVerTarget || c.softVerTarget != 5 {
		t.Fatalf("softVerTarget = %d has=%v, 期望 5/true", c.softVerTarget, c.hasSoftVerTarget)
	}
}

// ---- 固件快照（core.FirmwareSnapshot，冻结只读）----

// firmwareSnapshotFor 构造路径匹配的合法固件快照（SHA256 由 data 实算）。
func firmwareSnapshotFor(path string, data []byte) *core.FirmwareSnapshot {
	sum := sha256.Sum256(data)
	return &core.FirmwareSnapshot{
		Path:   path,
		Data:   data,
		Size:   int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]),
	}
}

// TestMatchFirmwarePath 校验 Windows 语义路径匹配：filepath.Clean 归一化 +
// EqualFold（不区分大小写）。
func TestMatchFirmwarePath(t *testing.T) {
	if !matchFirmwarePath(`C:\Dir\Fw.bin`, `c:\dir\fw.bin`) {
		t.Error("大小写不同的路径应匹配（Windows 语义）")
	}
	if !matchFirmwarePath(`C:\Dir\..\Dir\Fw.bin`, `C:\Dir\Fw.bin`) {
		t.Error("filepath.Clean 归一化后应匹配")
	}
	if matchFirmwarePath(`C:\Dir\A.bin`, `C:\Dir\B.bin`) {
		t.Error("不同文件不应匹配")
	}
	if matchFirmwarePath(``, `C:\Dir\A.bin`) {
		t.Error("空路径不应匹配")
	}
}

// 快照路径与所选路径匹配时，即使磁盘文件被删除，也使用快照数据（防
// 升级期间文件被替换/删除导致刷错固件）。
func TestUpgradeFirmware_SnapshotUsed_FileDeleted(t *testing.T) {
	content := []byte("SNAPSHOT")
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", content)
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	log := &recordLog{}
	env := newMBusRunEnvLog(&fakeUI{}, dev, log)
	env.Firmware = firmwareSnapshotFor(path, content)
	// 快照冻结后删除磁盘文件：快照匹配时应不 stat/不读盘，直接使用快照
	if err := os.Remove(path); err != nil {
		t.Fatalf("删除磁盘文件失败: %v", err)
	}
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("快照匹配时应使用快照成功, 实际: %v", err)
	}
	blocks := dev.MockUpgradeBlocks()
	if len(blocks) != 1 || !bytes.Equal(blocks[0].Data, content) {
		t.Fatalf("应使用快照数据 SNAPSHOT, 实际 %+v", blocks)
	}
	// 快照路径日志也应含完整 sha256
	sum := sha256.Sum256(content)
	if !log.contains(hex.EncodeToString(sum[:])) {
		t.Fatalf("快照路径日志应含 sha256, 实际: %v", log.msgs)
	}
}

// 快照路径与所选路径不匹配 → 不误用快照，保持磁盘路径行为（读取磁盘内容）。
func TestUpgradeFirmware_SnapshotPathMismatch(t *testing.T) {
	content := []byte("DISK")
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", content)
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	env := newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{})
	// 快照指向另一个文件：路径不匹配，不得误用快照数据
	env.Firmware = firmwareSnapshotFor(filepath.Join(t.TempDir(), "other.bin"), []byte("SNAP"))
	if err := c.Run(context.Background(), env); err != nil {
		t.Fatalf("快照路径不匹配应走磁盘路径成功, 实际: %v", err)
	}
	blocks := dev.MockUpgradeBlocks()
	if len(blocks) != 1 || !bytes.Equal(blocks[0].Data, content) {
		t.Fatalf("应使用磁盘内容 DISK, 实际 %+v", blocks)
	}
}

// 快照 Size 与 Data 长度不一致 → 返回错误，防快照损坏。
func TestUpgradeFirmware_SnapshotSizeMismatch(t *testing.T) {
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", []byte("XXXX"))
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	env := newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{})
	snap := firmwareSnapshotFor(path, []byte("ABC"))
	snap.Size = 999 // 人为破坏 Size
	env.Firmware = snap
	err := c.Run(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "Size") {
		t.Fatalf("快照 Size 不一致应报错含 Size, got %v", err)
	}
	if got := len(dev.MockUpgradeBlocks()); got != 0 {
		t.Fatalf("校验失败不应发送块, 实际 %d", got)
	}
}

// 快照 SHA256 与 Data 实算不一致 → 返回错误，防快照损坏。
func TestUpgradeFirmware_SnapshotHashMismatch(t *testing.T) {
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", []byte("XXXX"))
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	env := newMBusRunEnvLog(&fakeUI{}, dev, &recordLog{})
	snap := firmwareSnapshotFor(path, []byte("ABC"))
	snap.SHA256 = strings.Repeat("0", 64) // 人为破坏 hash
	env.Firmware = snap
	err := c.Run(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("快照 SHA256 不一致应报错含 SHA256, got %v", err)
	}
	if got := len(dev.MockUpgradeBlocks()); got != 0 {
		t.Fatalf("校验失败不应发送块, 实际 %d", got)
	}
}

// 磁盘路径（无快照）也要输出完整 sha256 与字节数日志。
func TestUpgradeFirmware_DiskPathLogsHash(t *testing.T) {
	content := []byte("ABC")
	path := writeFirmware(t, "homeValve_PSAV_V2_20260807_v5_ota.bin", content)
	dev := newConnectedMockMBus(t, 3, 8)
	c := &WireValveMBusUpgradeFirmwareCase{}
	if err := c.Configure(map[string]any{"升级文件": path, "软件版本": 5}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c.sleep = noSleep
	log := &recordLog{}
	if err := c.Run(context.Background(), newMBusRunEnvLog(&fakeUI{}, dev, log)); err != nil {
		t.Fatalf("Run 意外错误: %v", err)
	}
	sum := sha256.Sum256(content)
	if !log.contains(hex.EncodeToString(sum[:])) {
		t.Fatalf("磁盘路径日志应含 sha256, 实际: %v", log.msgs)
	}
	if !log.contains("3 bytes") {
		t.Fatalf("日志应含字节数, 实际: %v", log.msgs)
	}
}

// ---- 实际发送块数预检（oracle 复审：超 65536 必须发送前报错）----

// TestActualFirmwareBlocks 纯计数函数单测（不构造大文件）：总块数减去被
// 跳过的全 0xFF padding 段。覆盖无 padding / 中间 padding / 全 FF 与
// 65536/65537 边界。
func TestActualFirmwareBlocks(t *testing.T) {
	// 无 padding（paddingStart=totalBlocks, lastReal=0）
	if got := actualFirmwareBlocks(3, 3, 0); got != 3 {
		t.Errorf("无 padding 实际块数 = %d, 期望 3", got)
	}
	// 中间 padding 跳过 1 块
	if got := actualFirmwareBlocks(3, 1, 2); got != 2 {
		t.Errorf("中间 padding 实际块数 = %d, 期望 2", got)
	}
	// 全 FF：跳过全部块
	if got := actualFirmwareBlocks(3, 0, 3); got != 0 {
		t.Errorf("全 FF 实际块数 = %d, 期望 0", got)
	}
	// 尾部 padding（lastRealStart==totalBlocks 时跳过尾部）：
	// totalBlocks=2、paddingStart=1、lastReal=2 → 只发 1 块
	if got := actualFirmwareBlocks(2, 1, 2); got != 1 {
		t.Errorf("尾部 padding 实际块数 = %d, 期望 1", got)
	}
	// 边界：65536 合法、65537 超限（纯计数，不实际分配 8MB 文件）
	if got := actualFirmwareBlocks(65536, 65536, 0); got != 65536 {
		t.Errorf("边界 65536 实际块数 = %d", got)
	}
	if got := actualFirmwareBlocks(65537, 65537, 0); got != 65537 {
		t.Errorf("边界 65537 实际块数 = %d", got)
	}
}

// TestValidateUpgradeBlockCount 边界单测：0/负数与超 65536 报错，65536 及
// 以下合法。
func TestValidateUpgradeBlockCount(t *testing.T) {
	for _, ok := range []int{1, 128, 65536} {
		if err := validateUpgradeBlockCount(ok); err != nil {
			t.Errorf("实际块数 %d 应合法, got %v", ok, err)
		}
	}
	for _, bad := range []int{0, -1, 65537, 100000} {
		if err := validateUpgradeBlockCount(bad); err == nil {
			t.Errorf("实际块数 %d 应报错", bad)
		}
	}
	if err := validateUpgradeBlockCount(65537); err == nil || !strings.Contains(err.Error(), "超出上限") {
		t.Errorf("65537 错误应含「超出上限」, got %v", err)
	}
}
