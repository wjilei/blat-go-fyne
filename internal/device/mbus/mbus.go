// Package mbus provides a business-level M-Bus device for the Heat
// suite, ported from the Perl BLAT::Device::UserValve + serial.pm
// M-Bus path. It mirrors the internal/device/bluetooth package style:
// methods hang directly off *Device instead of going through the low
// level core.Device Command protocol.
//
// It intentionally does NOT implement core.Device (Open/Close/Command):
// it lives above the low level driver layer and is injected into
// env.Devs["mbus"] by the application (cmd/blat/main.go), then
// type-asserted by the Case that owns it (wire_valve_mbus_read_motor).
//
// Two operating modes:
//
//   - mock（NewMockDevice）：内存假数据，无硬件可跑，默认返回状态 "01"。
//   - real（NewRealDevice）：Windows 串口（internal/serial.OpenPort，
//     2400 8E1）通过 M-Bus 协议读取电机状态。与 bluetooth 不同，mbus
//     不需要 tinygo 专用串行 executor goroutine——串口读写天然串行。
//
// 协议细节对齐 Perl 原版，关键处注释标注"对应 Perl BLAT xxx.pm Lxxx"。
package mbus

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"blat/internal/serial"
)

// errNotConnected 是设备未连接（Connect 未调用或失败）时返回的错误。
var errNotConnected = errors.New("mbus 设备未连接")

// errMBusWrite 表示请求帧写入串口失败（与"无响应超时"区分）。供
// UpgradeByMbus 的 dummy 唤醒帧判定：真实 Write 错误必须报错，而设备无
// 响应（超时）按 Perl 语义可以继续。
var errMBusWrite = errors.New("mbus: 写入失败")

// errMBusShortWrite 表示 Write 返回 n < len(frame) 且 err==nil（短写）：
// 串口只接收了半帧。同一 transaction 直接重试会把残帧追加到下一帧上，
// 必须立即返回不可重试错误（oracle 复审）。普通明确 Write error 仍走
// errMBusWrite 的 retry 语义。
var errMBusShortWrite = errors.New("mbus: 短写 (short write)")

// Logger 是 mbus 包可选的日志输出接口。契约要求 Info(args ...any)
// （调用侧 cmd/blat/cases 的 mbusLogAdapter 按此签名适配 core.Logger）。
// nil 时静默跳过日志。
type Logger interface {
	Info(args ...any)
}

// Device is a business-level M-Bus device.
//
// mock==true 时走内存 mock；mock==false 时走真实 Windows 串口 + M-Bus
// 协议（internal/serial 提供非阻塞串口读写）。
type Device struct {
	mu         sync.Mutex
	mock       bool
	connected  bool // mock 模式的连接标志（real 模式以 port != nil 为准）
	port       serial.Port
	portName   string
	mockStatus string   // mock 电机状态，默认 "01"
	mockInfo   MbusInfo // mock MbusReadInfo 返回值，默认 Alarm=0
	// mockOpenPreSeq + mockOpenPreIdx：MbusReadInfo 每次读消费一个
	// OpenPre，未配序列时回退到 mockInfo.OpenPre。仅 mock 模式生效，
	// 用于单测两阶段开度检查 happy path（阶段 1 读 80 → 阶段 2 读 100）。
	mockOpenPreSeq []uint8
	mockOpenPreIdx int
	// mockUpgradeBlocks + mockUpgradeErr：UpgradeByMbus mock 路径的升级块
	// 记录与注入错误（对应 bluetooth 包 mock 模式的双状态设计）。
	mockUpgradeBlocks []UpgradeBlock
	mockUpgradeErr    error
	logger            Logger
	debug             bool // true 时 MBusReadMotor 打印发送/接收 hex（--debug 模式）
}

// UpgradeBlock is one MBus firmware-upgrade data block（对应 Perl
// UpgradeByMbus L680-692 的 %args + $mbus_dat）。Data 为原始字节
// （Perl 侧为 hex 字符串，Go 侧在调用前已解码）；BlockID/BlockSize 为
// 十进制序号/字节数，IsEnd 标记最后一块。
type UpgradeBlock struct {
	HardVer   uint8
	SoftVer   uint8
	BlockID   int
	BlockSize int
	IsEnd     bool
	Data      []byte
}

// MbusInfo 是 _MbusReadInfo（Perl UserValve.pm L556-599）一次读全的设备
// 信息。温度字段为有符号（Perl trans_hex_negative：高位为 1 视为负值），
// 其它数值字段为无符号。mock 模式下默认 Alarm=0，让 dev_normal_check_motor
// 末尾的"状态保存"检查通过。
type MbusInfo struct {
	TempOutput int32  // 出水温度（0.01℃/单位，可负）
	TempInput  int32  // 进水温度（0.01℃/单位，可负）
	OpenPre    uint8  // 当前开度（%）
	HardVer    uint8  // 硬件版本
	SoftVer    uint8  // 软件版本
	Alarm      uint8  // 告警位，0 表示无告警
	FlowRate   uint32 // 瞬时流量
	CumHot     uint32 // 累计热量
}

// NewDevice returns a real M-Bus device（等价 NewRealDevice），保持向后兼容
// （main 现有两处调用不变）。默认走真实串口；无硬件调试时用 NewMockDevice。
func NewDevice() *Device {
	return NewRealDevice()
}

// NewMockDevice 返回 mock 模式设备：MBusReadMotor 直接返回 mockStatus，
// 无需硬件。
func NewMockDevice() *Device {
	return &Device{
		mock:       true,
		mockStatus: "01",
	}
}

