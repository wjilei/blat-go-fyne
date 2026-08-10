package mbus

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"blat/internal/serial"
)

// captureLogger 实现 mbus.Logger，把 Info 输出写入 buf（线程安全，logger
// 内部可能从 commandTrans 的 goroutine 调用）。
type captureLogger struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *captureLogger) Info(args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintln(&c.buf, args...)
}

func (c *captureLogger) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *captureLogger) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

// ---- fake Port（测试注入，不触真实串口）----

// fakePort 实现 serial.Port：Write 记录帧；Read 按 chunks 依次返回，
// chunks 耗尽后返回 (0, nil)（模拟无数据）；err 非 nil 时 Read 返回错误。
type fakePort struct {
	written [][]byte
	chunks  [][]byte
	err     error
}

func (f *fakePort) Write(b []byte) (int, error) {
	f.written = append(f.written, append([]byte(nil), b...))
	return len(b), nil
}

func (f *fakePort) Read(buf []byte) (int, error) {
	if len(f.chunks) > 0 {
		c := f.chunks[0]
		f.chunks = f.chunks[1:]
		return copy(buf, c), nil
	}
	if f.err != nil {
		return 0, f.err
	}
	return 0, nil
}

func (f *fakePort) Close() error { return nil }

var _ serial.Port = (*fakePort)(nil)

// buildTestResponseFrame 构造一个能通过 Perl matcher 校验的 BB1D 响应帧
// （20 字节，L=0x04、数据 2 字节，状态位于 hex 位置 34-35）：
//
//	FE FE FE 68 + 9 字节 00 + 04 + BB 1D + 00 <status> + CS + 16
//
// CS = sum(字节 3..17) & 0xff（对应 Perl serial.pm L519-525 的 matcher 校验）。
// 说明：matcher 要求 m_data 长度 ≥ 2*L+28 且 MBusReadMotor 正则
// `^\w{34}(\w{2})\w{2}16$` 要求返回帧恰 40 个 hex 字符，故 L 只能取 0x04、
// 数据只能 2 字节（数据[1] 即状态，位于 hex 位置 34-35）。
func buildTestResponseFrame(status byte) string {
	f := []byte{0xfe, 0xfe, 0xfe, 0x68}
	f = append(f, make([]byte, 9)...) // 9 字节固定信息
	f = append(f, 0x04, 0xbb, 0x1d)   // L=0x04, cmd_id=BB1D
	f = append(f, 0x00, status)       // 数据 2 字节，状态=数据[1]
	var cs byte
	for i := 3; i <= 17; i++ {
		cs += f[i]
	}
	f = append(f, cs, 0x16)
	return hex.EncodeToString(f)
}

// ---- 帧构造 ----

func TestBuildReadMotorFrame_12DigitMAC(t *testing.T) {
	frame, err := buildReadMotorFrame("262601300011")
	if err != nil {
		t.Fatalf("buildReadMotorFrame 意外错误: %v", err)
	}
	if len(frame) != 19 {
		t.Fatalf("帧长度 = %d, 期望 19", len(frame))
	}
	// 独立重算校验和交叉验证（sum(3..16) & 0xff，对应 Perl UserValve.pm L246-252）
	var cs byte
	for i := 3; i <= 16; i++ {
		cs += frame[i]
	}
	want := "fefefe6820" + "110030012626" + "000103bb1d01" + fmt.Sprintf("%02x", cs) + "16"
	got := hex.EncodeToString(frame)
	if got != want {
		t.Fatalf("帧 hex = %s\n期望   = %s", got, want)
	}
	if frame[17] != cs {
		t.Fatalf("校验和字节 = %02x, 期望 %02x", frame[17], cs)
	}
}

func TestBuildReadMotorFrame_InvalidMAC(t *testing.T) {
	for _, mac := range []string{"26260130001", "2626013000111", "2626a1300011", "", "abc"} {
		if _, err := buildReadMotorFrame(mac); err == nil {
			t.Errorf("mac %q 应返回错误", mac)
		}
	}
}

