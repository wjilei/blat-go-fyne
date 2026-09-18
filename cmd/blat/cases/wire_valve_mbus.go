package cases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"blat/internal/core"
	"blat/internal/device/mbus"
)

// WireValveMBusReadMotorCase 翻译自 Perl
// BLAT::APP::Heat::Cases::wire_valve::dev_normal_check_motor 的"两阶段
// 开度人工确认"变体。流程：
//  1. 通过 M-Bus 设置阀门开度到 step1Open（默认 80）；
//  2. 弹框询问用户确认电机已开始转动；
//  3. 弹框提示用户等电机转完到目标位置后手动点确认；
//  4. 读取 M-Bus 设备信息校验告警位（Alarm 必须为 0，非 0 视为电机
//     状态异常，立即失败不恢复开度）；
//  5. 再次设置阀门开度到 step2Open（默认 100，恢复开度）；
//  6. 在日志里提示用户等电机转完再断电，避免带电拔线烧驱动。
//
// 注意：本用例不读取 M-Bus 的开度反馈——设备回读的不是实时值，机械
// 到位要靠肉眼确认。流程靠两次人工弹框驱动。
//
// 不判断 test_mode==normal——本 Run 场景本身就是 normal（产线 normal
// 计划下才会执行此用例，对应 Perl 里的 if 分支在 Run 路径恒为真）。
type WireValveMBusReadMotorCase struct {
	step1Open int // 阶段 1 目标开度（%），默认 80
	step2Open int // 阶段 2 恢复开度（%），默认 100
}

func (c *WireValveMBusReadMotorCase) Name() string {
	return "wire_valve_mbus_read_motor"
}

// Configure 读取 plan 的自定义参数。键名中文优先，同时兼容英文键
// step1_open/step2_open；未知键忽略不报错。
// 整数读取兼容 YAML 解析出的 int/int64/float64。
func (c *WireValveMBusReadMotorCase) Configure(args map[string]any) error {
	// 默认值
	c.step1Open = 80
	c.step2Open = 100

	// 阶段开度：中文键优先，兼容英文；非 [0,100] 视为非法值忽略
	if v, ok := lookupInt(args, "第一阶段开度", "step1_open"); ok && v >= 0 && v <= 100 {
		c.step1Open = v
	}
	if v, ok := lookupInt(args, "第二阶段开度", "step2_open"); ok && v >= 0 && v <= 100 {
		c.step2Open = v
	}
	return nil
}

// lookupInt 从 args 中按顺序查 keys（中文→英文），命中第一个存在的键。
// 返回 (值, 是否命中)；YAML 解析的 int/int64/float64 都接受。无可识别值
// 返回 (0, false)，调用方据此判断是否覆盖默认值。
func lookupInt(args map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := args[k]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case int:
			return x, true
		case int64:
			return int(x), true
		case float64:
			return int(x), true
		}
	}
	return 0, false
}

func (c *WireValveMBusReadMotorCase) Run(ctx context.Context, env *core.Env) error {
	heatnote, _ := env.Vars["HeatNote"].(map[string]any)

	// mac 来源：优先 HeatNote["mac"]，为空则回退到 serial（12 位数字串，
	// 直接可用作 M-Bus 从站地址）。
	mac := _str(heatnote, "serial")
	if mac == "" {
		return fmt.Errorf("未配置mac或serial")
	}

	// 获取/复用 M-Bus 设备并连接（见 _ensureMBUS）
	dev, err := _ensureMBUS(ctx, env)
	if err != nil {
		return err
	}

	// 阶段 1：设置开度 step1Open → 弹框让用户确认电机开始转动
	if err := dev.SetValveOpenpreByMbus(ctx, mac, c.step1Open); err != nil {
		return fmt.Errorf("设置阶段1开度 %d 失败: %w", c.step1Open, err)
	}
	if err := askValveTurning(ctx, env, c.step1Open); err != nil {
		return err
	}

	// 阶段 1.5：弹框让用户等电机转完到目标位置后手动点确认，
	// 再恢复开度到 step2Open。机械到位要靠肉眼，开度回读不实时。
	if err := askValveDoneWait(ctx, env, c.step1Open, c.step2Open); err != nil {
		return err
	}

	// 阶段 1.6：读取 M-Bus 设备信息，确认告警位为 0 后再恢复开度。
	// 对齐 Perl dev_normal_check_motor 末尾的 alarm 校验：Alarm 非 0 说明
	// 电机状态异常（如堵转/过温），不应继续把开度恢复到 step2Open。
	info, err := dev.MbusReadInfo(ctx, mac)
	if err != nil {
		return fmt.Errorf("读取M-Bus设备信息失败: %w", err)
	}
	if info.Alarm != 0 {
		return fmt.Errorf("M-Bus读取到告警(Alarm=%d)，不继续恢复开度", info.Alarm)
	}
	env.Log.Info("", fmt.Sprintf("M-Bus 设备信息读取成功，无告警（Alarm=0），准备恢复开度到 %d%%", c.step2Open))

	// 阶段 2：恢复开度到 step2Open
	if err := dev.SetValveOpenpreByMbus(ctx, mac, c.step2Open); err != nil {
		return fmt.Errorf("恢复阶段2开度 %d 失败: %w", c.step2Open, err)
	}

	// 结束提示：电机仍在转，提醒用户等转完再断电，避免带电拔线烧驱动。
	env.Log.Warn("", fmt.Sprintf("已恢复开度到 %d%%，请等电机转完再断电", c.step2Open))
	return nil
}

