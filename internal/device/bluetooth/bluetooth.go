// Package bluetooth provides a business-level Bluetooth device for the
// Heat suite. It mirrors the Perl BLAT::Device::bluetooth OOP style:
// methods hang directly off *Device instead of going through the low
// level core.Device Command protocol.
//
// It intentionally does NOT implement core.Device (Open/Close/Command):
// it lives above the low level driver layer and is injected into
// env.Devs["bluetooth"] by the application, then type-asserted by the
// Case that owns it.
//
// All methods take a context so a cancelled run (toolbar Stop) never
// hangs inside a mock sleep; the mock itself is instant, but the
// signatures stay ctx-aware for the real implementation.
//
// Two operating modes:
//
//   - mock（NewMockDevice / NewDevice）：内存假数据，无硬件可跑。
//   - real（NewRealDevice）：tinygo.org/x/bluetooth 真实 BLE。
//     大多数 BLE 调用（Enable/Connect/Scan/Read/Write）通过专用串行
//     executor goroutine 依次执行，规避 Windows 上从非主线程/并发调用
//     导致的崩溃（tinygo issue #294）。GATT 服务/特征发现（DiscoverServices
//     / DiscoverCharacteristics）单独包在 gate + 超时 goroutine 中，因为
//     Windows 的 WinRT 这两个调用存在卡死风险：门闸互斥 + 超时熔断 +
//     重试对齐 Perl BLAT goExtTools 7b1f4349 起的实现。卡死后所有 BLE
//     入口快速失败并提示重启进程（Windows 无法在进程内复位 WinRT 栈）。
package bluetooth

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fxamacker/cbor/v2"
	bt "tinygo.org/x/bluetooth"
)

// GATT UUID 常量，对应 Perl FindCharacteristicToOperator(0xfff0, 0xfff2)：
// 服务 0xfff0、特征 0xfff2（读写同一特征）。
const (
	serviceUUID16 = 0xfff0
	charUUID16    = 0xfff2
)

// errNotConnected is returned when direct Connect + scan fallback both fail.
var errNotConnected = errors.New("蓝牙连接失败")

// errScanTimeout 是 Scan 兜底连接超时（15s，对应 Perl BLAT
// ConnectBle -> ScanAndConnect($ble_id, 15) 的扫描超时）。
var errScanTimeout = errors.New("蓝牙扫描超时")

// 真实模式使用的辅助错误。
var (
	errNoService        = errors.New("蓝牙未发现服务")
	errNoCharacteristic = errors.New("蓝牙未发现特征")
	errNotifyEmpty      = errors.New("蓝牙通知为空")
	errNotifyTimeout    = errors.New("等待设备通知超时")
	errSetConfigNack    = errors.New("蓝牙配置响应异常")
)

// GATT 发现时间/重试常量（对齐 Perl export.go 7b1f4349 起：
// 升级 service 发现总预算 5s→16s，引入 service/characteristic 重试与退避）。
const (
	scanTimeout                       = 30 * time.Second
	serviceDiscoveryTimeout           = 16 * time.Second
	characteristicDiscoveryTimeout    = 5 * time.Second
	gattDiscoveryGateTimeout          = 5 * time.Second
	gattDiscoveryRetryDelay           = 250 * time.Millisecond
	serviceDiscoveryRetryCount        = 3
	characteristicDiscoveryRetryCount = 8
)

// GATT 发现进程级门闸与 hung 状态。Windows 的 WinRT GATT 发现存在“卡死”
// 风险（discoverServices/Characteristics 永不返回），Perl 工程为处理该风险
// 在所有发现调用外加全局门闸（容量 1）+ hung 标记：超时的发现继续持有门闸
// 直到 tinygo 真正返回；后续 acquire 超时则把进程标记为 hung，之后所有 BLE
// 入口立即返回 errGattHung 并提示用户重启进程复位 BLE 栈。
//
// 我们沿用该设计：因为 3 工位共享 DefaultAdapter，且现有 executor 串行不
// 保证发现返回（卡死时 executor 也会卡死），门闸互斥 + hung 熔断是必要的。
var (
	gattGate      = make(chan struct{}, 1)
	gattMu        sync.Mutex
	gattNextID    uint64
	gattCurrentID uint64
	gattHung      uint64
)

// errGattHung 在发现 hung 后被所有 BLE 入口返回；对应 Perl 的
// "previous GATT discovery is still running; restart the process to
// reset the Bluetooth stack"。Windows 上无法在进程内复位 WinRT 栈。
var errGattHung = errors.New("GATT 发现已卡死（previous GATT discovery is still running）；请重启程序复位蓝牙栈")

// Logger 是 bluetooth 包可选的日志输出接口（与 core.Logger 的 Info 兼容，
// 不直接依赖 core 避免引入包级耦合）。nil 时静默跳过日志。
//
// Phase 2 A：跟随 core.Logger 升级为 (category, msg)。SetLogger 通常收到
// env.Log（core.Logger 接口值），签名不一致会破坏透传，故必须同步。
type Logger interface {
	Info(category, msg string)
}

// Status mirrors the Perl BluetoothRead result fields.
type Status struct {
	ForceCloseNB int
	NbRssi       int
	NbSnr        int
	SoftVer      int
	Sn           string
	DN           int
	Voltage      int
	Flow         int
	InTemp       int
	Err          int
	ValveState   int // 0 成功 / 1 失败 / 3 reset
	Timestamp    int
	BlVer        int
}