// NewRealDevice 返回真实 Windows 串口模式设备。
func NewRealDevice() *Device {
	return &Device{mock: false}
}

// IsReal reports whether the device runs in real mode (mock==false)。
// 供 case 端判断 env.Devs 中的实例是否为真实设备。
func (d *Device) IsReal() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.mock
}

// SetMockStatus 注入下一次 MBusReadMotor 返回的电机状态 hex 串（如 "09"）。
func (d *Device) SetMockStatus(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mockStatus = s
}

// SetMockInfo 注入下一次 MbusReadInfo 返回的设备信息（mock 模式专用）。
// 未调用时返回默认 MbusInfo（Alarm=0，其它字段为 0）。
func (d *Device) SetMockInfo(info MbusInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mockInfo = info
}

// SetMockOpenPreSequence 注入 MbusReadInfo mock 路径的开度序列。每次
// 读取依次返回序列中的下一个 OpenPre（其它字段沿用 mockInfo）；序列
// 末尾保持最后一个值。仅 mock 模式生效，real 模式下调用无效果。空
// 序列或不调用本方法时，MbusReadInfo 直接返回 mockInfo.OpenPre。
func (d *Device) SetMockOpenPreSequence(pres ...uint8) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mockOpenPreSeq = append([]uint8(nil), pres...)
	d.mockOpenPreIdx = 0
}

// SetMockUpgradeError 注入 UpgradeByMbus mock 路径的返回错误；传 nil 清除。
// 非 nil 时 mock 升级调用记录 block 后返回该错误（模拟固件写入失败）。
func (d *Device) SetMockUpgradeError(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mockUpgradeErr = err
}

// MockUpgradeBlocks 返回 mock 模式下 UpgradeByMbus 记录的全部升级块
// （深拷贝，调用方可安全持有）。未调用过返回 nil。
func (d *Device) MockUpgradeBlocks() []UpgradeBlock {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]UpgradeBlock, len(d.mockUpgradeBlocks))
	for i, b := range d.mockUpgradeBlocks {
		out[i] = b
		out[i].Data = append([]byte(nil), b.Data...)
	}
	return out
}

// SetLogger 注入日志输出（通常传 env.Log 适配器）。nil 时静默跳过。
func (d *Device) SetLogger(l Logger) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logger = l
}

// SetDebug 设置 --debug 调试模式：true 时 MBusReadMotor 会把发送/接收的
// M-Bus 帧以 hex 形式打印到 logger，便于排查真实串口收发内容；false 时
// 静默（与 Perl 原版一致）。
func (d *Device) SetDebug(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.debug = on
}

// logInfo 输出一条日志；logger 为 nil 时静默跳过。
func (d *Device) logInfo(args ...any) {
	d.mu.Lock()
	l := d.logger
	d.mu.Unlock()
	if l != nil {
		l.Info(args...)
	}
}

// Connect 打开串口 portName（real：2400 8E1）并保存句柄；mock：仅记录
// 连接标志。已连接幂等返回 nil。
func (d *Device) Connect(ctx context.Context, portName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.connected || d.port != nil {
		return nil
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if d.mock {
		d.connected = true
		d.portName = portName
		return nil
	}
	p, err := serial.OpenPort(portName, 2400, "even")
	if err != nil {
		return fmt.Errorf("mbus: 打开串口 %s 失败: %w", portName, err)
	}
	d.port = p
	d.portName = portName
	return nil
}

// Disconnect 关闭串口并清空连接状态，幂等（已断开时返回 nil）。
func (d *Device) Disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.port != nil {
		if err := d.port.Close(); err != nil {
			return err
		}
		d.port = nil
	}
	d.connected = false
	d.portName = ""
	return nil
}

// MBusReadMotor 读取电机状态，返回状态 hex 字符串（如 "01"/"09"）。
// 失败返回 error（用例侧视为"未就绪"继续轮询，失败是正常路径）。
//
// 对齐 Perl BLAT UserValve.pm MBusReadMotor（L400-415）：
//   - mock：未连接（Connect 未调用或失败）返回错误；否则返回 mockStatus。
//   - real：构造 BB1D 请求帧 → commandTrans（timeout=1s, retry=5）→
//     按 `^\w{34}(\w{2})\w{2}16$` 解析电机状态（L409），命中日志输出
//     电机状态（对应 Perl $log->debugf，L411）。
func (d *Device) MBusReadMotor(ctx context.Context, mac string) (string, error) {
	d.mu.Lock()
	mock := d.mock
	connected := d.connected
	port := d.port
	d.mu.Unlock()

	if mock {
		if !connected {
			return "", errNotConnected
		}
		d.mu.Lock()
		st := d.mockStatus
		d.mu.Unlock()
		return st, nil
	}
	if port == nil {
		return "", errNotConnected
	}

	frame, err := buildReadMotorFrame(mac)
	if err != nil {
		return "", err
	}
	ret, err := d.commandTrans(ctx, port, frame, time.Millisecond*time.Duration(500), 2)
	if err != nil {
		return "", err
	}
	stat, err := parseMotorResponse(ret)
	if err != nil {
		return "", err
	}
	d.logInfo(fmt.Sprintf("电机状态：%s", stat))
	return stat, nil
}