func TestBuildReadMotorFrame_10DigitMAC(t *testing.T) {
	frame, err := buildReadMotorFrame("2132011234")
	if err != nil {
		t.Fatalf("buildReadMotorFrame 意外错误: %v", err)
	}
	// mbus_id[5] = 0（10 位时第 6 个 MAC 字节为 0，对应 Perl UserValve.pm L211）
	if frame[10] != 0x00 {
		t.Fatalf("frame[10] = %02x, 期望 00（10 位 mac 末字节补 0）", frame[10])
	}
	// 前 5 字节倒序：21 32 01 12 34 → 34 12 01 32 21
	want := []byte{0x34, 0x12, 0x01, 0x32, 0x21}
	for i, v := range want {
		if frame[5+i] != v {
			t.Fatalf("frame[%d] = %02x, 期望 %02x", 5+i, frame[5+i], v)
		}
	}
}

func TestBuildReadMotorFrame_14DigitMAC(t *testing.T) {
	frame, err := buildReadMotorFrame("11223344556677")
	if err != nil {
		t.Fatalf("buildReadMotorFrame 意外错误: %v", err)
	}
	// 14 位：7 组倒序填充 5..11（索引 11 的模板 0x00 被覆盖，Perl L243-245）
	want := []byte{0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11}
	for i, v := range want {
		if frame[5+i] != v {
			t.Fatalf("frame[%d] = %02x, 期望 %02x", 5+i, frame[5+i], v)
		}
	}
}

// ---- matcher（对应 Perl serial.pm L506-529）----

func TestMBusMatcher_Hit(t *testing.T) {
	// 合法响应帧：状态 01 位于 hex 位置 34-35
	hexStr := buildTestResponseFrame(0x01)
	matcher := makeMBusMatcher("BB1D")
	ret := matcher(strings.ToUpper(hexStr))
	if ret == "" {
		t.Fatal("合法响应帧未被 matcher 命中")
	}
	if !strings.HasSuffix(ret, "16") {
		t.Fatalf("matcher 返回帧不以 16 结尾: %s", ret)
	}
	// 完整帧 hex 应与输入一致（matcher 不做截断，返回完整帧）
	if strings.ToLower(ret) != hexStr {
		t.Fatalf("matcher 返回 = %s\n期望        = %s", strings.ToLower(ret), hexStr)
	}
	stat, err := parseMotorResponse(ret)
	if err != nil {
		t.Fatalf("parseMotorResponse 意外错误: %v", err)
	}
	if stat != "01" {
		t.Fatalf("状态 = %q, 期望 %q", stat, "01")
	}
}

func TestMBusMatcher_NoHit(t *testing.T) {
	matcher := makeMBusMatcher("BB1D")
	valid := buildTestResponseFrame(0x01)

	// 结尾非 16
	if ret := matcher(strings.ToUpper(valid[:38] + "15")); ret != "" {
		t.Fatalf("结尾非 16 应不命中, 却返回 %s", ret)
	}
	// CS 错误：状态字节改成 44 但 CS 保持原值（状态位于 hex 位置 34-35）
	if ret := matcher(strings.ToUpper(valid[:34] + "44" + valid[36:])); ret != "" {
		t.Fatalf("CS 错误应不命中, 却返回 %s", ret)
	}
	// 长度不足：缺 16 结束符
	if ret := matcher(strings.ToUpper(valid[:38])); ret != "" {
		t.Fatalf("长度不足应不命中, 却返回 %s", ret)
	}
	// 空输入 / 无关数据
	if ret := matcher("AABBCC"); ret != "" {
		t.Fatalf("无关数据应不命中, 却返回 %s", ret)
	}
	// 累积缓冲里无完整帧（只有前缀）也不命中
	if ret := matcher("FEFEFE680000000000"); ret != "" {
		t.Fatalf("不完整帧应不命中, 却返回 %s", ret)
	}
}

// ---- parseMotorResponse（对应 Perl UserValve.pm L409）----