// Device is a business-level Bluetooth device. The default mock data is
// chosen so the PSAV flow of wire_valve_bluetooth_test_all_params passes
// on the first read (NbRssi>=−81 → NB ok, ValveState=0 → 阀门状态正确).
//
// mock==true 时走内存 mock；mock==false 时走 tinygo 真实 BLE 模式。
type Device struct {
	mu        sync.Mutex
	connected bool
	mac       string
	// id 是 Connect 传入的设备序列号（HeatNote.mac）。真实流程中 BLE 连接
	// 地址由 ParseIdToMac(id) 派生，而设备读回的 Sn 是原始序列号 id，故
	// mock 默认 Sn 使用 id 而非连接地址 mac。
	id string

	// readCount counts Read() calls. The mock keeps ForceCloseNB=0 by
	// default so the pass path is taken; a test can SetMockStatus with
	// ForceCloseNB!=0 / NbRssi=0,NbSnr=0 to exercise the NB branches.
	readCount int

	// connectedSinceReboot records the last Reboot() time; used by the
	// ResetValve mock to restore a clean ValveState.
	connectedSinceReboot time.Time

	// openingPre 记录最近一次 SetValveOpening 调用写入的开度（0-100）。
	// 仅 mock 用于断言；real 模式走 setConfigReal 不依赖该字段。零值表示
	// 尚未调用，默认初始值视为未设置。
	openingPre int

	status Status // mock 数据，SetMockStatus 可注入测试场景

	// mock 为 true 时走内存 mock 路径；false 时走 tinygo 真实 BLE 路径。
	mock bool

	// ---- real 模式字段（mock==false 时使用）----
	// adapter 是 tinygo 系统默认 BLE 适配器（Windows 上为 DefaultAdapter）。
	adapter *bt.Adapter
	// tinyDev 保存 Adapter.Connect 返回的远端设备引用；nil 表示未连接。
	// 只在串行 executor goroutine 内读写。
	tinyDev *bt.Device
	// devType 是设备类型（"PSAV"/"PFW"/""），由 SetDevType 设置，决定协议
	// 帧头字节（PSAV → f9，其它 → f8）。
	devType string
	// writeChar 缓存首次通信时发现的读写特征（对应 Perl 缓存的 BleCharac），
	// 之后复用；charCached 标记是否已缓存（DeviceCharacteristic 跨平台结构
	// 不同，不用零值比较判断）。
	writeChar  bt.DeviceCharacteristic
	charCached bool
	// notifOnce 保证 EnableNotifications 每个连接生命周期只做一次。
	notifOnce sync.Once
	// notifCh 接收 EnableNotifications 回调推送的原始字节（chan []byte, 10）。
	notifCh chan []byte

	// logger 是可选日志输出（SetLogger 注入，通常为 core.Logger 的 Info）。
	// nil 时各成功日志静默跳过。
	logger Logger

	// debug 为 true 时（--debug）额外打印扫描到的每一个广播地址（MAC）与
	// 广播名，便于排查"扫描到但没匹配上"的连接失败。默认 false，保持与
	// Perl 原版一致的日志量；由 SetDebug 设置。
	debug bool

	// enableOnce 保证 adapter.Enable() 只执行一次：Windows 上重复
	// RoInitialize 会返回 S_FALSE（0x1），go-ole 会把它当错误抛出。
	enableOnce sync.Once

	// exec 是真实 BLE 调用的专用串行 executor：所有 tinygo 调用
	// （Enable/Connect/Discover/Read/Write）投递到该 chan，由单 goroutine
	// 依次执行，方法侧用应答 channel 同步等待结果，遵守 ctx 取消。
	exec     chan func()
	execInit sync.Once
}

// NewDevice returns a real BLE device（等价 NewRealDevice），保持向后兼容
// （main 现有两处调用不变）。蓝牙默认走真实 BLE；需要无硬件调试时用
// NewMockDevice。
func NewDevice() *Device {
	return NewRealDevice()
}

// NewMockDevice 返回 mock 模式设备：所有调用走内存数据，无需硬件。
func NewMockDevice() *Device {
	return &Device{
		mock: true,
		status: Status{
			ForceCloseNB: 0,
			NbRssi:       -65,
			NbSnr:        10,
			SoftVer:      0,
			DN:           20,
			Voltage:      355,
			Flow:         0,
			InTemp:       25,
			Err:          0,
			ValveState:   0,
			Timestamp:    5,
			BlVer:        1,
		},
	}
}

// NewRealDevice 返回真实 tinygo BLE 模式设备：所有 BLE 调用经专用串行
// executor 串行执行。GATT 服务/特征用常量 0xfff0/0xfff2（对应 Perl BLAT
// FindCharacteristicToOperator）。
func NewRealDevice() *Device {
	return &Device{
		mock:    false,
		adapter: bt.DefaultAdapter,
		notifCh: make(chan []byte, 10),
	}
}

// IsReal reports whether the device runs in real tinygo BLE mode
// (mock==false). 供 case 端判断 env.Devs 中的实例是否为真实设备。
func (d *Device) IsReal() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.mock
}

// SetMockStatus injects a status for the next Read() calls. Sn stays
// empty to default to the connected mac.
func (d *Device) SetMockStatus(s Status) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status = s
}

// SetDevType 设置设备类型，决定协议帧头字节（PSAV → f9，其它 → f8）。
// case 端在 Run 里用 Configure 得到的设备类型调用；mock 模式同样记录。
func (d *Device) SetDevType(t string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.devType = t
}

// SetLogger 注入日志输出（通常传 env.Log 的 Info）。nil 时各成功日志静默跳过。
func (d *Device) SetLogger(l Logger) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logger = l
}

// SetDebug 设置 --debug 调试模式：true 时 scanAndConnect 会把扫描到的每个
// 广播设备（MAC + 广播名）打到 logger，便于排查扫描匹配失败；false 时只
// 打印命中目标后的日志（与 Perl 原版一致）。与 mbus.Device.SetDebug 对齐。
func (d *Device) SetDebug(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.debug = on
}

// logDebug 仅在 debug 模式（--debug）下把 msg 打到 logger；否则静默。
func (d *Device) logDebug(msg string) {
	d.mu.Lock()
	on := d.debug
	d.mu.Unlock()
	if on {
		d.logInfo(msg)
	}
}

// logScanResult 在 debug 模式下打印一个扫描到的广播设备；非 debug 静默。
// name 为空时只打 MAC。
func (d *Device) logScanResult(addr, name string) {
	if name == "" {
		d.logDebug(fmt.Sprintf("蓝牙扫描到设备: %s", addr))
		return
	}
	d.logDebug(fmt.Sprintf("蓝牙扫描到设备: %s (name=%s)", addr, name))
}

// logInfo 输出一条日志；logger 为 nil 时静默跳过。蓝牙设备日志不带分类
// （空 category），保持与 case 端一致。
func (d *Device) logInfo(msg string) {
	d.mu.Lock()
	l := d.logger
	d.mu.Unlock()
	if l != nil {
		l.Info("", msg)
	}
}

// IsConnected reports whether the device is connected to mac. real 模式
// 同样基于连接状态字段（connected/mac），不触发 BLE 调用。
func (d *Device) IsConnected(mac string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected && d.mac == mac
}

// Connect ensures the device is connected to the BLE address derived from
// the device serial number id. It mirrors the Perl flow of
// parseIdToMac(HeatNote.mac) + _ensure_bluetooth_connected:
// already-connected check skips the connect, otherwise it retries at most
// twice (Perl IsBleConnected / ConnectBle pair). The raw id is recorded so
// the mock can return it as the device Sn.
func (d *Device) Connect(ctx context.Context, id string) error {
	mac := ParseIdToMac(id)
	if d.IsConnected(mac) {
		return nil
	}
	if d.mock {
		return d.mockConnect(ctx, id, mac)
	}
	return d.realConnect(ctx, id, mac)
}

// Disconnect 断开蓝牙并清空连接状态，幂等（已断开时返回 nil）。
// mock：清 connected/id/mac 连接字段，保持 mock 状态数据不变。
// real：经 execCall 调 tinyDev.Disconnect()，然后清 tinyDev/connected/id/mac，
// 同时重置 notifOnce/writeChar/notifCh。Disconnect 不带 ctx（对应 BLAT
// release/disconnect），内部用 context.Background() 投递 executor。
func (d *Device) Disconnect() error {
	if d.mock {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.connected = false
		d.id = ""
		d.mac = ""
		return nil
	}
	_, err := d.execCall(context.Background(), func() (any, error) {
		d.mu.Lock()
		dev := d.tinyDev
		d.mu.Unlock()
		if dev == nil {
			// 已断开，幂等返回。
			return nil, nil
		}
		return nil, dev.Disconnect()
	})
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.tinyDev = nil
	d.connected = false
	d.id = ""
	d.mac = ""
	d.writeChar = bt.DeviceCharacteristic{}
	d.charCached = false
	d.notifOnce = sync.Once{}
	d.notifCh = make(chan []byte, 10)
	d.mu.Unlock()
	return nil
}