func init() {
	Register("HeatSuite::wire_valve_mbus_read_motor", func() (core.Case, error) {
		return &WireValveMBusReadMotorCase{}, nil
	})
	Register("HeatSuite::wire_valve_mbus_upgrade_firmware", func() (core.Case, error) {
		return &WireValveMBusUpgradeFirmwareCase{}, nil
	})
}

// WireValveMBusUpgradeFirmwareCase 翻译自 Perl
// BLAT::APP::Heat::Cases::wire_valve::mbus_upgrade_firmware（L2097-2245）。
// 流程：
//  1. 解析升级文件路径（plan 参数"升级文件"优先，其次 HeatNote.mbus.firmware）；
//     文件不存在/未配置 → 告警跳过；
//  2. 解析固件文件名确定目标版本（未配置 plan 版本且文件名无法解析 → 报错，
//     不得以 SoftVer=0 误跳过）；
//  3. 读取设备当前版本（_MbusReadInfo）；
//  4. 先完成固件文件名版本/前缀校验（_validate_upgrade_firmware），已是目标
//     版本 → 跳过；
//  5. 按 128 字节分块，跳过中间全 0xFF padding 段，逐块 UpgradeByMbus 发送；
//     空文件/整文件全 0xFF → 报错；以最后实际发送块决定 IsEnd；
//  6. 发完 sleep 8 秒等设备写 flash（Perl `sleep 8`，L2244）。
//
// 目标软件版本：plan 参数"软件版本"（英文 soft_ver）优先；未配置时从固件
// 文件名 OTA 解析（parseOtaFile）。
type WireValveMBusUpgradeFirmwareCase struct {
	softVerTarget    int
	hasSoftVerTarget bool
	firmwarePath     string
	// sleep 默认 _sleep（ctx-aware，8s）；测试注入 no-op 避免真睡。
	sleep func(ctx context.Context, d time.Duration) error
}

func (c *WireValveMBusUpgradeFirmwareCase) Name() string {
	return "wire_valve_mbus_upgrade_firmware"
}

// Configure 读取 plan 的自定义参数：软件版本（中文键"软件版本"优先，兼容
// 英文 soft_ver）与升级文件路径（"升级文件"优先，兼容英文 firmware）。
// 软件版本必须是 0..255 的整数（拒绝负数/超 255/非整数 float），非法返回
// 明确错误（oracle 审查强化）。
func (c *WireValveMBusUpgradeFirmwareCase) Configure(args map[string]any) error {
	c.softVerTarget = 0
	c.hasSoftVerTarget = false
	c.firmwarePath = ""

	if v, ok := args["软件版本"]; ok {
		n, err := versionInt(v)
		if err != nil {
			return fmt.Errorf("软件版本参数无效: %w", err)
		}
		c.softVerTarget = n
		c.hasSoftVerTarget = true
	} else if v, ok := args["soft_ver"]; ok {
		n, err := versionInt(v)
		if err != nil {
			return fmt.Errorf("soft_ver 参数无效: %w", err)
		}
		c.softVerTarget = n
		c.hasSoftVerTarget = true
	}
	if v, ok := args["升级文件"].(string); ok && v != "" {
		c.firmwarePath = v
	} else if v, ok := args["firmware"].(string); ok {
		c.firmwarePath = v
	}
	return nil
}