func TestParseMotorResponse(t *testing.T) {
	ret := strings.ToUpper(buildTestResponseFrame(0x09))
	stat, err := parseMotorResponse(ret)
	if err != nil {
		t.Fatalf("parseMotorResponse 意外错误: %v", err)
	}
	if stat != "09" {
		t.Fatalf("状态 = %q, 期望 %q", stat, "09")
	}

	for _, bad := range []string{"", "FE16", "FEFEFE6800000000000000000004BB1D000145", ret[:38]} {
		if _, err := parseMotorResponse(bad); err == nil {
			t.Errorf("输入 %q 应解析失败", bad)
		}
	}
}

// ---- commandTrans（对应 Perl serial.pm L688-709 收发循环）----

func TestCommandTrans_FragmentedRead(t *testing.T) {
	// 构造 BB1D 请求帧作为发送内容（commandTrans 用正则识别 cmd_id）
	frame, err := buildReadMotorFrame("262601300011")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(buildTestResponseFrame(0x01))
	if err != nil {
		t.Fatal(err)
	}
	port := &fakePort{chunks: [][]byte{raw[:12], raw[12:]}}
	d := NewMockDevice()
	ret, err := d.commandTrans(context.Background(), port, frame, time.Second, 5)
	if err != nil {
		t.Fatalf("commandTrans 意外错误: %v", err)
	}
	if len(port.written) != 1 {
		t.Fatalf("Write 次数 = %d, 期望 1（命中后不再重试）", len(port.written))
	}
	if strings.ToLower(ret) != buildTestResponseFrame(0x01) {
		t.Fatalf("命中返回 = %s\n期望      = %s", strings.ToLower(ret), buildTestResponseFrame(0x01))
	}
}

func TestCommandTrans_BadCS_NoHit(t *testing.T) {
	frame, err := buildReadMotorFrame("262601300011")
	if err != nil {
		t.Fatal(err)
	}
	// CS 错误帧：状态字节 01→44，CS 未同步更新
	bad := buildTestResponseFrame(0x01)
	bad = bad[:34] + "44" + bad[36:]
	raw, _ := hex.DecodeString(bad)
	port := &fakePort{chunks: [][]byte{raw}}
	d := NewMockDevice()
	// 短超时加速：2 轮 × 60ms
	_, err = d.commandTrans(context.Background(), port, frame, 60*time.Millisecond, 2)
	if err == nil {
		t.Fatal("CS 错误帧应导致 commandTrans 失败")
	}
}

func TestCommandTrans_CtxCancelled(t *testing.T) {
	frame, err := buildReadMotorFrame("262601300011")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 已取消
	port := &fakePort{}
	d := NewMockDevice()
	_, err = d.commandTrans(ctx, port, frame, time.Second, 5)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的 ctx 应返回 context.Canceled, 却得到 %v", err)
	}
}

func TestCommandTrans_UnknownRequest(t *testing.T) {
	d := NewMockDevice()
	_, err := d.commandTrans(context.Background(), &fakePort{}, []byte{0x01, 0x02}, time.Second, 1)
	if err == nil {
		t.Fatal("无法识别的请求帧应返回错误")
	}
}

// ---- mock Device ----

func TestMockDevice_MBusReadMotor(t *testing.T) {
	d := NewMockDevice()
	ctx := context.Background()

	// 未 Connect → 错误
	if _, err := d.MBusReadMotor(ctx, "262601300011"); err == nil {
		t.Fatal("未连接时应返回错误")
	}

	if err := d.Connect(ctx, "COM9"); err != nil {
		t.Fatalf("Connect 意外错误: %v", err)
	}
	// 幂等：重复 Connect 返回 nil
	if err := d.Connect(ctx, "COM9"); err != nil {
		t.Fatalf("重复 Connect 应幂等返回 nil, 却得到 %v", err)
	}

	st, err := d.MBusReadMotor(ctx, "262601300011")
	if err != nil {
		t.Fatalf("MBusReadMotor 意外错误: %v", err)
	}
	if st != "01" {
		t.Fatalf("默认 mock 状态 = %q, 期望 %q", st, "01")
	}

	d.SetMockStatus("09")
	st, err = d.MBusReadMotor(ctx, "262601300011")
	if err != nil {
		t.Fatalf("MBusReadMotor 意外错误: %v", err)
	}
	if st != "09" {
		t.Fatalf("SetMockStatus 后状态 = %q, 期望 %q", st, "09")
	}

	if err := d.Disconnect(); err != nil {
		t.Fatalf("Disconnect 意外错误: %v", err)
	}
	if _, err := d.MBusReadMotor(ctx, "262601300011"); err == nil {
		t.Fatal("Disconnect 后应返回错误")
	}
}