// CaliValveByMbus 通过 M-Bus 发起阀门电机重新校准（对应 Perl
// UserValve.pm L632-638 CaliValveByMbus：open_pre=0, calc_day=255 触发
// 重新校准）。用例侧用于 dev_normal_check_motor 启动电机前复位。
//
// 协议：cmd_id=BB1F（SET_VALVE），calc_day=255 是特殊值，固件视为重新校准。
// 真实路径委托 setValveByMbus(0, 0xff)；mock 路径直接返回 nil（与既有行为
// 一致）。详细协议说明见 setValveByMbus。
func (d *Device) CaliValveByMbus(ctx context.Context, mac string) error {
	d.mu.Lock()
	mock := d.mock
	connected := d.connected
	d.mu.Unlock()
	if mock {
		if !connected {
			return errNotConnected
		}
		return nil
	}
	return d.setValveByMbus(ctx, mac, 0, 0xff)
}

// SetValveOpenpreByMbus 通过 M-Bus 设置阀门开度（百分比），对应 Perl
// UserValve.pm L624-630 SetValveOpenpreByMbus → _SetValveByMbus(addr,
// open_pre, 30)：cmd_id=BB1F、calc_day=30（不重新校准，不重启设备）。
//
// openPre 必须在 [0,100] 范围内（含边界），否则返回错误且不发任何通信。
// mock 模式：仅做连接检查 + ctx 取消短路；real 模式委托 setValveByMbus。
func (d *Device) SetValveOpenpreByMbus(ctx context.Context, mac string, openPre int) error {
	if openPre < 0 || openPre > 100 {
		return fmt.Errorf("开度值必须在 0-100 之间: %d", openPre)
	}
	d.mu.Lock()
	mock := d.mock
	connected := d.connected
	d.mu.Unlock()
	if mock {
		if !connected {
			return errNotConnected
		}
		if err := ctxErr(ctx); err != nil {
			return err
		}
		return nil
	}
	return d.setValveByMbus(ctx, mac, byte(openPre), 30)
}

// setValveByMbus 是 SET_VALVE（BB1F）的真实协议入口，对应 Perl UserValve.pm
// L601-621 _SetValveByMbus：构造 21 字节请求帧 → commandTrans (timeout=3s,
// retry=5，对齐 Perl L614) → 校验 19 字节响应格式（`^\w{34}\w{2}16$` 或
// 短帧 0xE5 ACK）。CaliValveByMbus/SetValveOpenpreByMbus/RebootByMbus 等
// 均通过本方法走真实路径，差异仅在 open_pre/calc_day 两个字节。
func (d *Device) setValveByMbus(ctx context.Context, mac string, openPre, calcDay byte) error {
	d.mu.Lock()
	port := d.port
	d.mu.Unlock()
	if port == nil {
		return errNotConnected
	}
	frame, err := buildSetValveFrame(mac, openPre, calcDay)
	if err != nil {
		return err
	}
	ret, err := d.commandTrans(ctx, port, frame, 3*time.Second, 5)
	if err != nil {
		return err
	}
	if err := parseSetValveResponse(ret); err != nil {
		return err
	}
	d.logInfo(fmt.Sprintf("setValveByMbus 成功: %s open_pre=%d calc_day=%d", mac, openPre, calcDay))
	return nil
}

// MbusReadInfo 通过 M-Bus 一次性读取设备全部信息（cmd_id=BB1E，对应 Perl
// UserValve.pm L556-599 _MbusReadInfo）。用例侧用于 dev_normal_check_motor
// 末尾读取 alarm 校验状态保存。
//
// 返回的 MbusInfo 温度字段为有符号 int32（Perl trans_hex_negative：高位为
// 1 视为负值；如 -2500 补码 0xFFFFF63C LE 编码为 3C F6 FF FF）。
//
// mock 模式直接返回 mockInfo（默认 Alarm=0，让用例末尾校验通过）；
// real 模式构造 19 字节请求帧 → commandTrans → 校验 39 字节响应格式
// （`^\w{34}(\w{8})(\w{8})(\w{2})(\w{2})(\w{2})(\w{2})(\w{8})(\w{8})\w{2}16$`，
// 对应 Perl _MbusReadInfo L569）。
func (d *Device) MbusReadInfo(ctx context.Context, mac string) (MbusInfo, error) {
	var zero MbusInfo
	d.mu.Lock()
	mock := d.mock
	connected := d.connected
	port := d.port
	d.mu.Unlock()

	if mock {
		if !connected {
			return zero, errNotConnected
		}
		d.mu.Lock()
		info := d.mockInfo
		if len(d.mockOpenPreSeq) > 0 {
			info.OpenPre = d.mockOpenPreSeq[d.mockOpenPreIdx]
			if d.mockOpenPreIdx < len(d.mockOpenPreSeq)-1 {
				d.mockOpenPreIdx++
			}
		}
		d.mu.Unlock()
		return info, nil
	}
	if port == nil {
		return zero, errNotConnected
	}

	frame, err := buildReadInfoFrame(mac)
	if err != nil {
		return zero, err
	}
	// Perl _MbusReadInfo L568: timeout=>1, retry=>5
	ret, err := d.commandTrans(ctx, port, frame, time.Second, 5)
	if err != nil {
		return zero, err
	}
	info, err := parseInfoResponse(ret)
	if err != nil {
		return zero, err
	}
	d.logInfo(fmt.Sprintf("MbusReadInfo: %+v", info))
	return info, nil
}