func (d *Device) mockConnect(ctx context.Context, id, mac string) error {
	for i := 0; i < 2; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := d.doConnect(id, mac); err == nil {
			return nil
		}
	}
	return errNotConnected
}

func (d *Device) doConnect(id, mac string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.connected = true
	d.id = id
	d.mac = mac
	return nil
}

// realConnect 在真实模式下建立 BLE 连接。流程对齐 Perl 7b1f4349
// BlueToothScanAndConnect：
//  1. 派生新格式 MAC（ParseIdToMac）+ legacy 兼容 MAC；
//  2. 用新 MAC 直接 Connect（带 ConnectionTimeout=serviceDiscoveryTimeout）；
//  3. 失败则进入 scanAndConnect 扫描兜底（同时匹配新 MAC 与 legacy MAC，
//     legacy 用于兼容 ≤58 固件的旧设备）；
//  4. 全部失败返回 errNotConnected。
//
// 连接动作必须在串行 executor goroutine 内执行；Perl 的外层 2 次重试已
// 由“直接连接 + 扫描”两阶段取代，故此函数不做额外外层循环。
func (d *Device) realConnect(ctx context.Context, id, newMac string) error {
	// ParseMAC 是纯内存计算，不需要进 executor。
	macAddr, err := bt.ParseMAC(newMac)
	if err != nil {
		return errNotConnected
	}
	addr := bt.Address{MACAddress: bt.MACAddress{MAC: macAddr}}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	_, err = d.execCall(ctx, func() (any, error) {
		return nil, d.realConnectLocked(addr, id, newMac)
	})
	if err == nil {
		return nil
	}
	return err
}

// realConnectLocked 必须在串行 executor goroutine 内调用。
// 连接流程对齐 Perl BLAT goExtTools 的 BlueToothScanAndConnect：
//  1. adapter.Enable（只做一次，由 enableOnce 守卫）；
//  2. 用新格式 MAC 直接 Connect（带 ConnectionTimeout）；
//  3. 失败则 adapter.Scan 广播扫描，回调里按（新/旧）地址或大写广播名
//     匹配目标，命中后 StopScan 并把扫描到的地址回传；
//  4. 用扫描到的地址再次 Connect。
func (d *Device) realConnectLocked(addr bt.Address, id, newMac string) error {
	var enableErr error
	d.enableOnce.Do(func() { enableErr = d.adapter.Enable() })
	if enableErr != nil {
		return enableErr
	}
	dev, err := d.adapter.Connect(addr, bt.ConnectionParams{
		ConnectionTimeout: bt.NewDuration(serviceDiscoveryTimeout),
	})
	if err == nil {
		d.recordConnect(dev, id, newMac)
		return nil
	}
	// 直接连接失败 → 扫描兜底（同时匹配新/旧 MAC）
	dev, err = d.scanAndConnect(newMac, legacyMacFrom(newMac))
	if err != nil {
		return err
	}
	d.recordConnect(dev, id, newMac)
	return nil
}

// recordConnect 记录已连接的 tinygo 设备引用与业务 id/mac。
// 必须在持有连接结果后调用。
func (d *Device) recordConnect(dev bt.Device, id, mac string) {
	d.mu.Lock()
	d.tinyDev = &dev
	d.connected = true
	d.id = id
	d.mac = mac
	d.mu.Unlock()
	d.logInfo(fmt.Sprintf("蓝牙连接成功: %s", mac))
}