// ---- debug 日志（仅 --debug 模式打印发送/接收 hex）----

// TestMBusReadMotor_DebugLogging 验证 --debug 模式下 MBusReadMotor 输出
// 发送/接收数据 hex；非 debug 模式静默（不输出 send/recv 标记）。
// mock 模式：仅记录 "返回状态" 一行；real 模式（含 fakePort 模拟）记录
// 完整发送帧与命中响应帧。
func TestMBusReadMotor_DebugLogging(t *testing.T) {
	ctx := context.Background()

	// mock：debug=true 打印状态，debug=false 静默
	d := NewMockDevice()
	d.SetMockStatus("09")
	log := &captureLogger{}
	d.SetLogger(log)
	if err := d.Connect(ctx, "COM9"); err != nil {
		t.Fatalf("Connect 意外错误: %v", err)
	}

	// debug=false：不应出现 send/recv/状态 标记
	d.SetDebug(false)
	if _, err := d.MBusReadMotor(ctx, "262601300011"); err != nil {
		t.Fatalf("MBusReadMotor 意外错误: %v", err)
	}
	if out := log.String(); strings.Contains(out, "mbus 发送") || strings.Contains(out, "mbus 接收") || strings.Contains(out, "mbus[mock]") {
		t.Fatalf("debug=false 不应打印 mbus 调试日志, 实际输出: %q", out)
	}

	// debug=true：mock 模式不再额外打 "返回状态" 行（mbus 发送/接收 hex
	// 日志已全部移除，debug=true 时也应保持日志干净）。这里只断言
	// 不出现 send/recv hex 标记。
	log.Reset()
	d.SetDebug(true)
	if _, err := d.MBusReadMotor(ctx, "262601300011"); err != nil {
		t.Fatalf("MBusReadMotor 意外错误: %v", err)
	}
	out := log.String()
	if strings.Contains(out, "mbus 发送") {
		t.Fatalf("mock 模式不应出现 real 路径的 send 标记, 实际输出: %q", out)
	}
	if strings.Contains(out, "mbus 接收") {
		t.Fatalf("mock 模式不应出现 recv 标记, 实际输出: %q", out)
	}

	// real：debug=true 不再额外打 send/recv hex（已全部移除，仅保留
	// 正常流程日志 "电机状态：XX"）。这里只断言业务结果 + 不出现 hex 标记。
	respRaw, err := hex.DecodeString(buildTestResponseFrame(0x01))
	if err != nil {
		t.Fatal(err)
	}

	portDebug := &fakePort{chunks: [][]byte{respRaw}}
	dReal := NewRealDevice()
	dReal.SetLogger(log)
	dReal.SetDebug(true)
	dReal.port = portDebug
	if st, err := dReal.MBusReadMotor(ctx, "262601300011"); err != nil {
		t.Fatalf("real MBusReadMotor 意外错误: %v", err)
	} else if st != "01" {
		t.Fatalf("real 状态 = %q, 期望 %q", st, "01")
	}
	outReal := log.String()
	if strings.Contains(outReal, "mbus 发送") {
		t.Fatalf("real debug 不应再打印 send hex, 实际输出: %q", outReal)
	}
	if strings.Contains(outReal, "mbus 接收:") {
		t.Fatalf("real debug 不应再打印 recv hex, 实际输出: %q", outReal)
	}
	if !strings.Contains(outReal, "电机状态：01") {
		t.Fatalf("real 正常流程应打印 电机状态：01, 实际输出: %q", outReal)
	}

	// real：debug=false 静默（连 "电机状态" 都不打？看下面说明）
	// 实际 "电机状态" 始终打，debug 只控制 send/recv hex。debug=false
	// 时仅不打 send/recv hex；这里同时检查不打 hex、不打状态行外的噪声。
	log.Reset()
	portQuiet := &fakePort{chunks: [][]byte{respRaw}}
	dRealQ := NewRealDevice()
	dRealQ.SetLogger(log)
	dRealQ.SetDebug(false)
	dRealQ.port = portQuiet
	if _, err := dRealQ.MBusReadMotor(ctx, "262601300011"); err != nil {
		t.Fatalf("MBusReadMotor 意外错误: %v", err)
	}
	if out3 := log.String(); strings.Contains(out3, "mbus 发送") || strings.Contains(out3, "mbus 接收:") {
		t.Fatalf("real debug=false 不应打印 send/recv hex, 实际输出: %q", out3)
	}
}