// UpgradeByMbus 通过 M-Bus 发送固件升级数据块（cmd_id=BB20），对应 Perl
// UserValve.pm UpgradeByMbus L680-717 + dev_mbus_fill_set_cmd BB20 分支
// L278-300。设备写 flash 完成后才响应（实测 4.4~7.5s 且不稳定），因此
// 升级帧 commandTrans 用 timeout=2s, retry=5（Perl L699）。
//
// mock 模式：未连接返回 errNotConnected；ctx 取消短路；把 blk 记录到
// mockUpgradeBlocks；若注入过 mockUpgradeErr 则返回之，否则返回 nil。
// real 模式：先发 address dummy 唤醒帧（Perl L695-696，
// command_trans timeout=>0.05 retry=>1，结果丢弃；但 ctx 取消必须透传，
// 真实 Write 错误必须报错），再构造 155 字节 BB20 升级帧发送。响应兼容
// 新旧格式：命中新固件格式 `^\w{34}(\w{2})(\w{4})\w{2}16$` 时解析
// dev_ret / next_block_id 并校验（devRet!=0 或 nextBlockID!=BlockID+1
// 视为设备拒绝，返回错误；Perl 原版只打日志不检查，oracle 审查强化）；
// 旧格式（不匹配正则）保持"不校验应答"，只打成功行。
func (d *Device) UpgradeByMbus(ctx context.Context, mac string, blk UpgradeBlock) error {
	// 参数校验先行（BlockID/Data/BlockSize），非法直接返回，不发任何字节
	if err := validateUpgradeBlock(blk); err != nil {
		return err
	}
	d.mu.Lock()
	mock := d.mock
	connected := d.connected
	port := d.port
	d.mu.Unlock()

	if mock {
		if !connected {
			return errNotConnected
		}
		if err := ctxErr(ctx); err != nil {
			return err
		}
		d.mu.Lock()
		d.mockUpgradeBlocks = append(d.mockUpgradeBlocks, blk)
		e := d.mockUpgradeErr
		d.mu.Unlock()
		return e
	}
	if port == nil {
		return errNotConnected
	}

	// address dummy 唤醒帧（dev_mubs_addr_cmd_dummy，UserValve.pm L200-217）：
	// Perl 语义是结果丢弃、设备无响应可继续（command_trans timeout=>0.05
	// retry=>1）；但真实 Write 错误（全轮次失败 errMBusWrite）与短写
	//（errMBusShortWrite，半帧）必须可观测并失败（oracle 复审：不能把串口
	// 故障当"无响应"吞掉），ctx 取消也必须透传。
	_, dummyErr := d.commandTrans(ctx, port, buildUpgradeDummyFrame(), 50*time.Millisecond, 1)
	if errors.Is(dummyErr, errMBusWrite) || errors.Is(dummyErr, errMBusShortWrite) {
		return fmt.Errorf("升级唤醒帧发送失败: %w", dummyErr)
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}

	frame, err := buildUpgradeFrame(mac, blk)
	if err != nil {
		return err
	}
	// Perl L699: timeout=>2, retry=>5
	ret, err := d.commandTrans(ctx, port, frame, 1*time.Second, 5)
	if err != nil {
		return err
	}
	// 新固件响应：data = [ret(1B) | next_block_id(2B LE)]，长度 0x06。
	// devRet 非 0（设备拒绝）或 nextBlockID != BlockID+1 视为发送失败；
	// 旧固件响应不含 ret 字段，格式不匹配不打解析日志，只打成功行
	//（Perl "不校验应答"，非匹配不视为错误，L705-716）。
	if devRet, nextBlockID, ok := parseUpgradeResponse(ret); ok {
		if devRet != 0 {
			return fmt.Errorf("升级块 %d 设备返回错误 ret=0x%02x", blk.BlockID, devRet)
		}
		if int(nextBlockID) != blk.BlockID+1 {
			return fmt.Errorf("升级块 %d 设备返回 next_block_id=%d 不符合预期 (%d)", blk.BlockID, nextBlockID, blk.BlockID+1)
		}
		d.logInfo(fmt.Sprintf("升级块 %d 设备返回 ret=0x%02x next_block_id=%d", blk.BlockID, devRet, nextBlockID))
	} else {
		d.logInfo(fmt.Sprintf("升级块 %d 发送成功", blk.BlockID))
	}
	return nil
}

// validateUpgradeBlock 校验升级块参数（oracle 审查强化）：BlockID 必须
// 0..65535、Data 长度必须 1..128、BlockSize 必须 == len(Data) 且 1..128。
// 非法返回 error，不静默截断。
func validateUpgradeBlock(blk UpgradeBlock) error {
	if blk.BlockID < 0 || blk.BlockID > 0xFFFF {
		return fmt.Errorf("mbus: 升级块 BlockID=%d 超出范围 0..65535", blk.BlockID)
	}
	if len(blk.Data) < 1 || len(blk.Data) > 128 {
		return fmt.Errorf("mbus: 升级块数据长度 %d 超出范围 1..128", len(blk.Data))
	}
	if blk.BlockSize < 1 || blk.BlockSize > 128 || blk.BlockSize != len(blk.Data) {
		return fmt.Errorf("mbus: BlockSize=%d 必须等于数据长度 %d 且在 1..128 内", blk.BlockSize, len(blk.Data))
	}
	return nil
}

// ---- 请求帧构造（对应 Perl BLAT UserValve.pm dev_mbus_fill_set_cmd）----

