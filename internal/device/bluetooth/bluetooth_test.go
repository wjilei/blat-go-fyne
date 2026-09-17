package bluetooth

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	bt "tinygo.org/x/bluetooth"

	"github.com/fxamacker/cbor/v2"
)

// TestParseIdToMac 校验 ParseIdToMac（新格式，FC:XX:XX:XX:XX:XX）与 Perl
// BLAT::Common::Utils::parseIdToMac 7b1f4349 升级后的输出一致（期望值用
// Go 独立复现 _id2MacArray/_hashTimes33 验证，取 uint64 哈希低 40 位 = Perl
// Math::BigInt 无限精度结果低 40 位）。
//
//	_id2MacArray：id 从末尾起每 2 字符一组 hex 填 16 数组（高位补 0）
//	_hashTimes33：djb2/times33（hash = hash*33 + v，起始 5381），取低 40 位
//
// 覆盖偶数长度、全数字串、奇数长度（Perl substr 负数 offset 从尾部倒数、
// 越界截断）三种情况。
func TestParseIdToMac(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"0ba6dc07dfcb", "FC:11:F4:61:38:83"},
		{"262601300011", "FC:C8:08:9F:9E:B3"},
		{"00", "FC:52:BD:CB:7F:05"},
		{"abc", "FC:3F:5C:2C:75:4D"}, // 奇数长度：末位单独取最后一个字符
	}
	for _, tc := range cases {
		if got := ParseIdToMac(tc.id); got != tc.want {
			t.Errorf("ParseIdToMac(%q) = %s, want %s", tc.id, got, tc.want)
		}
	}
}

// TestParseIdToMacOld 校验旧格式派生（FC:E8:92:XX:XX:XX，3 字节哈希）的
// 输出与升级前 ParseIdToMac 的取值一致——Connect 内部在扫描匹配阶段用
// 旧格式兼容 ≤58 固件设备。
func TestParseIdToMacOld(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"0ba6dc07dfcb", "FC:E8:92:61:38:83"},
		{"262601300011", "FC:E8:92:9F:9E:B3"},
		{"00", "FC:E8:92:CB:7F:05"},
		{"abc", "FC:E8:92:2C:75:4D"},
	}
	for _, tc := range cases {
		if got := ParseIdToMacOld(tc.id); got != tc.want {
			t.Errorf("ParseIdToMacOld(%q) = %s, want %s", tc.id, got, tc.want)
		}
	}
}