// scanAndConnect 广播扫描目标设备并连接，对齐 Perl BLAT 7b1f4349+acc5c47f：
//   - adapter.Scan 回调中按（uppercased）地址或 legacy 地址匹配，或 uppercased
//     广播名匹配新格式目标 MAC；
//   - 命中后用 CompareAndSwap 防止并发匹配时重复发地址 + 重复 StopScan
//     （Perl L323 引入，避免小概率下的双重 connect）；
//   - 收到地址后用该地址 Connect（ConnectionTimeout=serviceDiscoveryTimeout）；
//   - 15s 整体超时未匹配返回 errScanTimeout。
//
// debug 模式（--debug）下额外打印扫描到的每个广播设备 MAC（+广播名）。
//
// 必须在串行 executor goroutine 内调用。
func (d *Device) scanAndConnect(targetMac, legacyMac string) (bt.Device, error) {
	target := strings.ToUpper(targetMac)
	legacy := strings.ToUpper(legacyMac)
	addrChan := make(chan bt.Address, 1)
	scanErrChan := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
	defer cancel()

	d.logDebug(fmt.Sprintf("蓝牙扫描开始（直接连接失败，走扫描兜底），目标地址 %s 兼容地址 %s", target, legacy))

	var scanMatched atomic.Bool
	go func() {
		err := d.adapter.Scan(func(a *bt.Adapter, sr bt.ScanResult) {
			addr := sr.Address.String()
			name := sr.LocalName()
			// debug 模式：打印每个扫描到的设备 MAC（+广播名），便于定位
			// "扫描到但匹配不上" 的序列号/mac 派生问题。
			d.logScanResult(addr, name)
			if !matchesBluetoothScanResult(sr, target, legacy) {
				return
			}
			if !scanMatched.CompareAndSwap(false, true) {
				return
			}
			addrChan <- sr.Address
			if err := a.StopScan(); err != nil {
				d.logInfo(fmt.Sprintf("StopScan 失败: %v", err))
			}
		})
		if err != nil {
			// Perl 7b1f4349 后不再 `&& !scanMatched.Load()` 短路：scanErrChan
			// 已用缓冲 1，重复错误只记录一次。
			select {
			case scanErrChan <- err:
			default:
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// Perl: StopScan 仅在未匹配时记录错误（防止与已发出的 StopScan
			// 双重报错）。
			if err := d.adapter.StopScan(); err != nil && !scanMatched.Load() {
				d.logInfo(fmt.Sprintf("StopScan 失败: %v", err))
			}
			return bt.Device{}, errScanTimeout
		case err := <-scanErrChan:
			_ = d.adapter.StopScan()
			return bt.Device{}, err
		case addr := <-addrChan:
			d.logInfo(fmt.Sprintf("蓝牙扫描成功，发现目标设备 %s，开始连接", addr.String()))
			device, err := d.adapter.Connect(addr, bt.ConnectionParams{
				ConnectionTimeout: bt.NewDuration(serviceDiscoveryTimeout),
			})
			if err != nil {
				return bt.Device{}, err
			}
			return device, nil
		}
	}
}

// matchesBluetoothScanResult 判断扫描结果是否命中目标设备（对齐 Perl
// 7b1f4349+acc5c47f）：
//   - 广播地址大小写不敏感匹配 targetUpperMAC（新格式）或 legacyUpperMAC
//     （≤58 固件的旧 FC:E8:92 格式）；
//   - 广播名**大写**后等于 targetUpperMAC（acc5c47f 起显式大写匹配）。
//
// 旧 target 是小写/混合大小写也可能命中，因为两侧都 ToUpper。广播名匹配
// 仅针对新格式 MAC（Perl 9-13 行注释：legacy 旧设备只在地址层匹配）。
//
// AdvertisementPayload 可能为 nil（嵌入接口字段，测试中可以构造空
// ScanResult；生产由 tinygo 平台层保证非 nil）。这里显式短路 nil 避免
// 在 promoted 方法上 panic。
func matchesBluetoothScanResult(sr bt.ScanResult, targetUpperMAC, legacyUpperMAC string) bool {
	discoveredMAC := strings.ToUpper(sr.Address.String())
	if discoveredMAC == targetUpperMAC || discoveredMAC == legacyUpperMAC {
		return true
	}
	if sr.AdvertisementPayload == nil {
		return false
	}
	return strings.ToUpper(sr.LocalName()) == targetUpperMAC
}

// Reboot restarts the device. Mock always succeeds and records the time
// for the ValveState reset mock. Real: 发送 SetConfig 帧
// {CtrlType:0x5a}（tag 12 → 90）并校验响应 05f[89]bf0000ff。
func (d *Device) Reboot(ctx context.Context) error {
	if d.mock {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.connectedSinceReboot = time.Now()
		return nil
	}
	p := DefaultSetConfigPayload(time.Now())
	p.CtrlType = intPtr(0x5a)
	return d.setConfigReal(ctx, p)
}

// Read returns the current (mock) status, defaulting Sn to the connected
// mac so the 序列号不匹配 check passes without injection.
func (d *Device) Read(ctx context.Context) *Status {
	if d.mock {
		return d.mockRead(ctx)
	}
	return d.realRead(ctx)
}

func (d *Device) mockRead(ctx context.Context) *Status {
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.readCount++
	st := d.status
	if st.Sn == "" {
		// 默认 Sn 用 Connect 传入的原始序列号 id（真实设备读回的 Sn 就是
		// 序列号，而非派生出的 BLE 地址 mac）；id 为空时退回连接地址。
		if d.id != "" {
			st.Sn = d.id
		} else {
			st.Sn = d.mac
		}
	}
	return &st
}

// realRead 真实模式下经串行 executor 构造读取帧（04+devTypeByte+a1 00...）
// 写入蓝牙，等通知数据后解析为 Status（hex 化跳过前 2 字节帧头，剩余 CBOR
// 解码按 tag 映射）。任何错误/空数据返回 nil（对应 Perl undef 路径）。
func (d *Device) realRead(ctx context.Context) *Status {
	if err := ctxErr(ctx); err != nil {
		return nil
	}
	// 未连接直接返回 nil，避免 nil deref；业务侧会报“读取蓝牙失败”。
	if !d.isConnected() {
		return nil
	}
	raw, err := d.execCall(ctx, func() (any, error) {
		d.mu.Lock()
		devType := d.devType
		d.mu.Unlock()
		frame := buildReadFrame(devType)
		return d.writeAndRecv(ctx, frame)
	})
	if err != nil {
		return nil
	}
	rawBytes, _ := raw.([]byte)
	return parseStatus(rawBytes)
}

// EnableNbiot turns the NB-IoT modem on. Mock: no-op.
// Real: 发送 SetConfig 帧 {ForceCloseNBModule:0x0}（tag 10 → 0）。
func (d *Device) EnableNbiot(ctx context.Context) error {
	if d.mock {
		return ctxErr(ctx)
	}
	p := DefaultSetConfigPayload(time.Now())
	p.ForceCloseNBModule = intPtr(0x0) // 0 是合法值，必须用 intPtr 保留
	return d.setConfigReal(ctx, p)
}

// DisableNbiot turns the NB-IoT modem off. Mock: no-op.
// Real: 发送 SetConfig 帧 {ForceCloseNBModule:0xb3}（tag 10 → 179）。
func (d *Device) DisableNbiot(ctx context.Context) error {
	if d.mock {
		return ctxErr(ctx)
	}
	p := DefaultSetConfigPayload(time.Now())
	p.ForceCloseNBModule = intPtr(0xb3)
	return d.setConfigReal(ctx, p)
}

// ResetValve resets the valve and restores ValveState to 0 in the mock.
// Real: 发送 SetConfig 帧 {CtrlType:0x5b}（tag 12 → 91）。
func (d *Device) ResetValve(ctx context.Context) error {
	if d.mock {
		if err := ctxErr(ctx); err != nil {
			return err
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.status.ValveState = 0
		return nil
	}
	p := DefaultSetConfigPayload(time.Now())
	p.CtrlType = intPtr(0x5b)
	return d.setConfigReal(ctx, p)
}

// SetValveOpening 设置阀门开度（0-100 整数百分比）。mock：把 pre 写入
// openingPre 字段供测试断言；real：发送 SetConfig 帧，覆盖 SetOpenPre
// 字段（tag 8），其余字段由 DefaultSetConfigPayload 默认填充。
//
// pre 必须在 [0,100] 范围内（含边界），否则返回错误而不发起任何通信。
// 该方法不带 CtrlType——只设开度，不强制阀门动作，由固件按当前状态解释。
func (d *Device) SetValveOpening(ctx context.Context, pre int) error {
	if pre < 0 || pre > 100 {
		return fmt.Errorf("开度值必须在 0-100 之间: %d", pre)
	}
	if d.mock {
		if err := ctxErr(ctx); err != nil {
			return err
		}
		d.mu.Lock()
		d.openingPre = pre
		d.mu.Unlock()
		return nil
	}
	p := DefaultSetConfigPayload(time.Now())
	p.SetOpenPre = intPtr(pre)
	return d.setConfigReal(ctx, p)
}

// mockOpenPre 返回最近一次 SetValveOpening 写入的开度（仅 mock 模式有
// 意义；real 模式始终返回零值）。同包测试用于断言参数被正确接收。
func (d *Device) mockOpenPre() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.openingPre
}

// setConfigReal 在真实模式下构造 SetConfig 帧（01+devTypeByte+CBOR(p)）
// 写入蓝牙，等通知响应并校验 05f[89]bf0000ff；不匹配返回 errSetConfigNack。
// 须在 executor 内执行 writeAndRecv（BLE 调用串行化）。
func (d *Device) setConfigReal(ctx context.Context, p *SetConfigPayload) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if !d.isConnected() {
		return errNotConnected
	}
	_, err := d.execCall(ctx, func() (any, error) {
		d.mu.Lock()
		devType := d.devType
		d.mu.Unlock()
		frame, err := buildSetConfigFrame(devType, p)
		if err != nil {
			return nil, err
		}
		raw, err := d.writeAndRecv(ctx, frame)
		if err != nil {
			return nil, err
		}
		if !setConfigOK(raw) {
			return nil, errSetConfigNack
		}
		return nil, nil
	})
	return err
}