// buildReadMotorFrame 构造 BB1D（读电机状态）请求帧，返回 19 字节。
//
// BB1D 走 READ_INFO 分支（UserValve.pm L220-222 + dev_get_mbus_data
// L141-146）：
//
//	索引: 0-2 前导 FE FE FE | 3: 68(长帧起始) | 4: 0x20(恒为32,构造后从不重算)
//	| 5-10: 设备ID | 11: 00 | 12: 01(C) | 13: 03(data_len)
//	| 14-15: cmd_id 大端 BB 1D | 16: 01 | 17: CS | 18: 16(结束符)
//
// cmd_id 先以 READ_INFO 模板（BB 1E）展开，再覆盖为大端 BB 1D
// （UserValve.pm L239-240）；data_len 恒 3（模板 19 字节 - 16）。
//
// CS = sum(字节 3..16) & 0xFF（cmd_len = data_len+14 = 17，即 sum(索引
// 3..cmd_len-1)，UserValve.pm L246-252），放字节 17。
func buildReadMotorFrame(mac string) ([]byte, error) {
	// READ_INFO 模板 19 字节（dev_get_mbus_data，UserValve.pm L141-146），
	// 注意 Perl 里 $mbus_data[12]='01' 是字符串，Go 显式 byte(0x01)。
	frame := []byte{
		0xfe, 0xfe, 0xfe, 0x68, 0x20,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 5-10 设备 ID（占位）
		0x00,       // 11
		0x01,       // 12 C
		0x03,       // 13 data_len
		0xbb, 0x1e, // 14-15 cmd_id（模板 BB1E，随后覆盖）
		0x01, // 16
		0x00, // 17 CS（随后重算）
		0x16, // 18 结束符
	}
	id, err := parseMbusID(mac)
	if err != nil {
		return nil, err
	}
	// 填充设备 ID（UserValve.pm L243-245）：12 位→5..10，10 位→5..9（末字节
	// 保持 0），14 位→5..11（覆盖索引 11 的 0x00）。
	for i, v := range id {
		frame[5+i] = v
	}
	// cmd_id 覆盖为 BB1D（UserValve.pm L239-240）
	frame[14] = 0xbb
	frame[15] = 0x1d
	// 校验和（UserValve.pm L246-252）
	var cs byte
	for i := 3; i <= 16; i++ {
		cs += frame[i]
	}
	frame[17] = cs
	return frame, nil
}

// parseMbusID 把 mac 解析为设备 ID 字节序列（对应 Perl UserValve.pm
// L188-215 的正则分支）：
//
//	12 位: mbus_id[0]=hex(第11-12位) ...[5]=hex(第1-2位)（倒序填 6 字节）
//	14 位: mbus_id[0]=hex(第13-14位) ...[6]=hex(第1-2位)（倒序填 7 字节，
//	      覆盖模板索引 11）
//	10 位: mbus_id[0]=hex(第9-10位) ...[4]=hex(第1-2位)，[5]=0（末字节补 0）
//
// 其它长度或含非数字 → 返回错误（对应 Perl die "$mac 不可用于MBUS"）。
// 说明：Go 侧按精确长度 10/12/14 判断（Perl 用无锚正则，13 位会误走 12
// 位分支截断，属原版隐患，此处按任务契约严格化）。
func parseMbusID(mac string) ([]byte, error) {
	for _, c := range mac {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("%s 不可用于MBUS", mac)
		}
	}
	var id []byte
	switch len(mac) {
	case 10:
		id = make([]byte, 6)
		id[5] = 0
		for i := 0; i < 5; i++ {
			id[i] = hexByte(mac, 2*(4-i))
		}
	case 12:
		id = make([]byte, 6)
		for i := 0; i < 6; i++ {
			id[i] = hexByte(mac, 2*(5-i))
		}
	case 14:
		id = make([]byte, 7)
		for i := 0; i < 7; i++ {
			id[i] = hexByte(mac, 2*(6-i))
		}
	default:
		return nil, fmt.Errorf("%s 不可用于MBUS", mac)
	}
	return id, nil
}

// hexByte 解析 s[off:off+2] 为字节（输入已保证是数字）。
func hexByte(s string, off int) byte {
	v, err := strconv.ParseUint(s[off:off+2], 16, 8)
	if err != nil {
		return 0
	}
	return byte(v)
}

// ---- 响应解析（对应 Perl BLAT UserValve.pm MBusReadMotor L409）----

// parseMotorResponse 从 matcher 返回的完整响应帧提取电机状态：
// `^\w{34}(\w{2})\w{2}16$`（状态位于 hex 位置 34-35）。格式不匹配返回错误。
func parseMotorResponse(ret string) (string, error) {
	re := regexp.MustCompile(`^\w{34}(\w{2})\w{2}16$`)
	m := re.FindStringSubmatch(ret)
	if m == nil {
		return "", fmt.Errorf("mbus: 电机状态响应格式异常: %s", ret)
	}
	return m[1], nil
}

// parseSetValveResponse 校验 SET_VALVE（BB1F）响应帧格式，对应 Perl
// UserValve.pm L617-621 `^\w{34}\w{2}16$`：17 字节 header + 1 字节 CS +
// 1 字节 0x16 结束符。CS 由 makeMBusMatcher 已校验过，这里只查总长度与
// 16 结束符。
//
// 兼容 M-Bus 规范的短帧 ACK "E5"（SND_UD 类 SET 命令 slave 可能回 0xE5
// 而非 19 字节长帧），同样视为成功。匹配返回 nil，否则返回错误。
func parseSetValveResponse(ret string) error {
	if ret == "E5" {
		return nil
	}
	re := regexp.MustCompile(`^\w{34}\w{2}16$`)
	if !re.MatchString(ret) {
		return fmt.Errorf("mbus: SET_VALVE 响应格式异常: %s", ret)
	}
	return nil
}