// TestLegacyMacFrom 校验新格式 MAC → 旧兼容 MAC 的派生（"FC:E8:92:" +
// 新 MAC 低 3 字节）。短输入/异常输入应返回空串（对齐 Perl 的
// len(targetUpperMAC) <= 12 短路）。
func TestLegacyMacFrom(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"FC:11:F4:61:38:83", "FC:E8:92:61:38:83"},
		{"FC:C8:08:9F:9E:B3", "FC:E8:92:9F:9E:B3"},
		{"", ""},
		{"FC:11:F4", ""}, // len <= 12 短路
	}
	for _, tc := range cases {
		if got := legacyMacFrom(tc.in); got != tc.want {
			t.Errorf("legacyMacFrom(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestConnectRecordsID 验证 Connect 记录原始序列号 id，mock Read 返回的
// Sn 默认为该 id（而非派生出的 BLE 地址），保证序列号校验默认通过。
func TestConnectRecordsID(t *testing.T) {
	d := NewMockDevice()
	const id = "0ba6dc07dfcb"

	if err := d.Connect(context.Background(), id); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !d.IsConnected(ParseIdToMac(id)) {
		t.Fatalf("IsConnected(%s) should be true after Connect(%q)", ParseIdToMac(id), id)
	}
	st := d.Read(context.Background())
	if st == nil {
		t.Fatal("Read returned nil")
	}
	if st.Sn != id {
		t.Errorf("mock Read Sn = %q, want %q (raw id, not derived mac %q)", st.Sn, id, ParseIdToMac(id))
	}

	// 幂等：已连接时再次 Connect 直接成功（对应 Perl _ensure_bluetooth_connected）。
	if err := d.Connect(context.Background(), id); err != nil {
		t.Errorf("second Connect should be a no-op: %v", err)
	}
}

// TestSetMockStatusSnPriority 验证显式注入的 Sn 优先于默认 id。
func TestSetMockStatusSnPriority(t *testing.T) {
	d := NewMockDevice()
	const id = "0ba6dc07dfcb"
	if err := d.Connect(context.Background(), id); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	d.SetMockStatus(Status{Sn: "injected-sn"})
	st := d.Read(context.Background())
	if st == nil {
		t.Fatal("Read returned nil")
	}
	if st.Sn != "injected-sn" {
		t.Errorf("Read Sn = %q, want injected-sn", st.Sn)
	}
}

// TestDevTypeByte 校验帧头设备类型字节：PSAV（大小写不敏感）→ "f9"，
// 其它（含空串）→ "f8"。
func TestDevTypeByte(t *testing.T) {
	cases := []struct {
		devType string
		want    string
	}{
		{"PSAV", "f9"},
		{"psav", "f9"},
		{"Psav", "f9"},
		{"PFW", "f8"},
		{"", "f8"},
		{"abc", "f8"},
	}
	for _, tc := range cases {
		if got := devTypeByte(tc.devType); got != tc.want {
			t.Errorf("devTypeByte(%q) = %q, want %q", tc.devType, got, tc.want)
		}
	}
}

// TestBuildReadFrame 校验读取帧构造：PSAV → "04 f9 a1 00...00"，
// PFW → "04 f8 a1 00...00"。
func TestBuildReadFrame(t *testing.T) {
	cases := []struct {
		devType string
		wantHex string
	}{
		{"PSAV", "04f9a100000000000000"},
		{"PFW", "04f8a100000000000000"},
	}
	for _, tc := range cases {
		got := hex.EncodeToString(buildReadFrame(tc.devType))
		if got != tc.wantHex {
			t.Errorf("buildReadFrame(%q) hex = %q, want %q", tc.devType, got, tc.wantHex)
		}
		if len(buildReadFrame(tc.devType)) != 10 {
			t.Errorf("buildReadFrame(%q) length = %d bytes, want 10", tc.devType, len(buildReadFrame(tc.devType)))
		}
	}
}

// TestDefaultSetConfigPayload 校验 SetConfigPayload 结构体方案的默认字段：
// BLAT gen_bluetooth_data_to_send 的时间戳/BeatDur/SetOpenPre/
// ValveActivityInterval/ReverseFlow 默认值，未覆盖字段（CtrlType 等）为 nil。
func TestDefaultSetConfigPayload(t *testing.T) {
	now := time.Date(2026, 8, 6, 10, 30, 45, 0, time.Local)
	p := DefaultSetConfigPayload(now)

	cases := []struct {
		name string
		got  *int
		want int
	}{
		{"Year", p.Year, 26},       // 2026 - 2000
		{"Month", p.Month, 7},      // 8 - 1（Perl localtime 0-11）
		{"Day", p.Day, 6},          //
		{"Hour", p.Hour, 10},       //
		{"Minute", p.Minute, 30},   //
		{"Second", p.Second, 45},   //
		{"BeatDur", p.BeatDur, 1380}, // 60*23
		{"SetOpenPre", p.SetOpenPre, 100},
		{"ValveActivityInterval", p.ValveActivityInterval, 30},
		{"ReverseFlow", p.ReverseFlow, 0},
	}
	for _, tc := range cases {
		if tc.got == nil {
			t.Errorf("%s = nil, want %d", tc.name, tc.want)
			continue
		}
		if *tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, *tc.got, tc.want)
		}
	}
	// 未传入字段必须为 nil（omitempty 才可能省略）。
	if p.CtrlType != nil || p.CtrlArg != nil || p.ForceCloseNBModule != nil {
		t.Error("CtrlType/CtrlArg/ForceCloseNBModule 应为 nil（默认未设置）")
	}
}

// TestBuildSetConfigFrame 校验配置帧构造：帧头 [0x01, devTypeByte] + CBOR(p)。
// payload 用 map[int]int 解码成功本身即证明键是整数（keyasint）。
// PSAV Reboot → 前缀 "01f9" 且 payload 含 tag 12=0x5a 与默认字段 tag 1；
// PFW DisableNbiot → 前缀 "01f8" 且 payload 含 tag 10=0xb3；
// PFW EnableNbiot → 前缀 "01f8" 且 tag 10=0 必须存在（回归：0 值不被吞掉）。
func TestBuildSetConfigFrame(t *testing.T) {
	now := time.Date(2026, 8, 6, 10, 30, 45, 0, time.Local)
	t.Run("PSAV Reboot", func(t *testing.T) {
		p := DefaultSetConfigPayload(now)
		p.CtrlType = intPtr(0x5a)
		frame, err := buildSetConfigFrame("PSAV", p)
		if err != nil {
			t.Fatalf("buildSetConfigFrame: %v", err)
		}
		got := hex.EncodeToString(frame)
		if len(got) < 4 || got[:4] != "01f9" {
			t.Errorf("hex = %q, want prefix \"01f9\"", got)
		}
		var m map[int]int
		if err := cbor.Unmarshal(frame[2:], &m); err != nil {
			t.Fatalf("cbor.Unmarshal payload: %v", err)
		}
		if m[12] != 0x5a {
			t.Errorf("tag 12 = %d, want 0x5a", m[12])
		}
		if _, ok := m[1]; !ok {
			t.Error("payload 缺默认字段 tag 1 (Year)")
		}
	})
	t.Run("PFW DisableNbiot", func(t *testing.T) {
		p := DefaultSetConfigPayload(now)
		p.ForceCloseNBModule = intPtr(0xb3)
		frame, err := buildSetConfigFrame("PFW", p)
		if err != nil {
			t.Fatalf("buildSetConfigFrame: %v", err)
		}
		got := hex.EncodeToString(frame)
		if len(got) < 4 || got[:4] != "01f8" {
			t.Errorf("hex = %q, want prefix \"01f8\"", got)
		}
		var m map[int]int
		if err := cbor.Unmarshal(frame[2:], &m); err != nil {
			t.Fatalf("cbor.Unmarshal payload: %v", err)
		}
		if m[10] != 0xb3 {
			t.Errorf("tag 10 = %d, want 0xb3", m[10])
		}
	})
	t.Run("PFW EnableNbiot", func(t *testing.T) {
		p := DefaultSetConfigPayload(now)
		p.ForceCloseNBModule = intPtr(0x0) // 0 是合法值，intPtr 保证不被 omitempty 吞掉
		frame, err := buildSetConfigFrame("PFW", p)
		if err != nil {
			t.Fatalf("buildSetConfigFrame: %v", err)
		}
		got := hex.EncodeToString(frame)
		if len(got) < 4 || got[:4] != "01f8" {
			t.Errorf("hex = %q, want prefix \"01f8\"", got)
		}
		var m map[int]int
		if err := cbor.Unmarshal(frame[2:], &m); err != nil {
			t.Fatalf("cbor.Unmarshal payload: %v", err)
		}
		v, ok := m[10]
		if !ok {
			t.Fatal("tag 10 缺失：EnableNbiot 的 0 值被 omitempty 吞掉了")
		}
		if v != 0 {
			t.Errorf("tag 10 = %d, want 0", v)
		}
	})
}

// TestParseStatus 校验读响应解析：hex 化后跳过前 2 字节帧头，剩余 CBOR
// 按数字 tag 映射到 Status；非法字节返回 nil。
func TestParseStatus(t *testing.T) {
	payload, err := cbor.Marshal(map[int]interface{}{
		25: "0ba6dc07dfcb", // Sn
		4:  -65,            // NbRssi
		5:  10,             // NbSnr
		8:  0,              // SoftVer
		26: 20,             // DN
		16: 355,            // Voltage
		23: 0,              // ValveState
	})
	if err != nil {
		t.Fatalf("cbor.Marshal: %v", err)
	}
	raw := append([]byte{0x05, 0xf9}, payload...)

	st := parseStatus(raw)
	if st == nil {
		t.Fatal("parseStatus returned nil for valid frame")
	}
	if st.Sn != "0ba6dc07dfcb" {
		t.Errorf("Sn = %q, want %q", st.Sn, "0ba6dc07dfcb")
	}
	if st.NbRssi != -65 {
		t.Errorf("NbRssi = %d, want -65", st.NbRssi)
	}
	if st.NbSnr != 10 {
		t.Errorf("NbSnr = %d, want 10", st.NbSnr)
	}
	if st.SoftVer != 0 {
		t.Errorf("SoftVer = %d, want 0", st.SoftVer)
	}
	if st.DN != 20 {
		t.Errorf("DN = %d, want 20", st.DN)
	}
	if st.Voltage != 355 {
		t.Errorf("Voltage = %d, want 355", st.Voltage)
	}
	if st.ValveState != 0 {
		t.Errorf("ValveState = %d, want 0", st.ValveState)
	}

	if got := parseStatus([]byte{0xff, 0xff}); got != nil {
		t.Errorf("parseStatus(invalid) = %+v, want nil", got)
	}
	if got := parseStatus(nil); got != nil {
		t.Errorf("parseStatus(nil) = %+v, want nil", got)
	}
}

// TestSetConfigOK 校验 SetConfig 成功判定：响应 hex 匹配 05f8bf0000ff
// 或 05f9bf0000ff（大小写不敏感）。
func TestSetConfigOK(t *testing.T) {
	cases := []struct {
		raw  []byte
		want bool
	}{
		{[]byte{0x05, 0xf9, 0xbf, 0x00, 0x00, 0xff}, true},
		{[]byte{0x05, 0xf8, 0xbf, 0x00, 0x00, 0xff}, true},
		{[]byte{0x05, 0xF9, 0xBF, 0x00, 0x00, 0xFF}, true},
		{[]byte{0x05, 0xf8, 0xbf, 0x00, 0x00, 0xfe}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := setConfigOK(tc.raw); got != tc.want {
			t.Errorf("setConfigOK(%x) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestSetDevType 校验 SetDevType 记录设备类型（同包访问未导出字段）。
func TestSetDevType(t *testing.T) {
	d := NewRealDevice()
	d.SetDevType("PSAV")
	d.mu.Lock()
	got := d.devType
	d.mu.Unlock()
	if got != "PSAV" {
		t.Errorf("devType = %q, want %q", got, "PSAV")
	}

	m := NewMockDevice()
	m.SetDevType("PFW")
	m.mu.Lock()
	gotM := m.devType
	m.mu.Unlock()
	if gotM != "PFW" {
		t.Errorf("mock devType = %q, want %q", gotM, "PFW")
	}
}

// TestDisconnectMock 校验 mock Disconnect：清连接状态且幂等。
func TestDisconnectMock(t *testing.T) {
	d := NewMockDevice()
	const id = "0ba6dc07dfcb"
	if err := d.Connect(context.Background(), id); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !d.IsConnected(ParseIdToMac(id)) {
		t.Fatal("IsConnected should be true before Disconnect")
	}
	if err := d.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if d.IsConnected(ParseIdToMac(id)) {
		t.Error("IsConnected should be false after Disconnect")
	}
	// 幂等：已断开再次 Disconnect 返回 nil。
	if err := d.Disconnect(); err != nil {
		t.Errorf("second Disconnect should be no-op, got %v", err)
	}
}

// TestDisconnectMockKeepsStatus 校验 mock Disconnect 不破坏 mock 状态数据。
func TestDisconnectMockKeepsStatus(t *testing.T) {
	d := NewMockDevice()
	const id = "0ba6dc07dfcb"
	if err := d.Connect(context.Background(), id); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	d.SetMockStatus(Status{Sn: "injected", NbRssi: -70})
	if err := d.Disconnect(); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	st := d.Read(context.Background())
	if st == nil {
		t.Fatal("Read returned nil after Disconnect")
	}
	if st.Sn != "injected" {
		t.Errorf("Read Sn = %q, want injected (mock status preserved)", st.Sn)
	}
}

// fakeAdvPayload 是用于测试 matchesBluetoothScanResult 的最小 AdvertisementPayload
// 实现：仅 LocalName 有意义，其余方法返回零值（对齐 Perl export_test.go
// 的 testAdvertisementPayload）。
type fakeAdvPayload struct {
	localName string
}

func (p fakeAdvPayload) LocalName() string                            { return p.localName }
func (p fakeAdvPayload) ManufacturerData() []bt.ManufacturerDataElement { return nil }
func (p fakeAdvPayload) HasServiceUUID(bt.UUID) bool                  { return false }
func (p fakeAdvPayload) ServiceUUIDs() []bt.UUID                      { return nil }
func (p fakeAdvPayload) Bytes() []byte                                { return nil }
func (p fakeAdvPayload) ServiceData() []bt.ServiceDataElement         { return nil }

// TestMatchesBluetoothScanResult 校验 Scan 回调匹配逻辑（对应 Perl 7b1f4349+
// acc5c47f）：广播地址大写后等于 target 或 legacy，或广播名大写后等于 target。
//
// 注：tinygo MAC.UnmarshalText 仅接受大写 A-F（mac.go:22-28），Address.Set
// 对小写输入静默返回错误（MAC 留为 00:00:00:00:00:00），String() 经
// hexDigit 常量强制输出大写——所以对 Address 字段的大小写不敏感主要由我们
// 的 ToUpper 提供兜底；这里只断言可观察到的行为（大写形式匹配/不匹配）。
func TestMatchesBluetoothScanResult(t *testing.T) {
	const target = "FC:11:F4:61:38:83" // 新格式（ParseIdToMac 输出）
	const legacy = "FC:E8:92:61:38:83" // 兼容格式（ParseIdToMacOld 输出）

	addr := bt.Address{}
	addr.Set(target)
	if !matchesBluetoothScanResult(bt.ScanResult{Address: addr}, target, legacy) {
		t.Error("exact-address match should return true")
	}
	// legacy 地址命中
	addrLegacy := bt.Address{}
	addrLegacy.Set(legacy)
	if !matchesBluetoothScanResult(bt.ScanResult{Address: addrLegacy}, target, legacy) {
		t.Error("legacy address match should return true (≤58 固件兼容)")
	}
	// target 在广播名里
	addrOther := bt.Address{}
	addrOther.Set("AA:BB:CC:DD:EE:FF")
	if !matchesBluetoothScanResult(bt.ScanResult{
		Address:              addrOther,
		AdvertisementPayload: fakeAdvPayload{localName: target},
	}, target, legacy) {
		t.Error("LocalName matching target (exact case) should return true")
	}
	// target 小写在广播名里：acc5c47f 起两侧都 ToUpper，应命中
	if !matchesBluetoothScanResult(bt.ScanResult{
		Address:              addrOther,
		AdvertisementPayload: fakeAdvPayload{localName: strings.ToLower(target)},
	}, target, legacy) {
		t.Error("LocalName matching target (case-insensitive after acc5c47f) should return true")
	}
	// 都不匹配
	if matchesBluetoothScanResult(bt.ScanResult{
		Address:              addrOther,
		AdvertisementPayload: fakeAdvPayload{localName: "other-device"},
	}, target, legacy) {
		t.Error("non-matching LocalName should return false")
	}
	// 都不匹配（地址无关，名字也无关）
	addrNo := bt.Address{}
	addrNo.Set("11:22:33:44:55:66")
	if matchesBluetoothScanResult(bt.ScanResult{
		Address:              addrNo,
		AdvertisementPayload: fakeAdvPayload{localName: "nothing"},
	}, target, legacy) {
		t.Error("non-matching all fields should return false")
	}
	// 空 AdvertisementPayload 不应 panic 也不应命中（仅靠地址匹配）
	if matchesBluetoothScanResult(bt.ScanResult{Address: addrNo}, target, legacy) {
		t.Error("non-matching address with nil payload should return false (no panic)")
	}
}

// TestIsRetryableGattDiscoveryError 校验 retry 错误分类（对齐 Perl export.go
// isRetryableGattDiscoveryError）。
func TestIsRetryableGattDiscoveryError(t *testing.T) {
	if !isRetryableGattDiscoveryError(errors.New("could not retrieve device services, operation failed with code 1")) {
		t.Error("WinRT 服务获取失败(code 1)应可重试")
	}
	if !isRetryableGattDiscoveryError(errors.New("bluetooth: did not find all requested characteristic")) {
		t.Error("特征发现不完整应可重试")
	}
	if !isRetryableGattDiscoveryError(errors.New("service discovery failed: timeout after 16s")) {
		t.Error("本包发现超时应可重试")
	}
	if !isRetryableGattDiscoveryError(errors.New("characteristics discovery failed: timeout after 5s")) {
		t.Error("本包特征超时应可重试")
	}
	if !isRetryableGattDiscoveryError(errors.New("async operation failed with status 2")) {
		t.Error("WinRT async status 2 应可重试")
	}
	if !isRetryableGattDiscoveryError(errors.New("previous GATT discovery is still running: restart the process")) {
		t.Error("hung 标识串应可重试")
	}
	if isRetryableGattDiscoveryError(nil) {
		t.Error("nil 不应可重试")
	}
	if isRetryableGattDiscoveryError(errors.New("随便一个不相关的错误")) {
		t.Error("不相关错误不应可重试")
	}
}

// TestMinDuration 校验 minDuration。
func TestMinDuration(t *testing.T) {
	if got := minDuration(2*time.Second, 5*time.Second); got != 2*time.Second {
		t.Errorf("minDuration(2s,5s)=%v, want 2s", got)
	}
	if got := minDuration(7*time.Second, 3*time.Second); got != 3*time.Second {
		t.Errorf("minDuration(7s,3s)=%v, want 3s", got)
	}
	if got := minDuration(4*time.Second, 4*time.Second); got != 4*time.Second {
		t.Errorf("minDuration(4s,4s)=%v, want 4s", got)
	}
}

// TestGattHungFailFast 验证发现 hung 后 execCall 立即返回 errGattHung：
// reset hung flag first to ensure a clean state, then poison it manually,
// then exercise execCall to confirm fail-fast; finally reset.
func TestGattHungFailFast(t *testing.T) {
	// reset
	gattMu.Lock()
	gattHung = 0
	gattMu.Unlock()
	t.Cleanup(func() {
		gattMu.Lock()
		gattHung = 0
		gattMu.Unlock()
	})

	// 未 hung：execCall 不应返回 errGattHung
	d := NewRealDevice()
	if _, err := d.execCall(context.Background(), func() (any, error) { return "ok", nil }); err != nil {
		if errors.Is(err, errGattHung) {
			t.Fatalf("未 hung 时 execCall 不应返回 errGattHung, got %v", err)
		}
	}

	// poison
	gattMu.Lock()
	gattHung = 1
	gattMu.Unlock()

	if _, err := d.execCall(context.Background(), func() (any, error) {
		t.Fatal("hung 后 fn 不应被执行")
		return nil, nil
	}); !errors.Is(err, errGattHung) {
		t.Fatalf("hung 后 execCall 应返回 errGattHung, got %v", err)
	}
}

// scanLogger 记录 logger 收到的 (category, msg)，供 debug 日志断言使用。
type scanLogger struct {
	msgs []string
}

func (l *scanLogger) Info(category, msg string) { l.msgs = append(l.msgs, msg) }

// TestSetDebug 校验 SetDebug 记录 debug 标志（同包访问未导出字段）。
func TestSetDebug(t *testing.T) {
	d := NewRealDevice()
	d.mu.Lock()
	off := d.debug
	d.mu.Unlock()
	if off {
		t.Error("debug should default to false")
	}
	d.SetDebug(true)
	d.mu.Lock()
	on := d.debug
	d.mu.Unlock()
	if !on {
		t.Error("debug should be true after SetDebug(true)")
	}
}

// TestLogScanResultDebug 验证 scanAndConnect 的调试日志行为：
//   - debug=false 时静默（不打印任何扫描到的设备）；
//   - debug=true 时打印扫描到的设备 MAC（+广播名），name 为空时只打 MAC。
func TestLogScanResultDebug(t *testing.T) {
	const mac = "FC:E8:92:61:38:83"

	d := NewRealDevice()
	log := &scanLogger{}
	d.SetLogger(log)

	// debug=false：静默
	d.logScanResult(mac, "BLAT-001")
	if len(log.msgs) != 0 {
		t.Fatalf("debug=false 不应打印扫描日志, 实际: %#v", log.msgs)
	}
	d.logDebug("不应出现")
	if len(log.msgs) != 0 {
		t.Fatalf("debug=false logDebug 不应打印, 实际: %#v", log.msgs)
	}

	// debug=true + 广播名：MAC 与 name 都打印
	d.SetDebug(true)
	d.logScanResult(mac, "BLAT-001")
	if len(log.msgs) != 1 {
		t.Fatalf("debug=true 应打印 1 条扫描日志, 实际: %#v", log.msgs)
	}
	got := log.msgs[0]
	if !strings.Contains(got, mac) || !strings.Contains(got, "BLAT-001") {
		t.Errorf("扫描日志应含 MAC 与广播名, 实际: %q", got)
	}

	// debug=true + 无广播名：只打 MAC，不带 name=
	d.logScanResult("AA:BB:CC:DD:EE:FF", "")
	if len(log.msgs) != 2 {
		t.Fatalf("应累计 2 条扫描日志, 实际: %#v", log.msgs)
	}
	got2 := log.msgs[1]
	if !strings.Contains(got2, "AA:BB:CC:DD:EE:FF") {
		t.Errorf("扫描日志应含 MAC, 实际: %q", got2)
	}
	if strings.Contains(got2, "name=") {
		t.Errorf("广播名为空时不应输出 name=, 实际: %q", got2)
	}

	// debug=true：logDebug 打印
	d.logDebug("蓝牙扫描开始")
	if len(log.msgs) != 3 {
		t.Fatalf("debug=true logDebug 应打印, 实际: %#v", log.msgs)
	}
}