// versionInt 把 plan 软件版本参数严格解析为 0..255 的整数（oracle 审查
// 强化）：接受 int/int64 与整数值 float64（YAML 常见解析结果），拒绝
// 非整数 float（如 5.7）、负数与超 255 的值（固件版本号是单字节）。
func versionInt(v any) (int, error) {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int64:
		n = x
	case float64:
		if x != math.Trunc(x) {
			return 0, fmt.Errorf("版本号必须是整数, 得到 %v", x)
		}
		n = int64(x)
	default:
		return 0, fmt.Errorf("版本号类型无法识别: %T", v)
	}
	if n < 0 || n > 255 {
		return 0, fmt.Errorf("版本号 %d 超出范围 0..255", n)
	}
	return int(n), nil
}

// matchFirmwarePath 判断固件快照路径与 Case 所选路径在 Windows 语义下是否
// 一致：filepath.Clean 归一化后 EqualFold 比较（不区分大小写）。空路径或
// 相对/绝对不一致视为不匹配。
func matchFirmwarePath(snapshotPath, selectedPath string) bool {
	if snapshotPath == "" || selectedPath == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(snapshotPath), filepath.Clean(selectedPath))
}

// sha256Hex 计算数据的 SHA-256 并返回小写 hex 字符串。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// maxUpgradeBlocks 是单次升级允许的最大实际发送块数：block_id 为 16 位
// LE（0..65535），故最多 65536 块。超限必须在发送前报错，避免部分写入后
// 因 BlockID 越界失败（oracle 复审）。
const maxUpgradeBlocks = 65536

// actualFirmwareBlocks 计算固件实际发送块数 = 总块数 - 被跳过的全 0xFF
// padding 段（[paddingStart, lastRealStart)）。纯计数函数，供发送前预检与
// 边界单测，无需构造大文件。
func actualFirmwareBlocks(totalBlocks, paddingStart, lastRealStart int) int {
	skipped := 0
	if paddingStart < lastRealStart {
		skipped = lastRealStart - paddingStart
	}
	return totalBlocks - skipped
}

// validateUpgradeBlockCount 预检实际发送块数：0 块（空文件/整文件全 0xFF）
// 与超 maxUpgradeBlocks 上限都返回错误，必须在发送任何块之前调用，防止
// 部分写入（oracle 复审）。
func validateUpgradeBlockCount(actual int) error {
	if actual <= 0 {
		return fmt.Errorf("固件文件无有效数据块（空文件或内容全为 0xFF）")
	}
	if actual > maxUpgradeBlocks {
		return fmt.Errorf("固件实际发送块数 %d 超出上限 %d，拒绝发送（防部分写入）", actual, maxUpgradeBlocks)
	}
	return nil
}

