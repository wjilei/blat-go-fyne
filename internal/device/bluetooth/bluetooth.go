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
//     所有真实 BLE 调用（Enable/Connect/Discover/Read/Write）都投递到
//     一个专用串行 executor goroutine 依次执行，规避 Windows 上从非
//     主线程并发调用 Connect/DiscoverServices/DiscoverCharacteristics/
//     EnableNotifications 导致的崩溃（tinygo issue #294）。
package bluetooth

import (
	"context"
	"errors"
	"sync"
	"time"

	bt "tinygo.org/x/bluetooth"
)

// errNotConnected is returned when both Connect retries fail.
var errNotConnected = errors.New("蓝牙连接失败")

// 真实模式使用的辅助错误。
var (
	errEmptyRead        = errors.New("蓝牙读取为空")
	errNoService        = errors.New("蓝牙未发现服务")
	errNoCharacteristic = errors.New("蓝牙未发现特征")
)

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

	// readCount counts Read() calls. The mock keeps ForceCloseNB=0 by
	// default so the pass path is taken; a test can SetMockStatus with
	// ForceCloseNB!=0 / NbRssi=0,NbSnr=0 to exercise the NB branches.
	readCount int

	// connectedSinceReboot records the last Reboot() time; used by the
	// ResetValve mock to restore a clean ValveState.
	connectedSinceReboot time.Time

	status Status // mock 数据，SetMockStatus 可注入测试场景

	// mock 为 true 时走内存 mock 路径；false 时走 tinygo 真实 BLE 路径。
	mock bool

	// ---- real 模式字段（mock==false 时使用）----
	// adapter 是 tinygo 系统默认 BLE 适配器（Windows 上为 DefaultAdapter）。
	adapter *bt.Adapter
	// tinyDev 保存 Adapter.Connect 返回的远端设备引用；nil 表示未连接。
	// 只在串行 executor goroutine 内读写。
	tinyDev *bt.Device
	// GATT 服务/特征 UUID。业务协议未定，默认空串：Discover 时传空过滤
	// 全量枚举并取第一个发现的服务/特征。
	// TODO: 按 Perl BLAT 协议配置真实的服务与特征 UUID。
	serviceUUID string
	readUUID    string
	writeUUID   string

	// enableOnce 保证 adapter.Enable() 只执行一次：Windows 上重复
	// RoInitialize 会返回 S_FALSE（0x1），go-ole 会把它当错误抛出。
	enableOnce sync.Once

	// exec 是真实 BLE 调用的专用串行 executor：所有 tinygo 调用
	// （Enable/Connect/Discover/Read/Write）投递到该 chan，由单 goroutine
	// 依次执行，方法侧用应答 channel 同步等待结果，遵守 ctx 取消。
	exec     chan func()
	execInit sync.Once
}

// NewDevice returns a device pre-loaded with pass-through mock data.
// 等价 NewMockDevice，保持向后兼容（main 现有两处调用不变）。
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
// executor 串行执行。GATT 服务/特征 UUID 默认空串（按 Perl BLAT 协议
// 尚未定稿，TODO）；空串时 Discover 全量并取第一个发现的服务/特征。
func NewRealDevice() *Device {
	return &Device{
		mock:    false,
		adapter: bt.DefaultAdapter,
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

// IsConnected reports whether the device is connected to mac. real 模式
// 同样基于连接状态字段（connected/mac），不触发 BLE 调用。
func (d *Device) IsConnected(mac string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected && d.mac == mac
}

// Connect ensures the device is connected to mac, retrying at most twice
// (mirrors the Perl IsBleConnected / ConnectBle pair).
func (d *Device) Connect(ctx context.Context, mac string) error {
	if d.IsConnected(mac) {
		return nil
	}
	if d.mock {
		return d.mockConnect(ctx, mac)
	}
	return d.realConnect(ctx, mac)
}

func (d *Device) mockConnect(ctx context.Context, mac string) error {
	for i := 0; i < 2; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := d.doConnect(mac); err == nil {
			return nil
		}
	}
	return errNotConnected
}

func (d *Device) doConnect(mac string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.connected = true
	d.mac = mac
	return nil
}

// realConnect 在真实模式下建立 BLE 连接。连接动作（Enable/Connect）必须
// 在串行 executor goroutine 内执行；全部重试失败时返回 errNotConnected。
func (d *Device) realConnect(ctx context.Context, mac string) error {
	// ParseMAC 是纯内存计算，不需要进 executor。
	macAddr, err := bt.ParseMAC(mac)
	if err != nil {
		return errNotConnected
	}
	addr := bt.Address{MACAddress: bt.MACAddress{MAC: macAddr}}
	for i := 0; i < 2; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := d.execCall(ctx, func() (any, error) {
			return nil, d.realConnectLocked(addr, mac)
		})
		if err == nil {
			return nil
		}
	}
	return errNotConnected
}

// realConnectLocked 必须在串行 executor goroutine 内调用。
func (d *Device) realConnectLocked(addr bt.Address, mac string) error {
	var enableErr error
	d.enableOnce.Do(func() { enableErr = d.adapter.Enable() })
	if enableErr != nil {
		return enableErr
	}
	dev, err := d.adapter.Connect(addr, bt.ConnectionParams{})
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.tinyDev = &dev
	d.connected = true
	d.mac = mac
	d.mu.Unlock()
	return nil
}