// parseInfoResponse 解析 READ_INFO（BB1E）响应帧（39 字节），提取
// temp_output/temp_input/open_pre/hard_ver/soft_ver/alarm/flow_rate/
// cum_hot 八字段。对应 Perl UserValve.pm L569-598。
//
// 字节布局（17 字节 header + 20 字节数据 + 1 字节 CS + 1 字节 0x16）：
//   - data 段: temp_output(4B LE) temp_input(4B LE) open_pre(1)
//     hard_ver(1) soft_ver(1) alarm(1) flow_rate(4B LE) cum_hot(4B LE)
//
// 温度字段是有符号（Perl L574-575 trans_hex_negative：高位为 1 视为负值）；
// 流量/累计热量按无符号读取。
func parseInfoResponse(ret string) (MbusInfo, error) {
	re := regexp.MustCompile(`^\w{34}(\w{8})(\w{8})(\w{2})(\w{2})(\w{2})(\w{2})(\w{8})(\w{8})\w{2}16$`)
	m := re.FindStringSubmatch(ret)
	if m == nil {
		return MbusInfo{}, fmt.Errorf("mbus: READ_INFO 响应格式异常: %s", ret)
	}
	// Perl L572-573: 先 high_switch_low（字节序交换），再 trans_hex_negative。
	// 字节在 wire 上按 LE 排，Go 直接 LittleEndian.Uint32 读出与 high_switch_low
	// 等价；负数再转 int32（与 trans_hex_negative 等价）。
	le32 := func(s string) uint32 {
		b, _ := hex.DecodeString(s)
		return binary.LittleEndian.Uint32(b)
	}
	le8 := func(s string) uint8 {
		v, _ := strconv.ParseUint(s, 16, 8)
		return uint8(v)
	}
	return MbusInfo{
		TempOutput: int32(le32(m[1])),
		TempInput:  int32(le32(m[2])),
		OpenPre:    le8(m[3]),
		HardVer:    le8(m[4]),
		SoftVer:    le8(m[5]),
		Alarm:      le8(m[6]),
		FlowRate:   le32(m[7]),
		CumHot:     le32(m[8]),
	}, nil
}

// parseUpgradeResponse 解析升级响应帧（新固件格式），对应 Perl UpgradeByMbus
// L707-712：`^\w{34}(\w{2})(\w{4})\w{2}16$`。返回设备返回值 devRet 与期望的
// 下一块序号 nextBlockID（little-endian uint16）。格式不匹配返回 ok=false
// （Perl "不校验应答"，非匹配只影响日志，不视为错误）。
func parseUpgradeResponse(ret string) (devRet uint8, nextBlockID uint16, ok bool) {
	re := regexp.MustCompile(`^\w{34}(\w{2})(\w{4})\w{2}16$`)
	m := re.FindStringSubmatch(ret)
	if m == nil {
		return 0, 0, false
	}
	v, err := strconv.ParseUint(m[1], 16, 8)
	if err != nil {
		return 0, 0, false
	}
	b, err := hex.DecodeString(m[2])
	if err != nil {
		return 0, 0, false
	}
	return uint8(v), binary.LittleEndian.Uint16(b), true
}

// ---- 请求帧构造（SET_VALVE BB1F，对应 Perl UserValve.pm L156-160
// SET_VALVE 模板 + L273-277 数据填充）----

// buildSetValveFrame 构造 SET_VALVE（BB1F）请求帧，返回 21 字节。
//
// 帧布局（UserValve.pm L156-160 模板 + L273-277 数据 + L305-322 头/CS）：
//
//	索引: 0-2 前导 FE FE FE | 3: 68 | 4: 0x20(L, 恒为 32 构造后不重算)
//	| 5-10: 设备ID | 11: 00 | 12: 01(C) | 13: 05(data_len)
//	| 14-15: cmd_id BB 1F | 16: 01(sub_id)
//	| 17: open_pre | 18: calc_day
//	| 19: CS | 20: 0x16
//
// data_len = 5 = cmd_id(2) + sub_id(1) + open_pre(1) + calc_day(1)。
// CS = sum(字节 3..18) & 0xFF（cmd_len = data_len+14 = 19），放字节 19。
// 模板初始 cmd_id 占位 0xbb 0x1c 在 L264-265 会被覆盖为 BB1F 模板
// 占位 0xbb 0x1c，这里直接写 0xbb 0x1f 即可。
func buildSetValveFrame(mac string, openPre, calcDay byte) ([]byte, error) {
	frame := []byte{
		0xfe, 0xfe, 0xfe, 0x68, 0x20,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 5-10 设备 ID（占位）
		0x00,       // 11
		0x01,       // 12 C
		0x05,       // 13 data_len（cmd_id 2 + sub_id 1 + open_pre 1 + calc_day 1）
		0xbb, 0x1f, // 14-15 cmd_id
		0x01, // 16 sub_id
		0x00, // 17 open_pre（占位）
		0x00, // 18 calc_day（占位）
		0x00, // 19 CS（随后重算）
		0x16, // 20 结束符
	}
	id, err := parseMbusID(mac)
	if err != nil {
		return nil, err
	}
	for i, v := range id {
		frame[5+i] = v
	}
	frame[17] = openPre
	frame[18] = calcDay
	// CS = sum(3..18)（cmd_len = data_len+14 = 19，UserValve.pm L317-322）
	var cs byte
	for i := 3; i <= 18; i++ {
		cs += frame[i]
	}
	frame[19] = cs
	return frame, nil
}