// ---- SET_VALVE (BB1F) 帧构造（对应 Perl UserValve.pm L156-160 SET_VALVE
// 模板 + L273-277 数据填充，调用方为 CaliValveByMbus/open_pre=0/calc_day=255）----

// buildTestSetValveResponse 构造一个能通过 Perl _SetValveByMbus 正则校验
// 的 BB1F 响应帧（19 字节：17 字节 header + 1 字节 CS + 1 字节 0x16）。
// 对应 Perl UserValve.pm L617-621：`^\w{34}\w{2}16$` 命中即视为成功。
func buildTestSetValveResponse() string {
	f := []byte{0xfe, 0xfe, 0xfe, 0x68}
	f = append(f, make([]byte, 9)...) // L(0) + ID(6) + ext + C
	f = append(f, 0x03, 0xbb, 0x1f)   // data_len=3, cmd_id=BB1F
	f = append(f, 0x01)               // sub_id
	var cs byte
	for i := 3; i <= 16; i++ {
		cs += f[i]
	}
	f = append(f, cs, 0x16)
	return strings.ToUpper(hex.EncodeToString(f))
}

func TestBuildSetValveFrame_12DigitMAC(t *testing.T) {
	// 12 位 mac + open_pre=0, calc_day=255（CaliValveByMbus 校准）
	frame, err := buildSetValveFrame("262601300011", 0, 0xff)
	if err != nil {
		t.Fatalf("buildSetValveFrame 意外错误: %v", err)
	}
	if len(frame) != 21 {
		t.Fatalf("帧长度 = %d, 期望 21（SET_VALVE 模板 UserValve.pm L156-160）", len(frame))
	}
	// data section 5 字节：BB 1F 01 open_pre calc_day（UserValve.pm L273-277）
	if frame[14] != 0xbb || frame[15] != 0x1f {
		t.Fatalf("cmd_id = %02x%02x, 期望 bb1f", frame[14], frame[15])
	}
	if frame[16] != 0x01 {
		t.Fatalf("sub_id = %02x, 期望 01", frame[16])
	}
	if frame[17] != 0x00 || frame[18] != 0xff {
		t.Fatalf("open_pre=%02x calc_day=%02x, 期望 00 ff", frame[17], frame[18])
	}
	// CS = sum(3..18) & 0xff（cmd_len = data_len+14 = 19，UserValve.pm L317-322）
	var cs byte
	for i := 3; i <= 18; i++ {
		cs += frame[i]
	}
	if frame[19] != cs {
		t.Fatalf("CS = %02x, 期望 %02x", frame[19], cs)
	}
	if frame[20] != 0x16 {
		t.Fatalf("结束符 = %02x, 期望 16", frame[20])
	}
}

func TestBuildSetValveFrame_InvalidMAC(t *testing.T) {
	for _, mac := range []string{"26260130001", "2626013000111", "2626a1300011", "", "abc"} {
		if _, err := buildSetValveFrame(mac, 0, 0xff); err == nil {
			t.Errorf("mac %q 应返回错误", mac)
		}
	}
}

func TestParseSetValveResponse(t *testing.T) {
	if err := parseSetValveResponse(buildTestSetValveResponse()); err != nil {
		t.Fatalf("parseSetValveResponse 合法响应意外错误: %v", err)
	}
	// 格式异常：长度不对、缺 16 结束符、CS 错位
	for _, bad := range []string{
		"",
		"FEFEFE6800000000000000000003BB1F0100",
		"FEFEFE6800000000000000000003BB1F01FF15", // 结束符非 16
		"FEFEFE6800000000000000000003BB1F01FF00", // 缺结束符
	} {
		if err := parseSetValveResponse(bad); err == nil {
			t.Errorf("输入 %q 应解析失败", bad)
		}
	}
}

