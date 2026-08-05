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
// signatures stay ctx-aware for future real implementations.
package bluetooth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errNotConnected is returned when both Connect retries fail.
var errNotConnected = errors.New("蓝牙连接失败")

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
}

// NewDevice returns a device pre-loaded with pass-through mock data.
func NewDevice() *Device {
	return &Device{
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

// SetMockStatus injects a status for the next Read() calls. Sn stays
// empty to default to the connected mac.
func (d *Device) SetMockStatus(s Status) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status = s
}

// IsConnected reports whether the device is connected to mac.
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

// Reboot restarts the device. Mock always succeeds and records the time
// for the ValveState reset mock.
func (d *Device) Reboot(ctx context.Context) error {
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

// Read returns the current (mock) status, defaulting Sn to the connected
// mac so the 序列号不匹配 check passes without injection.
func (d *Device) Read(ctx context.Context) *Status {
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

// EnableNbiot turns the NB-IoT modem on. Mock: no-op.
func (d *Device) EnableNbiot(ctx context.Context) error {
	return ctxErr(ctx)
}

// DisableNbiot turns the NB-IoT modem off. Mock: no-op.
func (d *Device) DisableNbiot(ctx context.Context) error {
	return ctxErr(ctx)
}

// ResetValve resets the valve and restores ValveState to 0 in the mock.
func (d *Device) ResetValve(ctx context.Context) error {
	if err := ctxErr(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status.ValveState = 0
	return nil
}

func ctxErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
