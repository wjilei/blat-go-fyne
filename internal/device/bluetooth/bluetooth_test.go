package bluetooth

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// TestParseIdToMac 校验 ParseIdToMac 与 Perl BLAT::Common::Utils::parseIdToMac
// 的输出一致（期望值用系统 Perl 独立复现 _id2MacArray/_hashTimes33 验证）。
//
//	_id2MacArray：id 从末尾起每 2 字符一组 hex 填 16 数组（高位补 0）
//	_hashTimes33：djb2/times33（hash = hash*33 + v，起始 5381），
//	              遍历全部 16 个元素（含补零位），取哈希低 24 位
//
// 覆盖偶数长度、全数字串、以及奇数长度（Perl substr 负数 offset 从尾部
// 倒数、越界截断）三种情况。
func TestParseIdToMac(t *testing.T) {
	cases := []struct {
		id string
		want string
	}{
		{"0ba6dc07dfcb", "FC:E8:92:61:38:83"},
		{"262601300011", "FC:E8:92:9F:9E:B3"},
		{"00", "FC:E8:92:CB:7F:05"},
		{"abc", "FC:E8:92:2C:75:4D"}, // 奇数长度：末位单独取最后一个字符
	}
	for _, tc := range cases {
		if got := ParseIdToMac(tc.id); got != tc.want {
			t.Errorf("ParseIdToMac(%q) = %s, want %s", tc.id, got, tc.want)
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

// TestScanResultMatches 校验 Scan 回调的匹配逻辑（对应 Perl BLAT
// BlueToothScanAndConnect：地址大小写不敏感匹配，或广播名精确匹配）。
func TestScanResultMatches(t *testing.T) {
	cases := []struct {
		name   string
		target string
		addr   string
		bname  string
		want   bool
	}{
		{"地址完全匹配", "FC:E8:92:61:38:83", "FC:E8:92:61:38:83", "", true},
		{"地址大小写不敏感", "FC:E8:92:61:38:83", "fc:e8:92:61:38:83", "", true},
		{"广播名匹配", "my-device", "AA:BB:CC:DD:EE:FF", "my-device", true},
		{"广播名大小写敏感(Perl 语义)", "my-device", "AA:BB:CC:DD:EE:FF", "My-Device", false},
		{"都不匹配", "FC:E8:92:61:38:83", "FC:E8:92:61:38:84", "other", false},
		{"空目标不匹配", "", "FC:E8:92:61:38:83", "x", false},
	}
	for _, tc := range cases {
		if got := scanResultMatches(tc.target, tc.addr, tc.bname); got != tc.want {
			t.Errorf("%s: scanResultMatches(%q, %q, %q) = %v, want %v",
				tc.name, tc.target, tc.addr, tc.bname, got, tc.want)
		}
	}
}