// ---- READ_INFO (BB1E) 帧构造（对应 Perl UserValve.pm L141-146 READ_INFO
// 模板 + L556-599 _MbusReadInfo 响应解析）----

func TestBuildReadInfoFrame_12DigitMAC(t *testing.T) {
	frame, err := buildReadInfoFrame("262601300011")
	if err != nil {
		t.Fatalf("buildReadInfoFrame 意外错误: %v", err)
	}
	if len(frame) != 19 {
		t.Fatalf("帧长度 = %d, 期望 19", len(frame))
	}
	if frame[14] != 0xbb || frame[15] != 0x1e {
		t.Fatalf("cmd_id = %02x%02x, 期望 bb1e", frame[14], frame[15])
	}
	// 整体 hex 与 buildReadMotorFrame 对齐，仅 cmd_id 改为 BB1E
	var cs byte
	for i := 3; i <= 16; i++ {
		cs += frame[i]
	}
	want := "fefefe6820" + "110030012626" + "000103bb1e01" + fmt.Sprintf("%02x", cs) + "16"
	if got := hex.EncodeToString(frame); got != want {
		t.Fatalf("帧 hex = %s\n期望   = %s", got, want)
	}
}

func TestBuildReadInfoFrame_InvalidMAC(t *testing.T) {
	for _, mac := range []string{"26260130001", "2626013000111", "2626a1300011", "", "abc"} {
		if _, err := buildReadInfoFrame(mac); err == nil {
			t.Errorf("mac %q 应返回错误", mac)
		}
	}
}

// buildTestInfoResponse 构造 BB1E 响应帧（39 字节：14 字节 header + 23
// 字节数据 + 1 字节 CS + 1 字节 0x16）。数据布局对齐 Perl UserValve.pm
// L569：cmd_id BB 1E + sub_id 01 + 20 字节业务数据
// (temp_output(4B LE) temp_input(4B LE) open_pre hard_ver soft_ver
// alarm flow_rate(4B LE) cum_hot(4B LE))。
//
// 注意 data_len=23 而非 20：Go 侧 makeMBusMatcher 用 L (= data_len) 算响应
// 长度 need=L*2+28，BB1D 这类小响应刚好等于 (16+data_len) 字节总长，BB1E
// 响应 39 字节需 data_len=23 才能让 need 覆盖完整 mData，避免被截断。
// 23 字节 = cmd_id(2) + sub_id(1) + 业务数据(20)。
// temp_* 传 uint32，最高位为 1 时视为负数（Perl L574-575 trans_hex_negative）。
func buildTestInfoResponse(tempOut, tempIn, openPre, hardVer, softVer, alarm, flow, cumHot uint32) string {
	f := []byte{0xfe, 0xfe, 0xfe, 0x68}
	f = append(f, make([]byte, 9)...) // L + ID(6) + ext + C
	f = append(f, 0x17)               // data_len=23 (cmd_id 2 + sub_id 1 + 业务 20)
	f = append(f, 0xbb, 0x1e)         // cmd_id=BB1E
	f = append(f, 0x01)               // sub_id
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, tempOut)
	f = append(f, buf...)
	binary.LittleEndian.PutUint32(buf, tempIn)
	f = append(f, buf...)
	f = append(f, byte(openPre), byte(hardVer), byte(softVer), byte(alarm))
	binary.LittleEndian.PutUint32(buf, flow)
	f = append(f, buf...)
	binary.LittleEndian.PutUint32(buf, cumHot)
	f = append(f, buf...)
	var cs byte
	for i := 3; i <= 36; i++ {
		cs += f[i]
	}
	f = append(f, cs, 0x16)
	return strings.ToUpper(hex.EncodeToString(f))
}