// isConnected 报告真实模式的连接状态（连接字段为 true 且持有设备引用）。
func (d *Device) isConnected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected && d.tinyDev != nil
}

// ---- 串行 executor ----

// startExec 惰性启动 executor goroutine（幂等，可并发调用）。
func (d *Device) startExec() {
	d.execInit.Do(func() {
		d.exec = make(chan func(), 8)
		go func() {
			for fn := range d.exec {
				fn()
			}
		}()
	})
}

// execCall 把 fn 投递到串行 executor 并同步等待结果，遵守 ctx 取消。
// 返回 fn 的 (值, 错误)。fn 中允许调用任意 tinygo BLE API——它们只在
// 单 goroutine 内串行执行，规避 Windows 并发 BLE 调用崩溃。
//
// 当 GATT 发现已 hung（Windows 上无法在进程内复位 WinRT 栈）时，execCall
// 立即返回 errGattHung 而不投递 fn，避免与仍可能卡在 tinygo 内的 abandoned
// discovery goroutine 产生并发调用。Perl BlueToothScanAndConnect 对所有发现
// 入口做了同样的熔断；我们扩展到所有 BLE 入口（含 Connect/Read/Write）以
// 杜绝交叉并发风险。
func (d *Device) execCall(ctx context.Context, fn func() (any, error)) (any, error) {
	if isGattDiscoveryHung() {
		return nil, errGattHung
	}
	d.startExec()
	reply := make(chan execResult, 1)
	select {
	case d.exec <- func() {
		v, err := fn()
		reply <- execResult{val: v, err: err}
	}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-reply:
		return r.val, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// isGattDiscoveryHung 返回 GATT 发现是否已 hung 熔断（true 时 execCall 拒绝）。
func isGattDiscoveryHung() bool {
	gattMu.Lock()
	defer gattMu.Unlock()
	return gattHung != 0
}

type execResult struct {
	val any
	err error
}

// ---- GATT 发现门闸（对齐 Perl export.go acquireGattDiscoveryGate）----

// acquireGattDiscoveryGate 申请一次 GATT 发现令牌。互斥保证同一时刻只有
// 一个 tinygo DiscoverServices/Characteristics 在跑（避免 Windows 并发 GATT
// 调用崩溃）；hung 状态下立即返回 errGattHung；acquire 超时则把当前持有
// 的 discovery 标记为 hung（Perl L579-580 同语义）。
//
// 必须在 executor 内调用或同步等待前完成 hung 检查（通过 isGattDiscoveryHung
// 守门）。
func acquireGattDiscoveryGate(timeout time.Duration) (uint64, func(), error) {
	gattMu.Lock()
	if gattHung != 0 {
		gattMu.Unlock()
		return 0, nil, errGattHung
	}
	gattMu.Unlock()

	if timeout <= 0 {
		timeout = gattDiscoveryGateTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case gattGate <- struct{}{}:
		gattMu.Lock()
		gattNextID++
		id := gattNextID
		gattCurrentID = id
		gattMu.Unlock()
		return id, func() {
			gattMu.Lock()
			if gattCurrentID == id {
				gattCurrentID = 0
			}
			// 仅清本 id 的 hung；hung 由 markGattDiscoveryHung 显式置位。
			gattMu.Unlock()
			<-gattGate
		}, nil
	case <-timer.C:
		// 门闸 5s 内拿不到：当前持有者大概率卡死（tinygo discover 调用
		// 永不返回），按 Perl 语义 mark 为 hung 永久熔断。
		if markGattDiscoveryHung(0) {
			return 0, nil, fmt.Errorf("previous GATT discovery still running after %s", timeout)
		}
		return 0, nil, errGattHung
	}
}

// markGattDiscoveryHung 标记 GATT 发现 hung（Windows WinRT 卡死）。id>0
// 时仅当其等于当前持有 id 才置位；id==0 用于 acquire 超时路径的无条件置
// 位（Perl markGattDiscoveryHung(id) 返回 false 时代表已被其它协程 reset）。
func markGattDiscoveryHung(id uint64) bool {
	gattMu.Lock()
	defer gattMu.Unlock()
	if id != 0 && gattCurrentID != id {
		return false
	}
	gattHung++
	return true
}

// discoverServices 定位 GATT 服务，带超时与门闸互斥。对齐 Perl export.go
// discoverServices：把 tinygo DiscoverServices 投递到独立 goroutine，select
// 等待结果；超时则 mark hung 并返回 "service discovery failed: timeout
// after %s"（该串被 isRetryableGattDiscoveryError 识别以触发重试）。
//
// 不在 executor 内调用（必须由 findCharacteristicWithRetry 在 executor 内调
// 用）；goroutine 仅承载 tinygo 调用，不做其它共享状态写。
func discoverServices(device *bt.Device, svcUUID uint16, timeout time.Duration) ([]bt.DeviceService, error) {
	if timeout <= 0 {
		timeout = serviceDiscoveryTimeout
	}
	discoveryID, releaseGate, err := acquireGattDiscoveryGate(gattDiscoveryGateTimeout)
	if err != nil {
		return nil, fmt.Errorf("service discovery failed: %w", err)
	}

	resultChan := make(chan struct {
		services []bt.DeviceService
		err      error
	}, 1)
	go func() {
		defer releaseGate()
		svcs, gerr := device.DiscoverServices([]bt.UUID{bt.New16BitUUID(svcUUID)})
		select {
		case resultChan <- struct {
			services []bt.DeviceService
			err      error
		}{svcs, gerr}:
		default:
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	select {
	case r := <-resultChan:
		return r.services, r.err
	case <-ctx.Done():
		// 给 goroutine 一点时间把结果补发到 resultChan，避免与 hung 标记竞态。
		select {
		case r := <-resultChan:
			return r.services, r.err
		default:
		}
		markGattDiscoveryHung(discoveryID)
		return nil, fmt.Errorf("service discovery failed: timeout after %s", timeout)
	}
}

// discoverCharacteristic 定位 GATT 特征，带超时与门闸互斥。对齐 Perl
// export.go discoverCharacteristic：返回切片（空切片 → errNoCharacteristic），
// 超时则 mark hung 并返回 "characteristics discovery failed: timeout
// after %s"（该串被 isRetryableGattDiscoveryError 识别以触发重试）。
func discoverCharacteristic(svc bt.DeviceService, charUUID uint16, timeout time.Duration) ([]bt.DeviceCharacteristic, error) {
	if timeout <= 0 {
		timeout = characteristicDiscoveryTimeout
	}
	discoveryID, releaseGate, err := acquireGattDiscoveryGate(gattDiscoveryGateTimeout)
	if err != nil {
		return nil, fmt.Errorf("characteristics discovery failed: %w", err)
	}

	resultChan := make(chan struct {
		chars []bt.DeviceCharacteristic
		err   error
	}, 1)
	go func() {
		defer releaseGate()
		chars, gerr := svc.DiscoverCharacteristics([]bt.UUID{bt.New16BitUUID(charUUID)})
		select {
		case resultChan <- struct {
			chars []bt.DeviceCharacteristic
			err   error
		}{chars, gerr}:
		default:
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	select {
	case r := <-resultChan:
		if r.err != nil {
			return nil, r.err
		}
		if len(r.chars) == 0 {
			return nil, errNoCharacteristic
		}
		return r.chars, nil
	case <-ctx.Done():
		select {
		case r := <-resultChan:
			if r.err != nil {
				return nil, r.err
			}
			if len(r.chars) == 0 {
				return nil, errNoCharacteristic
			}
			return r.chars, nil
		default:
		}
		markGattDiscoveryHung(discoveryID)
		return nil, fmt.Errorf("characteristics discovery failed: timeout after %s", timeout)
	}
}

// discoverServicesWithRetryUntil 在总预算 deadline 内按
// serviceDiscoveryRetryCount 次数重试服务发现，每次尝试 sleepUntil 退避。
// isRetryableGattDiscoveryError 判定是否值得重试；非可重试错误或达到上限
// 立即退出。错误串来自 Perl export.go 对齐：tinygo v0.15.0 在
// gattc_windows.go:71 / adapter_windows.go:63 / gattc_windows.go:274 仍
// 命中本项目匹配模式。
func discoverServicesWithRetryUntil(device *bt.Device, svcUUID uint16, deadline time.Time) ([]bt.DeviceService, error) {
	var lastErr error
	for attempt := 1; attempt <= serviceDiscoveryRetryCount; attempt++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		svcs, err := discoverServices(device, svcUUID, minDuration(serviceDiscoveryTimeout, remaining))
		if err == nil {
			return svcs, nil
		}
		lastErr = err
		if !isRetryableGattDiscoveryError(err) || attempt == serviceDiscoveryRetryCount {
			break
		}
		time.Sleep(gattDiscoveryRetryDelay * time.Duration(attempt))
		if time.Until(deadline) <= 0 {
			break
		}
	}
	if lastErr == nil {
		lastErr = context.DeadlineExceeded
	}
	return nil, lastErr
}

// findCharacteristicWithRetry 端到端服务+特征发现重试：对齐 Perl
// export.go findCharacteristicWithRetry。
//   - 总预算 deadline = now + serviceDiscoveryTimeout（16s）；
//   - characteristicDiscoveryRetryCount（8）次外层循环；
//   - 每次先 discoverServicesWithRetryUntil（内部再 serviceDiscoveryRetryCount=3
//     次重试），拿到服务后再用剩余时间 discoverCharacteristic；
//   - 可重试错误（isRetryableGattDiscoveryError）按 250ms*attempt 退避后
//     重试；hung/非可重试错误立即返回。
func findCharacteristicWithRetry(device *bt.Device, svcUUID, charUUID uint16) (bt.DeviceCharacteristic, error) {
	var lastErr error
	deadline := time.Now().Add(serviceDiscoveryTimeout)
	for attempt := 1; attempt <= characteristicDiscoveryRetryCount; attempt++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		svcs, err := discoverServicesWithRetryUntil(device, svcUUID, deadline)
		if err != nil {
			lastErr = err
		} else if len(svcs) == 0 {
			lastErr = errNoService
		} else {
			remaining = time.Until(deadline)
			if remaining <= 0 {
				break
			}
			chars, cerr := discoverCharacteristic(svcs[0], charUUID, minDuration(characteristicDiscoveryTimeout, remaining))
			if cerr == nil {
				if len(chars) == 0 {
					lastErr = errNoCharacteristic
				} else {
					return chars[0], nil
				}
			} else {
				lastErr = cerr
			}
		}

		if !isRetryableGattDiscoveryError(lastErr) || attempt == characteristicDiscoveryRetryCount {
			break
		}
		time.Sleep(gattDiscoveryRetryDelay * time.Duration(attempt))
		if time.Until(deadline) <= 0 {
			break
		}
	}
	if lastErr == nil {
		lastErr = context.DeadlineExceeded
	}
	return bt.DeviceCharacteristic{}, lastErr
}

// minDuration 返回两 duration 的较小值。
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// isRetryableGattDiscoveryError 判定 GATT 发现错误是否值得重试。对齐
// Perl export.go isRetryableGattDiscoveryError：错误串来自 tinygo
// v0.15.0 Windows 后端（gattc_windows.go:71 "operation failed with code %d"、
// gattc_windows.go:274 "did not find all requested characteristic"、
// adapter_windows.go:63 "async operation failed with status %d"）；其它
// 两条由本包自身的超时/门闸错误生成。
func isRetryableGattDiscoveryError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "operation failed with code 1") ||
		strings.Contains(msg, "async operation failed with status 2") ||
		strings.Contains(msg, "did not find all requested characteristic") ||
		strings.Contains(msg, "discovery failed: timeout after") ||
		strings.Contains(msg, "previous gatt discovery is still running")
}

// ---- GATT 通信层（均须在串行 executor goroutine 内调用）----

// ensureWriteChar 定位服务（0xfff0）/特征（0xfff2，读写同一特征）并缓存
// writeChar，首次通信时 EnableNotifications 把通知字节推入 d.notifCh。
// 之后复用缓存（对应 Perl 缓存的 BleCharac）。
//
// 服务/特征发现走 findCharacteristicWithRetry（gate + 超时 + 重试）。
// 该函数本身在 executor 内运行（被 writeAndRecv 间接调用），但其内部真
// 正的 tinygo DiscoverServices/Characteristics 调用位于独立 goroutine（被
// 门闸互斥）；详见 discoverServices/discoverCharacteristic。
func (d *Device) ensureWriteChar() error {
	d.mu.Lock()
	cached := d.charCached
	d.mu.Unlock()
	if cached {
		return nil
	}
	if d.tinyDev == nil {
		return errNotConnected
	}
	dev := d.tinyDev
	ch, err := findCharacteristicWithRetry(dev, serviceUUID16, charUUID16)
	if err != nil {
		return err
	}
	// EnableNotifications 只做一次（每个连接生命周期）；回调是 tinygo 内部
	// goroutine 触发，不经过 executor，不会死锁。select+default 防阻塞。
	d.notifOnce.Do(func() {
		d.logInfo("EnableNotifications 开始...")
		if err := ch.EnableNotifications(func(buf []byte) {
			select {
			case d.notifCh <- buf:
			default:
				// 缓冲满丢弃（writeAndRecv 每次读一条，正常不应发生）
			}
		}); err != nil {
			d.logInfo(fmt.Sprintf("EnableNotifications 失败: %v", err))
		} else {
			d.logInfo("EnableNotifications 成功，等待设备通知")
		}
	})
	d.mu.Lock()
	d.writeChar = ch
	d.charCached = true
	d.mu.Unlock()
	return nil
}

// writeAndRecv 在 executor 内执行完整一轮 BLE 通信：
//  1. 非阻塞 drain notifCh 残留通知；
//  2. ensureWriteChar（发现服务/特征 + 首次 EnableNotifications）；
//  3. WriteWithoutResponse 优先写帧，不支持则普通 Write；
//  4. 阻塞等待 notifCh 的第一个 len>0 通知数据返回，遵守 ctx 取消。
//
// 阻塞等通知在 executor 内是 OK 的：通知回调不经 executor，不会死锁。
func (d *Device) writeAndRecv(ctx context.Context, frame []byte) ([]byte, error) {
	if d.tinyDev == nil {
		return nil, errNotConnected
	}
drain:
	for {
		select {
		case <-d.notifCh:
		default:
			break drain
		}
	}
	if err := d.ensureWriteChar(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	ch := d.writeChar
	d.mu.Unlock()

	// WriteWithoutResponse 优先；不支持（权限/HCI 未实现）时退回普通 Write。
	d.logInfo(fmt.Sprintf("发送蓝牙帧: %s", hex.EncodeToString(frame)))
	if _, err := ch.WriteWithoutResponse(frame); err != nil {
		d.logInfo(fmt.Sprintf("WriteWithoutResponse 失败(%v)，退回普通 Write", err))
		if _, werr := ch.Write(frame); werr != nil {
			d.logInfo(fmt.Sprintf("普通 Write 也失败: %v", werr))
			return nil, werr
		}
	}
	d.logInfo("帧已写入，等待设备通知...")
	// 等第一个 len>0 的通知数据返回；超过 40s 未收到则超时（对应 Perl
	// SendAndRecv 的默认 timeout=40），避免设备不响应时无限阻塞。
	timeout := time.NewTimer(40 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			d.logInfo("等待通知被 ctx 取消")
			return nil, ctx.Err()
		case <-timeout.C:
			d.logInfo("等待设备通知超时(40s)")
			return nil, errNotifyTimeout
		case buf := <-d.notifCh:
			d.logInfo(fmt.Sprintf("收到通知: len=%d data=%s", len(buf), hex.EncodeToString(buf)))
			if len(buf) > 0 {
				return buf, nil
			}
		}
	}
}

// findService 定位 GATT 服务（常量 0xfff0，对应 Perl BLAT
// FindCharacteristicToOperator 的服务过滤）。
//
// 不再由 Device 直连：服务发现已纳入 findCharacteristicWithRetry 流程（服
// 务+特征一次性发现，含重试）。此函数仅保留以兼容历史调用路径
// （当前代码无外部调用）。
func (d *Device) findService() (bt.DeviceService, error) {
	svcs, err := discoverServices(d.tinyDev, serviceUUID16, serviceDiscoveryTimeout)
	if err != nil {
		return bt.DeviceService{}, err
	}
	if len(svcs) == 0 {
		return bt.DeviceService{}, errNoService
	}
	d.logInfo(fmt.Sprintf("发现服务成功: %s", svcs[0].UUID()))
	return svcs[0], nil
}

// findCharacteristic 在 svc 内定位特征（常量 0xfff2，读写同一特征）。
func (d *Device) findCharacteristic(svc bt.DeviceService) (bt.DeviceCharacteristic, error) {
	chars, err := discoverCharacteristic(svc, charUUID16, characteristicDiscoveryTimeout)
	if err != nil {
		return bt.DeviceCharacteristic{}, err
	}
	if len(chars) == 0 {
		return bt.DeviceCharacteristic{}, errNoCharacteristic
	}
	d.logInfo(fmt.Sprintf("发现特征成功: %s", chars[0].UUID()))
	return chars[0], nil
}

func ctxErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// ---- 帧构造（对应 Perl BLAT BluetoothRead / BluetoothReboot 协议）----

// devTypeByte 返回帧头设备类型字节的 hex：PSAV(大小写不敏感) → "f9"，
// 其它（含空串）→ "f8"。对应 Perl 的 /PSAV/i 匹配。
func devTypeByte(t string) string {
	if strings.Contains(strings.ToUpper(t), "PSAV") {
		return "f9"
	}
	return "f8"
}

// buildReadFrame 构造读取帧：hex "04"+devTypeByte+"a100000000000000" → 字节。
// PSAV 时为 04 f9 a1 00 00 00 00 00 00 00 00（11 字节）。
func buildReadFrame(devType string) []byte {
	b, _ := hex.DecodeString("04" + devTypeByte(devType) + "a100000000000000")
	return b
}

// buildSetConfigFrame 构造配置帧：[0x01, devTypeByte] + CBOR(p)。
// p 应来自 DefaultSetConfigPayload 并覆盖所需字段；nil 字段被 omitempty
// 省略。帧 = 01 + devTypeByte + CBOR(p)，等价 Perl 的
// pack("H*", "01".$devtype) . marshal($args)。
func buildSetConfigFrame(devType string, p *SetConfigPayload) ([]byte, error) {
	payload, err := cbor.Marshal(p)
	if err != nil {
		return nil, err
	}
	head, err := hex.DecodeString("01" + devTypeByte(devType))
	if err != nil {
		return nil, err
	}
	return append(head, payload...), nil
}

// SetConfigPayload 是 SetConfig 帧的 CBOR 载荷，字段对应 Perl
// gen_bluetooth_data_to_send 的 tag 表（1-13）。*int + keyasint,omitempty：
// keyasint 让数字 tag 编码为 CBOR 整数键（与 CBOR::XS 一致，设备固件按整数
// 键解析）；omitempty + 指针保证 nil 字段省略、0 值字段保留（如 EnableNbiot
// 的 tag10=0 必须发出）。
type SetConfigPayload struct {
	Year                  *int `cbor:"1,keyasint,omitempty"`
	Month                 *int `cbor:"2,keyasint,omitempty"`
	Day                   *int `cbor:"3,keyasint,omitempty"`
	Hour                  *int `cbor:"4,keyasint,omitempty"`
	Minute                *int `cbor:"5,keyasint,omitempty"`
	Second                *int `cbor:"6,keyasint,omitempty"`
	BeatDur               *int `cbor:"7,keyasint,omitempty"`
	SetOpenPre            *int `cbor:"8,keyasint,omitempty"`
	ValveActivityInterval *int `cbor:"9,keyasint,omitempty"`
	ForceCloseNBModule    *int `cbor:"10,keyasint,omitempty"`
	ReverseFlow           *int `cbor:"11,keyasint,omitempty"`
	CtrlType              *int `cbor:"12,keyasint,omitempty"`
	CtrlArg               *int `cbor:"13,keyasint,omitempty"`
}

// DefaultSetConfigPayload 返回带 BLAT 默认字段的载荷，对应 Perl
// gen_bluetooth_data_to_send（HeatDev.pm L136-148）：
//
//	Year => 当前年 - 2000；Month => localtime 的 $mon（0-11）
//	Day/Hour/Minute/Second 取当前时间；BeatDur => 60*23（23 小时）
//	SetOpenPre => 100；ValveActivityInterval => 30；ReverseFlow => 0
//
// now 参数注入便于测试（生产传 time.Now()）。调用方按需覆盖具体字段后传给
// buildSetConfigFrame。
func DefaultSetConfigPayload(now time.Time) *SetConfigPayload {
	return &SetConfigPayload{
		Year:                  intPtr(now.Year() - 2000),
		Month:                 intPtr(int(now.Month()) - 1), // Perl localtime $mon 范围 0-11
		Day:                   intPtr(now.Day()),
		Hour:                  intPtr(now.Hour()),
		Minute:                intPtr(now.Minute()),
		Second:                intPtr(now.Second()),
		BeatDur:               intPtr(60 * 23), // BeatDur 23 小时
		SetOpenPre:            intPtr(100),
		ValveActivityInterval: intPtr(30),
		ReverseFlow:           intPtr(0),
	}
}

func intPtr(v int) *int { return &v }

// ---- 响应解析（对应 Perl BLAT parse_bluetooth_response_data）----

// parseStatus 解析读响应：hex 化后跳过前 4 个 hex 字符（前 2 字节帧头），
// 剩余字节 pack 回字节做 CBOR 解码（map[interface{}]interface{}），按数字
// tag 映射到 Status。Sn(tag 25) 可能是 string 或 []byte（设备固件两种都
// 发过，map 路径 + snToString 兼容两种；cbor tag 直解不支持 byte string）。
// 解码失败返回 nil（对应 Perl 的 undef 路径）。
func parseStatus(raw []byte) *Status {
	if len(raw) == 0 {
		return nil
	}
	hexStr := hex.EncodeToString(raw)
	if len(hexStr) < 4 {
		return nil
	}
	payload, err := hex.DecodeString(hexStr[4:])
	if err != nil {
		return nil
	}
	var m map[interface{}]interface{}
	if err := cbor.Unmarshal(payload, &m); err != nil {
		return nil
	}
	st := &Status{}
	for k, v := range m {
		switch toInt(k) {
		case 1:
			st.Timestamp = toInt(v)
		case 4:
			st.NbRssi = toInt(v)
		case 5:
			st.NbSnr = toInt(v)
		case 8:
			st.SoftVer = toInt(v)
		case 10:
			st.InTemp = toInt(v)
		case 13:
			st.Err = toInt(v)
		case 14:
			st.Flow = toInt(v)
		case 16:
			st.Voltage = toInt(v)
		case 20:
			st.ForceCloseNB = toInt(v)
		case 22:
			st.BlVer = toInt(v)
		case 23:
			st.ValveState = toInt(v)
		case 25:
			st.Sn = snToString(v)
		case 26:
			st.DN = toInt(v)
		}
	}
	return st
}

// toInt 把 CBOR 解码出的数值统一转为 int：float 直接 int()，整数用
// reflect 类型断言兜底，其它类型返回 0。
func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int8:
		return int(n)
	case int16:
		return int(n)
	case int32:
		return int(n)
	case int64:
		return int(n)
	case uint:
		return int(n)
	case uint8:
		return int(n)
	case uint16:
		return int(n)
	case uint32:
		return int(n)
	case uint64:
		return int(n)
	case float32:
		return int(n)
	case float64:
		return int(n)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return int(rv.Float())
	}
	return 0
}