func (c *WireValveMBusUpgradeFirmwareCase) Run(ctx context.Context, env *core.Env) error {
	heatnote, _ := env.Vars["HeatNote"].(map[string]any)

	// 升级文件路径：plan 参数优先（Configure 已解析），否则从
	// HeatNote.mbus.firmware 读取（GUI 配置 lane 写入该键）。
	path := c.firmwarePath
	if path == "" {
		if m, ok := heatnote["mbus"].(map[string]any); ok {
			path = _str(m, "firmware")
		}
	}

	// 固件数据来源：若 env 上有固件快照且快照路径与所选路径匹配（Windows
	// 语义 filepath.Clean + EqualFold），使用快照 Data，不再 Stat/ReadFile，
	// 避免升级期间磁盘文件被替换/删除导致刷错固件；路径不匹配时保持磁盘
	// 行为，绝不误用快照。
	useSnapshot := path != "" && env.Firmware != nil && matchFirmwarePath(env.Firmware.Path, path)
	if !useSnapshot {
		// 文件必须存在且为普通文件（Perl -f $firmware_path，L2115）
		fi, statErr := os.Stat(path)
		if path == "" || statErr != nil || !fi.Mode().IsRegular() {
			env.Log.Warn("", "升级文件不存在或未配置,跳过升级")
			return nil
		}
	}

	// 解析固件文件名（前缀/版本），Perl parseOtaFile L2139。未配置目标
	// 版本且文件名无法解析 → 立即报错，不能以 SoftVer=0 误判"已是目标
	// 版本"跳过（oracle 审查）。
	ota := parseOtaFileName(path)
	target := c.softVerTarget
	if !c.hasSoftVerTarget {
		if !ota.OK {
			return fmt.Errorf("未配置目标软件版本且无法从固件文件名解析: %s", filepath.Base(path))
		}
		target = ota.SoftVer
	}

	mac := _str(heatnote, "serial")
	if mac == "" {
		return fmt.Errorf("未配置mac或serial")
	}

	dev, err := _ensureMBUS(ctx, env)
	if err != nil {
		return err
	}

	// 升级前读取设备当前信息（软/硬件版本），对应 Perl L2123-2127
	info, err := dev.MbusReadInfo(ctx, mac)
	if err != nil {
		return fmt.Errorf("读取设备当前信息失败: %w", err)
	}

	// 先完成固件文件名/目标版本校验（_validate_upgrade_firmware），再做
	// 当前版本跳过——校验失败不得被"已是目标版本"掩盖（oracle 审查顺序）。
	skip, verr := validateUpgradeFirmware(int(info.HardVer), target, ota)
	if verr != nil {
		return verr
	}
	if skip {
		env.Log.Warn("", fmt.Sprintf("未知设备硬件版本 HardVer=%d, 跳过升级", info.HardVer))
		return nil
	}

	// 已是目标版本 → 跳过（Perl L2134-2137）
	if int(info.SoftVer) == target {
		env.Log.Info("", fmt.Sprintf("设备已是目标版本 v%d, 跳过升级", target))
		return nil
	}

	// 固件数据来源：快照（校验 Size==len(Data) 且 SHA256 与 Data 实算一致，
	// 防快照损坏；不匹配返回错误，不 fallback 磁盘）或磁盘读取（实算
	// SHA-256）。两种来源都记录含 basename、字节数、完整 sha256 的日志，
	// 供报告经 case log 获得。
	var firmware []byte
	var firmwareSHA string
	source := "磁盘"
	if useSnapshot {
		snap := env.Firmware
		if int64(len(snap.Data)) != snap.Size {
			return fmt.Errorf("固件快照校验失败: Size=%d 与 Data 长度 %d 不一致", snap.Size, len(snap.Data))
		}
		hash := sha256Hex(snap.Data)
		if !strings.EqualFold(hash, snap.SHA256) {
			return fmt.Errorf("固件快照校验失败: SHA256 期望 %s, 实算 %s", snap.SHA256, hash)
		}
		firmware = snap.Data
		firmwareSHA = strings.ToLower(hash)
		source = "快照"
	} else {
		var err error
		firmware, err = os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("无法打开固件文件: %w", err)
		}
		firmwareSHA = sha256Hex(firmware)
	}
	env.Log.Info("", fmt.Sprintf("固件文件校验通过（%s）: %s (v%d, HardVer=%d %d bytes, sha256=%s)",
		source, filepath.Base(path), ota.SoftVer, int(info.HardVer), len(firmware), firmwareSHA))

	// MBUS 升级数据区最多 128 字节，每块发 128 字节（Perl L2159-2166）
	blockSize := 128
	total := len(firmware)
	totalBlocks := (total + blockSize - 1) / blockSize
	env.Log.Info("", fmt.Sprintf("升级开始，总大小: %d bytes，总块数: %d", total, totalBlocks))

	// 定位中间全 0xFF padding 段（Perl L2168-2207）
	paddingStart, lastRealStart := findFFPadding(firmware, totalBlocks, blockSize)
	if paddingStart < lastRealStart {
		env.Log.Info("", fmt.Sprintf("检测到中间 %d 块全 0xFF padding (块 %d..%d), 整体跳过",
			lastRealStart-paddingStart, paddingStart, lastRealStart-1))
	}

	// 实际发送块数 = 总块数 - 被跳过的 padding 段。发送前预检：0 块
	// （空文件/整文件全 0xFF）与超 65536 块（block_id 16 位上限）都直接
	// 报错，不能部分写入（oracle 复审）。
	actual := actualFirmwareBlocks(totalBlocks, paddingStart, lastRealStart)
	if err := validateUpgradeBlockCount(actual); err != nil {
		return err
	}

	// 最后实际发送块：若尾部存在全 FF padding（lastRealStart==totalBlocks，
	// 即文件末块被跳过），最后实发块是 paddingStart-1；否则是文件末块
	// totalBlocks-1。以"最后实际发送块"决定 IsEnd，保证设备一定能收到
	// is_end=1（oracle 审查：修复 Perl 按文件末块判 is_end、尾部 padding
	// 时最后实发块 is_end 恒 0、设备永远收不到结束标志的缺陷）。
	lastSentIdx := totalBlocks - 1
	if lastRealStart == totalBlocks {
		lastSentIdx = paddingStart - 1
	}

	blockID := 0
	sentBlocks := 0
	offset := 0
	for i := 0; i < totalBlocks; i++ {
		// 跳过中间全 FF 填充区段：跳过的块不发给设备，block_id 仍保持连续
		if i >= paddingStart && i < lastRealStart {
			env.Log.Info("", fmt.Sprintf("块 %d: 中间全 0xFF padding, 跳过", i))
			continue
		}
		start := i * blockSize
		end := start + blockSize
		if end > total {
			end = total
		}
		chunk := firmware[start:end]
		isLast := i == lastSentIdx
		isLastInt := 0
		if isLast {
			isLastInt = 1
		}

		err := dev.UpgradeByMbus(ctx, mac, mbus.UpgradeBlock{
			HardVer:   info.HardVer,
			SoftVer:   uint8(target),
			BlockID:   blockID,
			BlockSize: len(chunk),
			IsEnd:     isLast,
			Data:      chunk,
		})
		if err != nil {
			return fmt.Errorf("升级块 %d 发送失败: %w", blockID, err)
		}
		env.Log.Info("", fmt.Sprintf("块 %d/%d 完成 len=%d last=%d", blockID+1, totalBlocks, len(chunk), isLastInt))
		blockID++
		sentBlocks++
		offset += len(chunk)
	}

	env.Log.Info("", fmt.Sprintf("升级完成，共发送 %d 块，%d 字节", sentBlocks, offset))

	// 设备写 flash 完成后才响应，发完等 8s（Perl L2244 `sleep 8`）
	sleep := c.sleep
	if sleep == nil {
		sleep = _sleep
	}
	return sleep(ctx, 8*time.Second)
}