// buildReadInfoFrame 构造 READ_INFO（BB1E）请求帧，返回 19 字节。
// 结构与 buildReadMotorFrame 完全相同，仅 cmd_id 不同（BB1E vs BB1D）。
// 对应 Perl UserValve.pm L141-146 READ_INFO 模板 + L262-263 cmd_id 覆盖。
func buildReadInfoFrame(mac string) ([]byte, error) {
	frame := []byte{
		0xfe, 0xfe, 0xfe, 0x68, 0x20,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // 5-10 设备 ID（占位）
		0x00,       // 11
		0x01,       // 12 C
		0x03,       // 13 data_len（cmd_id 2 + sub_id 1 = 3）
		0xbb, 0x1e, // 14-15 cmd_id BB1E
		0x01, // 16 sub_id
		0x00, // 17 CS（随后重算）
		0x16, // 18 结束符
	}
	id, err := parseMbusID(mac)
	if err != nil {
		return nil, err
	}
	for i, v := range id {
		frame[5+i] = v
	}
	var cs byte
	for i := 3; i <= 16; i++ {
		cs += frame[i]
	}
	frame[17] = cs
	return frame, nil
}

// buildUpgradeDummyFrame 构造升级前的"地址唤醒"帧，对应 Perl UserValve.pm
// dev_mubs_addr_cmd_dummy L200-217。字节为 Perl 字面量原样（checksum 0xc0
// 按字面量发送，不重算）。
func buildUpgradeDummyFrame() []byte {
	return []byte{
		0xfe, 0xfe, 0xfe, 0x68, 0x20,
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, // 5-11 地址（哑地址）
		0x03, 0x03, // 12-13 C / data_len
		0x81, 0x0a, // 14-15 cmd_id
		0x01, // 16 sub_id
		0xc0, // 17 CS（Perl 字面量，不重算）
		0x16, // 18 结束符
	}
}

// buildUpgradeFrame 构造 UPGRADE_REQ（BB20）升级请求帧，155 字节。对应
// Perl UserValve.pm dev_get_mbus_data("UPGRADE_REQ") L162-175 模板 +
// dev_mbus_fill_set_cmd BB20 分支 L278-300：
//
//	索引: 0-2 前导 FE FE FE | 3: 68 | 4: 0x20
//	| 5-11: 设备ID（12 位 mac 填 5..10、[11] 保持 0；14 位填 5..11）
//	| 12: 01(C) | 13: 8B(data_len=139=155-16)
//	| 14-15: cmd_id BB 20 | 16: 01(sub_id)
//	| 17-18: dev_type LE（未使用，0）
//	| 19: hard_ver | 20: soft_ver | 21-22: block_id LE
//	| 23: block_size | 24: is_end（1 或 0）
//	| 25-152: payload，长度 == len(blk.Data)（1..128），剩余补 0
//	| 153: CS = sum(3..152) & 0xff（cmd_len = data_len+14 = 153）| 154: 16
//
// 数据区从 [17] 开始，布局对齐 slave 端 mbus_slave_handler_upgrade：
// data[0..1] dev_type(LE) data[2] hard_ver data[3] soft_ver
// data[4..5] block_id(LE) data[6] block_size data[7] is_end data[8..] payload。
// 先做参数校验（validateUpgradeBlock）：非法（Data 超 128 / BlockSize 不匹配
// / BlockID 超范围等）返回 error，不静默截断（oracle 审查强化）。
func buildUpgradeFrame(mac string, blk UpgradeBlock) ([]byte, error) {
	if err := validateUpgradeBlock(blk); err != nil {
		return nil, err
	}
	frame := make([]byte, 155)
	frame[0], frame[1], frame[2], frame[3] = 0xfe, 0xfe, 0xfe, 0x68
	frame[4] = 0x20
	frame[12] = 0x01
	frame[13] = 0x8b // data_len = 155 - 16（dev_mbus_fill_set_cmd L305-307）
	frame[14], frame[15] = 0xbb, 0x20
	frame[16] = 0x01
	// 17-18 dev_type LE 未使用，保持 0
	frame[19] = blk.HardVer
	frame[20] = blk.SoftVer
	frame[21] = byte(blk.BlockID & 0xFF)
	frame[22] = byte((blk.BlockID >> 8) & 0xFF)
	frame[23] = byte(blk.BlockSize)
	if blk.IsEnd {
		frame[24] = 1
	}
	// payload [25..152]：Data 长度已校验 1..128，剩余保持模板 0x00
	copy(frame[25:25+len(blk.Data)], blk.Data)

	id, err := parseMbusID(mac)
	if err != nil {
		return nil, err
	}
	for i, v := range id {
		frame[5+i] = v
	}
	// CS = sum(3..152)（Perl L316-322：cmd_len = data_len+14 = 153）
	var cs byte
	for i := 3; i <= 152; i++ {
		cs += frame[i]
	}
	frame[153] = cs
	frame[154] = 0x16
	return frame, nil
}

// ---- commandTrans（对应 Perl BLAT serial.pm command_trans L502-552 的
// mbus 分支 + L688-709 收发循环）----