func TestParseInfoResponse(t *testing.T) {
	// 正温度 + 正向流量 + 固定 cum_hot
	ret := buildTestInfoResponse(2500, 2000, 45, 1, 2, 3, 1234, 0x12345678)
	info, err := parseInfoResponse(ret)
	if err != nil {
		t.Fatalf("parseInfoResponse 意外错误: %v", err)
	}
	if info.TempOutput != 2500 || info.TempInput != 2000 {
		t.Errorf("TempOutput=%d TempInput=%d, 期望 2500/2000", info.TempOutput, info.TempInput)
	}
	if info.OpenPre != 45 || info.HardVer != 1 || info.SoftVer != 2 || info.Alarm != 3 {
		t.Errorf("单字节字段解析错: %+v", info)
	}
	if info.FlowRate != 1234 || info.CumHot != 0x12345678 {
		t.Errorf("FlowRate=%d CumHot=%#x, 期望 1234/0x12345678", info.FlowRate, info.CumHot)
	}

	// 负温度：temp_output = -2500（补码 0xFFFFF63C）→ LE 字节序
	ret = buildTestInfoResponse(0xfffff63c, 2000, 45, 1, 2, 3, 1234, 0x12345678)
	info, err = parseInfoResponse(ret)
	if err != nil {
		t.Fatalf("parseInfoResponse 负温度意外错误: %v", err)
	}
	if info.TempOutput != -2500 {
		t.Errorf("TempOutput(负) = %d, 期望 -2500", info.TempOutput)
	}
	if info.TempInput != 2000 {
		t.Errorf("TempInput 受负温度污染 = %d, 期望 2000", info.TempInput)
	}

	// 格式异常
	for _, bad := range []string{
		"",
		"FEFEFE6800000000000000000003BB1F0100",  // BB1F 帧
		"FEFEFE6800000000000000000014BB1E01",    // 缺数据
		"FEFEFE6800000000000000000014BB1E01C4090000D00700002D01020", // 缺末字节
	} {
		if _, err := parseInfoResponse(bad); err == nil {
			t.Errorf("输入 %q 应解析失败", bad)
		}
	}
}

// ---- mock Device: CaliValveByMbus / MbusReadInfo ----

func TestMockDevice_CaliValveByMbus(t *testing.T) {
	d := NewMockDevice()
	ctx := context.Background()

	// 未 Connect → 错误
	if err := d.CaliValveByMbus(ctx, "262601300011"); err == nil {
		t.Fatal("未连接时应返回错误")
	}

	if err := d.Connect(ctx, "COM9"); err != nil {
		t.Fatalf("Connect 意外错误: %v", err)
	}
	if err := d.CaliValveByMbus(ctx, "262601300011"); err != nil {
		t.Fatalf("mock CaliValveByMbus 应直接成功, 却得到 %v", err)
	}
}

func TestMockDevice_MbusReadInfo(t *testing.T) {
	d := NewMockDevice()
	ctx := context.Background()

	// 未 Connect → 错误
	if _, err := d.MbusReadInfo(ctx, "262601300011"); err == nil {
		t.Fatal("未连接时应返回错误")
	}

	if err := d.Connect(ctx, "COM9"); err != nil {
		t.Fatalf("Connect 意外错误: %v", err)
	}
	// 默认 mock info：Alarm=0，dev_normal_check_motor 末尾检查需通过
	info, err := d.MbusReadInfo(ctx, "262601300011")
	if err != nil {
		t.Fatalf("MbusReadInfo 意外错误: %v", err)
	}
	if info.Alarm != 0 {
		t.Errorf("默认 mock Alarm = %d, 期望 0", info.Alarm)
	}

	// 注入非零 alarm 后应原样返回
	d.SetMockInfo(MbusInfo{Alarm: 7, SoftVer: 5})
	info, err = d.MbusReadInfo(ctx, "262601300011")
	if err != nil {
		t.Fatalf("MbusReadInfo 意外错误: %v", err)
	}
	if info.Alarm != 7 || info.SoftVer != 5 {
		t.Errorf("SetMockInfo 后字段错误: %+v", info)
	}
}

// ---- real Device 走 fakePort 测一次往返（端到端走通协议 + 解析）----