// snToString 把 Sn(tag 25) 统一转 string：CBOR 里可能是 string 或 []byte。
func snToString(v interface{}) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	}
	return ""
}

// setConfigOK 判定 SetConfig 响应是否成功：响应字节 hex 化后匹配
// 05f8bf0000ff 或 05f9bf0000ff（大小写不敏感），对应 Perl 的
// /05f[8-9]bf0000ff/i。
func setConfigOK(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	up := bytes.ToUpper([]byte(hex.EncodeToString(raw)))
	return bytes.Contains(up, []byte("05F8BF0000FF")) ||
		bytes.Contains(up, []byte("05F9BF0000FF"))
}

// ---- id 到 BLE 地址的派生（对应 Perl BLAT::Common::Utils::parseIdToMac）----

// idMacHash 复现 Perl _hashTimes33（Math::BigInt 无限精度）的截断结果：
// 取 uint64 自然溢出后的值。Perl 取哈希低 24 位（旧格式）/ 低 40 位（新格式）
// 时，低 40 位与 Go 截断后低 40 位一致（2^40 整除 2^64）。start=5381
// （djb2 初值），hash = hash*33 + v，v 来自 id2MacArray。
func idMacHash(id string) uint64 {
	arr := id2MacArray(id)
	h := uint64(5381)
	for _, v := range arr {
		h = h<<5 + h + v
	}
	return h
}