// blockAllFF 判断固件第 blockIdx 块（每块 blockSize 字节，末块可能不足）是否
// 全 0xFF。对应 Perl L2172-2186 / L2187-2201 里逐字节检查全 FF 的逻辑。
func blockAllFF(firmware []byte, blockIdx, blockSize int) bool {
	start := blockIdx * blockSize
	if start >= len(firmware) {
		return true
	}
	end := start + blockSize
	if end > len(firmware) {
		end = len(firmware)
	}
	for _, b := range firmware[start:end] {
		if b != 0xff {
			return false
		}
	}
	return true
}

// findFFPadding 定位固件中"中间连续全 0xFF 填充段"（对齐 Perl L2168-2207
// 两趟扫描）。返回 (paddingStart, lastRealStart)：发送时跳过
// [paddingStart, lastRealStart)；全文件全 0xFF 时返回 (0, totalBlocks)
// （全部跳过）。
//
// 固件结构: |真实数据|全 0xFF 填充|末尾非全 FF 校验值等|。
// 第一趟从末尾往前找最后一个"非全 FF"块（全 FF 块继续往前），得到
// lastReal；第二趟从 lastReal-1 往前把连续全 FF 段的起点记为 paddingStart。
func findFFPadding(firmware []byte, totalBlocks, blockSize int) (int, int) {
	// 第一趟（Perl L2172-2186）：全 FF 块 → break，lastReal 保持；否则收窄
	lastReal := totalBlocks
	for i := totalBlocks - 1; i >= 0; i-- {
		if blockAllFF(firmware, i, blockSize) {
			break
		}
		lastReal = i
	}
	// 第二趟（Perl L2187-2201）：从 lastReal-1 往前，非全 FF 块 → break；
	// 全 FF 块 → paddingStart 收窄到该块
	paddingStart := totalBlocks
	for i := lastReal - 1; i >= 0; i-- {
		if !blockAllFF(firmware, i, blockSize) {
			break
		}
		paddingStart = i
	}
	return paddingStart, lastReal
}

// askValveTurning 弹一个「是/否」确认框，让操作员确认电机已开始转动。
// 选「是」→ 返回 nil 继续；选「否」→ 返回 error（用例失败）；
// ctx 取消（Stop 按钮 / 关窗）→ 返回 ctx.Err()。回车默认「是」（参见
// internal/ui/fyne/app.go yesNoCh 处理，默认焦点在「是」上）。
// openPre 参数告知用户刚设置的目标开度值，文案携带便于判断。
func askValveTurning(ctx context.Context, env *core.Env, openPre int) error {
	msg := fmt.Sprintf("已通过 M-Bus 设置阀门开度到 %d%%，请观察电机是否已开始转动？选「否」将停止并失败", openPre)
	ok, err := env.UI.Confirm(ctx, msg, true)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("用户确认电机未转动")
	}
	env.Log.Info("", fmt.Sprintf("已确认电机开始转动（开度 %d%%）", openPre))
	return nil
}