// commandTrans 发送请求帧并匹配 M-Bus 响应，命中返回完整响应帧 hex
// （大写，与 Perl $ret 一致）。全部 retry 轮次失败返回 error。
//
// 响应支持两种形态（对应 M-Bus EN 13757-3）：
//   - 长帧：FE FE FE 68 ... 16（READ_INFO 等查询命令）；
//   - 短帧：单字节 0xE5（SND_UD 类 SET 命令的 ACK，对应 Perl 那边
//     实测 slave 经常回 0xE5 而非长帧，旧 matcher 会漏掉导致超时）。
//
// 0xE5 命中时返回字符串 "E5"，调用方按"成功 ACK"处理。
func (d *Device) commandTrans(ctx context.Context, port serial.Port, frame []byte, timeout time.Duration, retry int) (string, error) {
	reqHex := strings.ToUpper(hex.EncodeToString(frame))
	// 请求帧 hex 提取 cmd_id（对应 Perl L502 /^FEFEFE68\w{20}(\w{4})\w+16/i）
	reqRe := regexp.MustCompile(`^FEFEFE68\w{20}(\w{4})\w+16`)
	m := reqRe.FindStringSubmatch(reqHex)
	if m == nil {
		return "", fmt.Errorf("mbus: 请求帧格式无法识别: %s", reqHex)
	}
	matcher := makeMBusMatcher(m[1])

	// writeFailures 统计全部 retry 轮次里 Write 明确失败（err != nil）的
	// 次数：若所有轮次都写失败，返回 errMBusWrite（区别于超时），供
	// UpgradeByMbus 判定 dummy 唤醒帧的真实写错误；至少一次 Write 成功但
	// 无匹配响应仍按超时处理，不影响其它命令的既有行为。
	writeFailures := 0
	for i := 0; i < retry; i++ {
		if err := ctxErr(ctx); err != nil {
			return "", err
		}
		n, err := port.Write(frame)
		if err != nil {
			writeFailures++
			d.logInfo(fmt.Sprintf("mbus 发送失败: %v", err))
			continue // 明确写错误：进入下一轮 retry
		}
		// 短写（err==nil 但 n < len(frame)）：串口只收半帧，同一 transaction
		// 直接重试会把残帧追加到下一帧上，必须立即返回不可重试错误
		//（oracle 复审）。
		if n < len(frame) {
			d.logInfo(fmt.Sprintf("mbus 短写 %d/%d 字节", n, len(frame)))
			return "", errMBusShortWrite
		}
		deadline := time.Now().Add(timeout)
		var recvHex string
		for time.Now().Before(deadline) {
			if err := ctxErr(ctx); err != nil {
				return "", err
			}
			buf := make([]byte, 256)
			n, err := port.Read(buf)
			if err != nil {
				// 读取错误（非无数据）→ 记录日志，等下一轮 retry
				d.logInfo(fmt.Sprintf("mbus 读取错误: %v", err))
				break
			}
			if n > 0 {
				recvHex += strings.ToUpper(hex.EncodeToString(buf[:n]))
				if ret := matcher(recvHex); ret != "" {
					return ret, nil
				}
			} else {
				// 无数据：非阻塞轮询，睡 20ms（对应 Perl _BlockRead 的
				// usleep(20000)）；ctx 取消优先返回。
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
	}
	if writeFailures == retry && retry > 0 {
		return "", errMBusWrite
	}
	return "", fmt.Errorf("mbus: 等待响应超时（重试 %d 次）", retry)
}

// makeMBusMatcher 返回对应 Perl serial.pm L506-529 的 matcher 闭包：
// 输入累积的接收 hex 大写字符串，命中返回完整响应帧 hex，否则返回空串。
//
//	长帧：循环找 FE68 位置 → 其后至少 18 个 hex → L = hex(FE68 后 18 hex 后的 2 hex)
//	→ 其后 4 hex == reqMbusDi → m_data = 从 FE68 起到末尾；
//	hex_len = len(m_data)；若 hex_len < m_len*2+28 继续找下一个 FE68；
//	ret = "FEFE" + m_data[:m_len*2+28]；ret 必须以 "16" 结尾；
//	字节级 CS = sum(字节 3..len-3) & 0xff 须等于字节 len-2；
//	全通过 → 返回 ret。
//
//	短帧：M-Bus 规范下 SET 类命令（cmd_id BB1F 等）slave 回单字节 0xE5 ACK。
//	本 matcher 在长帧未命中时检查 recvHex 是否以 "E5" 开头，是则返回 "E5"
//	供调用方按"成功 ACK"处理；老代码只匹配长帧，会被 0xE5 卡到 5 次重试
//	超时——这正是产线 dev_normal_check_motor 末段 CaliValveByMbus 失败的根因。
//
// 顺序：先查长帧（M-Bus 一次请求只对应一次响应，长帧优先）；未命中再
// 退而求其次看是否以 "E5" 开头。recvHex 由 commandTrans 在收数据时统一
// ToUpper，所以这里直接比较 "E5" 即可。
func makeMBusMatcher(reqMbusDi string) func(recvHex string) string {
	re := regexp.MustCompile(`FE68\w{18}(\w{2})` + reqMbusDi)
	return func(recvHex string) string {
		// 先尝试长帧
		recv := recvHex
		for {
			loc := re.FindStringSubmatchIndex(recv)
			if loc == nil {
				break
			}
			mData := recv[loc[0]:]
			mLen, _ := strconv.ParseUint(recv[loc[2]:loc[3]], 16, 8)
			need := int(mLen)*2 + 28
			if len(mData) < need {
				recv = recv[loc[1]:]
				continue
			}
			ret := "FEFE" + mData[:need]
			if !strings.HasSuffix(ret, "16") {
				recv = recv[loc[1]:]
				continue
			}
			bs, err := hex.DecodeString(ret)
			if err != nil {
				recv = recv[loc[1]:]
				continue
			}
			var cs byte
			for i := 3; i <= len(bs)-3; i++ {
				cs += bs[i]
			}
			if bs[len(bs)-2] != cs {
				recv = recv[loc[1]:]
				continue
			}
			return ret
		}
		// 长帧未命中 → 退而求其次看是否以 0xE5 短帧开头（M-Bus SET 命令 ACK）
		if strings.HasPrefix(recvHex, "E5") {
			return "E5"
		}
		return ""
	}
}

// ctxErr 返回 ctx 已取消的错误，否则 nil。
func ctxErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