// ParseIdToMac 把设备序列号 id 派生为**新格式** BLE 广播地址
// （FC:XX:XX:XX:XX:XX，共 5 字节哈希）。对应 Perl 7b1f4349 升级后的
// parseIdToMac：新格式比旧格式（FC:E8:92: + 3 字节）多算 2 字节以减小
// 哈希冲突概率。Go 侧从截断 uint64 中取低 40 位（5 字节），与 Perl
// Math::BigInt 无限精度结果等价（2^40 | 2^64）。
func ParseIdToMac(id string) string {
	h := idMacHash(id)
	return fmt.Sprintf("FC:%02X:%02X:%02X:%02X:%02X",
		byte(h>>32), byte(h>>24), byte(h>>16), byte(h>>8), byte(h))
}

// ParseIdToMacOld 派生旧格式 BLE 地址（FC:E8:92:XX:XX:XX，3 字节哈希），
// 对应 Perl parseIdToMacOld。保留用于兼容 ≤58 固件的旧设备；Connect
// 内部会在扫描匹配阶段同时尝试新+旧 MAC，但不会直接连旧 MAC。
func ParseIdToMacOld(id string) string {
	h := idMacHash(id)
	return fmt.Sprintf("FC:E8:92:%02X:%02X:%02X",
		byte(h>>16), byte(h>>8), byte(h))
}