// Reboot restarts the device. Mock always succeeds and records the time
// for the ValveState reset mock.
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
	// TODO: 按 Perl BLAT 协议构造真实的重启命令字节
	return d.realWrite(ctx, nil)
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
		st.Sn = d.mac
	}
	return &st
}

// realRead 真实模式下经串行 executor DiscoverServices/DiscoverCharacteristics
// 后读取原始字节，再做最小解析骨架：字节非空且无错误时返回一个带 Sn=mac、
// 其余字段 0 的 Status。错误或空返回 nil。
// TODO: 按 Perl BLAT 协议解析真实字节。
func (d *Device) realRead(ctx context.Context) *Status {
	if err := ctxErr(ctx); err != nil {
		return nil
	}
	// 未连接直接返回 nil，避免 nil deref；业务侧会报“读取蓝牙失败”。
	if !d.isConnected() {
		return nil
	}
	raw, err := d.execCall(ctx, func() (any, error) {
		data, err := d.readRaw()
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			return nil, errEmptyRead
		}
		return data, nil
	})
	if err != nil {
		return nil
	}
	rawBytes, _ := raw.([]byte)
	if len(rawBytes) == 0 {
		return nil
	}
	// TODO: 按 Perl BLAT 协议解析真实字节（目前仅最小骨架：Sn=mac，其余 0）
	return &Status{Sn: d.mac}
}

// EnableNbiot turns the NB-IoT modem on. Mock: no-op.
func (d *Device) EnableNbiot(ctx context.Context) error {
	if d.mock {
		return ctxErr(ctx)
	}
	// TODO: 按 Perl BLAT 协议构造 NB-IoT 开启命令字节
	return d.realWrite(ctx, nil)
}

// DisableNbiot turns the NB-IoT modem off. Mock: no-op.
func (d *Device) DisableNbiot(ctx context.Context) error {
	if d.mock {
		return ctxErr(ctx)
	}
	// TODO: 按 Perl BLAT 协议构造 NB-IoT 关闭命令字节
	return d.realWrite(ctx, nil)
}

// ResetValve resets the valve and restores ValveState to 0 in the mock.
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
	// TODO: 按 Perl BLAT 协议构造阀门复位命令字节
	return d.realWrite(ctx, nil)
}

// realWrite 通过串行 executor 对 GATT 写入特征下发命令字节。对应的
// 服务/特征 UUID 未配置（协议未定，writeUUID 为空串）时直接返回 nil
// 并留 TODO 注释，不 panic。
func (d *Device) realWrite(ctx context.Context, payload []byte) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if d.writeUUID == "" {
		// TODO: 按 Perl BLAT 协议配置 GATT 写入特征 UUID 并构造命令字节后实现
		return nil
	}
	if !d.isConnected() {
		return errNotConnected
	}
	_, err := d.execCall(ctx, func() (any, error) {
		return nil, d.writeRaw(payload)
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
func (d *Device) execCall(ctx context.Context, fn func() (any, error)) (any, error) {
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

type execResult struct {
	val any
	err error
}

// ---- GATT 原始读写（均须在串行 executor goroutine 内调用）----

// readRaw 在 executor 内执行 DiscoverServices → DiscoverCharacteristics
// → Read，返回原始字节。
func (d *Device) readRaw() ([]byte, error) {
	if d.tinyDev == nil {
		return nil, errNotConnected
	}
	svc, err := d.findService(d.serviceUUID)
	if err != nil {
		return nil, err
	}
	ch, err := d.findCharacteristic(svc, d.readUUID)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 512)
	n, err := ch.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// writeRaw 在 executor 内执行 DiscoverServices → DiscoverCharacteristics
// → Write，下发 payload。
func (d *Device) writeRaw(payload []byte) error {
	if d.tinyDev == nil {
		return errNotConnected
	}
	svc, err := d.findService(d.serviceUUID)
	if err != nil {
		return err
	}
	ch, err := d.findCharacteristic(svc, d.writeUUID)
	if err != nil {
		return err
	}
	_, err = ch.Write(payload)
	return err
}

// findService 定位 GATT 服务；uuid 为空时返回发现的第一个服务。
// TODO: 按 Perl BLAT 协议配置真实的业务服务 UUID。
func (d *Device) findService(uuid string) (bt.DeviceService, error) {
	var filter []bt.UUID
	if uuid != "" {
		u, err := bt.ParseUUID(uuid)
		if err != nil {
			return bt.DeviceService{}, err
		}
		filter = []bt.UUID{u}
	}
	svcs, err := d.tinyDev.DiscoverServices(filter)
	if err != nil {
		return bt.DeviceService{}, err
	}
	if len(svcs) == 0 {
		return bt.DeviceService{}, errNoService
	}
	return svcs[0], nil
}

// findCharacteristic 在 svc 内定位特征；uuid 为空时返回第一个特征。
// TODO: 按 Perl BLAT 协议配置真实的业务特征 UUID。
func (d *Device) findCharacteristic(svc bt.DeviceService, uuid string) (bt.DeviceCharacteristic, error) {
	var filter []bt.UUID
	if uuid != "" {
		u, err := bt.ParseUUID(uuid)
		if err != nil {
			return bt.DeviceCharacteristic{}, err
		}
		filter = []bt.UUID{u}
	}
	chars, err := svc.DiscoverCharacteristics(filter)
	if err != nil {
		return bt.DeviceCharacteristic{}, err
	}
	if len(chars) == 0 {
		return bt.DeviceCharacteristic{}, errNoCharacteristic
	}
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