func TestRealDevice_CaliValveByMbus_EndToEnd(t *testing.T) {
	raw, _ := hex.DecodeString(buildTestSetValveResponse())
	port := &fakePort{chunks: [][]byte{raw}}
	d := NewRealDevice()
	// 绕开真实串口 Connect（测试环境无 COM9 串口），直接把 fakePort 注入
	// Device.port。real 模式以 port != nil 为准，不依赖 connected。
	d.mu.Lock()
	d.port = port
	d.mu.Unlock()
	if err := d.CaliValveByMbus(context.Background(), "262601300011"); err != nil {
		t.Fatalf("CaliValveByMbus 端到端意外错误: %v", err)
	}
}

func TestRealDevice_MbusReadInfo_EndToEnd(t *testing.T) {
	raw, _ := hex.DecodeString(buildTestInfoResponse(2500, 2000, 45, 1, 2, 3, 1234, 0x12345678))
	port := &fakePort{chunks: [][]byte{raw}}
	d := NewRealDevice()
	d.mu.Lock()
	d.port = port
	d.mu.Unlock()
	info, err := d.MbusReadInfo(context.Background(), "262601300011")
	if err != nil {
		t.Fatalf("MbusReadInfo 端到端意外错误: %v", err)
	}
	if info.TempOutput != 2500 || info.Alarm != 3 {
		t.Errorf("字段错误: %+v", info)
	}
}

// ---- M-Bus 短帧 0xE5 ACK（SET 类命令 slave 标准响应）----

// TestRealDevice_CaliValveByMbus_ShortACK 验证 slave 回单字节 0xE5 时
// CaliValveByMbus 视为成功（不再走 19 字节长帧 regex）。这正是产线
// dev_normal_check_motor 之前卡 5 次重试超时的根因。
func TestRealDevice_CaliValveByMbus_ShortACK(t *testing.T) {
	raw, _ := hex.DecodeString("E5")
	port := &fakePort{chunks: [][]byte{raw}}
	d := NewRealDevice()
	d.mu.Lock()
	d.port = port
	d.mu.Unlock()
	if err := d.CaliValveByMbus(context.Background(), "262601300011"); err != nil {
		t.Fatalf("CaliValveByMbus 对 0xE5 ACK 应成功, 却得到: %v", err)
	}
}

// TestMakeMBusMatcher_ShortACK 直接覆盖 makeMBusMatcher 的 E5 命中分支。
// commandTrans 在收数据时统一 ToUpper 再喂给 matcher，所以这里只测大写。
func TestMakeMBusMatcher_ShortACK(t *testing.T) {
	m := makeMBusMatcher("BB1F")
	// 纯短帧
	if got := m("E5"); got != "E5" {
		t.Errorf("makeMBusMatcher(E5) = %q, 期望 \"E5\"", got)
	}
	// 短帧后跟噪声（旧设备可能 ACK 完还吐点东西）
	if got := m("E5FEFEFE68"); got != "E5" {
		t.Errorf("makeMBusMatcher(E5FEFEFE68) = %q, 期望 \"E5\"", got)
	}
	// 多个 0xE5（极端情况：slave 重复 ACK），首字节即命中
	if got := m("E5E5E5E5"); got != "E5" {
		t.Errorf("makeMBusMatcher(E5E5E5E5) = %q, 期望 \"E5\"", got)
	}
	// 长帧优先：合法长帧不会被误判为 0xE5
	if got := m(buildTestSetValveResponse()); got == "E5" || got == "" {
		t.Errorf("makeMBusMatcher 对合法长帧不应返回 E5, 却得到 %q", got)
	}
	// 不以 E5 开头也不是长帧 → 不命中
	if got := m("AABBCC"); got != "" {
		t.Errorf("makeMBusMatcher(AABBCC) = %q, 期望 \"\"", got)
	}
}

// TestParseSetValveResponse_ShortACK 验证 parseSetValveResponse 对 0xE5
// 视为成功（覆盖"短帧也算成功"分支）。
func TestParseSetValveResponse_ShortACK(t *testing.T) {
	if err := parseSetValveResponse("E5"); err != nil {
		t.Errorf("parseSetValveResponse(E5) 应为 nil, 却得到: %v", err)
	}
}