// legacyMacFrom 由新格式 MAC 派生兼容用旧 MAC（"FC:E8:92:" + 新 MAC 的
// 低 3 字节）。len ≤ 12 时返回空串（输入非完整 5 字节 MAC），对齐 Perl
// BlueToothScanAndConnect 的 len(targetUpperMAC) <= 12 短路。
func legacyMacFrom(target string) string {
	if len(target) <= 12 {
		return ""
	}
	return "FC:E8:92:" + target[9:]
}

// id2MacArray 复现 Perl _id2MacArray：从 id 末尾起每 2 字符一组（substr
// 的负数 offset 从尾部计数，越界截断），hex 解析后正序填入 16 长度数组。
func id2MacArray(id string) [16]uint64 {
	var arr [16]uint64
	j := 0
	for i := len(id); i > 0; i -= 2 {
		// Perl substr($id, $i-2, 2) 的 offset 语义：
		// 负数表示从尾部倒数第 -offset 个字符开始；越界自动截断。
		start := i - 2
		if start < 0 {
			start = len(id) + start
		}
		if start < 0 {
			start = 0
		}
		end := start + 2
		if end > len(id) {
			end = len(id)
		}
		v, err := strconv.ParseInt(id[start:end], 16, 64)
		if err != nil {
			v = 0 // Perl hex() 对无法解析的内容返回 0
		}
		if j < len(arr) {
			arr[j] = uint64(v)
			j++
		}
	}
	return arr
}