// askValveDoneWait 弹一个「确定」按钮的提示框，让用户等电机转到目标位置
// 完成后手动点确认，确认后用例再读取 M-Bus 信息校验告警、恢复开度。
// 机械到位要肉眼判断，开度回读不是实时值。ctx 取消（Stop 按钮 / 关窗）
// → 返回 ctx.Err()。fromOpen/toOpen 仅用于文案告知用户当前→目标的开度值。
func askValveDoneWait(ctx context.Context, env *core.Env, fromOpen, toOpen int) error {
	msg := fmt.Sprintf("请等待电机转到 %d%% 目标位置后点击「确定」，确认后用例将继续把开度恢复到 %d%%",
		fromOpen, toOpen)
	if err := env.UI.Message(ctx, msg, true); err != nil {
		return err
	}
	env.Log.Info("", fmt.Sprintf("用户已确认电机转到 %d%%，准备恢复开度到 %d%%", fromOpen, toOpen))
	return nil
}

// _ensureMBUS 获取/创建 M-Bus 设备并确保已连接（对应 Perl
// _ensure_mbus_connected）。优先复用 Vars.HeatNote["mbus_dev"] 已持久化的
// 连接（mock 或 real 由 mbus_mock 标志决定构造）；无则创建、连接后写回
// HeatNote 供后续 case 复用。串口从 HeatNote["mbus"].(map[string]any) 的
// 小写键 port 读取（如 "COM9"）。
func _ensureMBUS(ctx context.Context, env *core.Env) (*mbus.Device, error) {
	// 注意：不能无条件 fallback 到 env.Devs["mbus"]——main 默认注入
	// NewDevice()（real），无条件复用会让 -mock-mbus=true 拿不到 mock 实例。
	// 仅当 Devs 实例的模式与 mbus_mock 请求的模式一致时才兜底复用。
	heatnote, _ := env.Vars["HeatNote"].(map[string]any)
	dev, ok := heatnote["mbus_dev"].(*mbus.Device)
	if !ok {
		mock, _ := heatnote["mbus_mock"].(bool)
		if d, has := env.Devs["mbus"].(*mbus.Device); has && !d.IsReal() == mock {
			dev = d
		} else if mock {
			dev = mbus.NewMockDevice()
		} else {
			dev = mbus.NewRealDevice()
		}
		// 存回 vars 供后续 case 复用
		if heatnote == nil {
			heatnote = map[string]any{}
			env.Vars["HeatNote"] = heatnote
		}
		heatnote["mbus_dev"] = dev
	}

	// 串口配置：HeatNote["mbus"] 子 map 的小写键 port
	var port string
	if m, ok := heatnote["mbus"].(map[string]any); ok {
		port = _str(m, "port")

	}
	if port == "" {
		return nil, fmt.Errorf("未配置MBUS串口")
	}

	// 注入日志输出并连接（env.Log 是 core.Logger(Info(string))，mbus 契约
	// 要求 Info(args ...any)，用 adapter 适配，见 mbusLogAdapter）
	dev.SetLogger(mbusLogAdapter{log: env.Log})
	// --debug 模式：打印 MBus 发送/接收 hex（默认 false 与 Perl 行为一致）
	if debug, _ := heatnote["debug"].(bool); debug {
		dev.SetDebug(true)
	}
	if err := dev.Connect(ctx, port); err != nil {
		return nil, err
	}
	return dev, nil
}

// mbusLogAdapter 把 core.Logger 适配成 mbus.Logger。mbus 包契约的 Logger
// 要求 Info(args ...any)（core.Logger 提供 Info(category, msg)），故在调用侧
// 包装转发，避免改动 mbus 包。
type mbusLogAdapter struct {
	log core.Logger
}

func (a mbusLogAdapter) Info(args ...any) {
	if len(args) == 1 {
		if s, ok := args[0].(string); ok {
			a.log.Info("", s)
			return
		}
	}
	a.log.Info("", fmt.Sprint(args...))
}
