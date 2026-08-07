package cases

import (
	"context"
	"errors"
	"fmt"
	"time"

	"blat/internal/core"
	"blat/internal/device/bluetooth"
)

// WireValveBluetoothTestAllParamsCase 翻译自 Perl
// BLAT::APP::Heat::Cases::wire_valve::bluetooth_test_all_params。
// 流程：连接蓝牙 → 重启 → 循环读取校验 NB 信号 → 校验软件版本/序列号/
// 管径/电压/流量/温度，PSAV 分支再等待阀门状态。
type WireValveBluetoothTestAllParamsCase struct {
	deviceType  string // 设备类型：PFW / PSAV
	bluetoothOp string // 蓝牙操作：read（预留）
}

func (c *WireValveBluetoothTestAllParamsCase) Name() string {
	return "wire_valve_bluetooth_test_all_params"
}

// Configure 读取 plan 的自定义参数，对应 Perl
// BLAT::APP::Heat::Cases::wire_valve::bluetooth_test_all_params_args 的
// 默认值（设备类型=PSAV、蓝牙操作=read）。
func (c *WireValveBluetoothTestAllParamsCase) Configure(args map[string]any) error {
	if v, ok := args["设备类型"].(string); ok && v != "" {
		c.deviceType = v
	}
	if c.deviceType == "" {
		c.deviceType = "PSAV"
	}
	if v, ok := args["蓝牙操作"].(string); ok && v != "" {
		c.bluetoothOp = v
	}
	if c.bluetoothOp == "" {
		c.bluetoothOp = "read"
	}
	return nil
}

func (c *WireValveBluetoothTestAllParamsCase) Run(ctx context.Context, env *core.Env) error {
	// 连接蓝牙（复用/创建并持久化连接，见 _ensureBluetooth）
	bt, id, err := _ensureBluetooth(ctx, env, c.deviceType)
	if err != nil {
		return err
	}

	heatnote, _ := env.Vars["HeatNote"].(map[string]any)
	pipe := _int(heatnote, "pipe")
	// 软件版本：优先 HeatNote（demo confs/env.yml 的位置），兜底 env.Vars 顶层
	// （Perl 里是 env_args["软件版本"]）。
	softVer := _int(heatnote, "软件版本")
	if softVer == 0 {
		softVer = _int(env.Vars, "软件版本")
	}

	// 重启
	if err := bt.Reboot(ctx); err != nil {
		return fmt.Errorf("重启失败: %w", err)
	}
	env.Log.Info("重启成功")
	if err := _sleep(ctx, 2*time.Second); err != nil {
		return err
	}

	var obj *bluetooth.Status

	obj = bt.Read(ctx)
	if obj == nil {
		return errors.New("读取蓝牙失败")
	}

	env.Log.Info(fmt.Sprintf("软件版本: %v", obj.SoftVer))
	env.Log.Info(fmt.Sprintf("蓝牙版本: %v", obj.BlVer))
	env.Log.Info(fmt.Sprintf("管径: %v", obj.DN))
	voltage := float32(obj.Voltage)/100 + 2
	env.Log.Info(fmt.Sprintf("电压: %.2f", voltage))

	if obj.Sn != id {
		return fmt.Errorf("序列号不匹配: got %q want %q", obj.Sn, id)
	}
	if obj.DN != pipe {
		return fmt.Errorf("管径不一致: got %d want %d", obj.DN, pipe)
	}
	// 设备类型分支
	if c.deviceType == "PFW" {
		if obj.Err&0x8 != 0 && obj.Flow == 0 {
			return errors.New("空管，超声波传感器不良")
		}
		if obj.Err&0x1 != 0 && obj.InTemp < 0 {
			return errors.New("进水温度异常")
		}
		return nil
	}

	// 非 PFW（PSAV）：循环 30s 等待阀门状态
	deadline := time.Now().Add(30 * time.Second)
	for {
		obj = bt.Read(ctx)
		if obj == nil {
			return errors.New("读取蓝牙失败")
		}
		switch obj.ValveState {
		case 0:
			env.Log.Info("阀门状态正确")
			return nil
		case 1:
			return errors.New("阀门状态异常")
		case 3:
			_ = bt.ResetValve(ctx)
			return errors.New("阀门状态错误")
		}
		if time.Now().After(deadline) {
			return errors.New("阀门状态超时")
		}
		if err := _sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

// _sleep 阻塞 d 时长，ctx 取消时提前返回（Perl sleep 的 ctx-aware 版，
// 防止 Stop 按钮卡死）。
func _sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func _str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func _int(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func init() {
	Register("HeatSuite::wire_valve_bluetooth_test_all_params", func() (core.Case, error) {
		return &WireValveBluetoothTestAllParamsCase{}, nil
	})
	Register("HeatSuite::wire_valve_bluetooth_reset_valve", func() (core.Case, error) {
		return &WireValveBluetoothResetValveCase{}, nil
	})
}

// WireValveBluetoothResetValveCase 翻译自 Perl
// BLAT::APP::Heat::Cases::wire_valve::bluetooth_reset_valve。
// 流程：连接蓝牙 → 重置阀门（ResetValve）。
type WireValveBluetoothResetValveCase struct {
	deviceType string // 设备类型：PFW / PSAV
}

func (c *WireValveBluetoothResetValveCase) Name() string {
	return "wire_valve_bluetooth_reset_valve"
}

// Configure 读取 plan 的自定义参数，默认设备类型=PSAV（与 test_all_params 一致）。
func (c *WireValveBluetoothResetValveCase) Configure(args map[string]any) error {
	if v, ok := args["设备类型"].(string); ok && v != "" {
		c.deviceType = v
	}
	if c.deviceType == "" {
		c.deviceType = "PSAV"
	}
	return nil
}

func (c *WireValveBluetoothResetValveCase) Run(ctx context.Context, env *core.Env) error {
	// 连接蓝牙（复用/创建并持久化连接，见 _ensureBluetooth）
	bt, _, err := _ensureBluetooth(ctx, env, c.deviceType)
	if err != nil {
		return err
	}

	// 重置阀门
	env.Log.Info("重置阀门")
	if err := bt.ResetValve(ctx); err != nil {
		return fmt.Errorf("重置阀门失败: %w", err)
	}
	env.Log.Info("重置阀门成功")
	return nil
}

// _ensureBluetooth 获取/创建蓝牙设备并确保已连接（对应 Perl
// _ensure_bluetooth_connected）。优先复用 Vars.HeatNote["bluetooth"] 已持久化的
// 连接（mock 或 real 由 bt_mock 标志决定构造）；无则创建、连接后写回 HeatNote 供
// 后续 case 复用。设备类型决定蓝牙协议帧头字节（PSAV→f9，其它→f8），对应 Perl
// dev_type 字段。返回设备与序列号（由 HeatNote["serial"] 派生 mac 用于连接）。
func _ensureBluetooth(ctx context.Context, env *core.Env, deviceType string) (*bluetooth.Device, string, error) {
	// 注意：不能无条件 fallback 到 env.Devs["bluetooth"]——main 默认注入
	// NewDevice()（real），无条件复用会让 -mock-bt=true 拿不到 mock 实例。
	// 仅当 Devs 实例的模式与 bt_mock 请求的模式一致时才兜底复用。
	heatnote, _ := env.Vars["HeatNote"].(map[string]any)
	bt, ok := heatnote["bluetooth"].(*bluetooth.Device)
	if !ok {
		mock, _ := heatnote["bt_mock"].(bool)
		if dev, has := env.Devs["bluetooth"].(*bluetooth.Device); has && !dev.IsReal() == mock {
			bt = dev
		} else if mock {
			bt = bluetooth.NewMockDevice()
		} else {
			bt = bluetooth.NewRealDevice()
		}
		// 存回 vars 供后续 case 复用
		if heatnote == nil {
			heatnote = map[string]any{}
			env.Vars["HeatNote"] = heatnote
		}
		heatnote["bluetooth"] = bt
	}

	id := _str(heatnote, "serial")

	// mac 由序列号 parseIdToMac 派生；已连接则跳过重复连接，否则重试连接
	// （Device.Connect 内部最多重试 2 次，对应 Perl ConnectBle 重试）。
	bt.SetDevType(deviceType)
	// 注入日志输出：发现服务/特征成功、扫描成功、连接成功都会打日志
	bt.SetLogger(env.Log)

	env.Log.Info("扫描并连接蓝牙")
	mac := bluetooth.ParseIdToMac(id)
	if bt.IsConnected(mac) {
		env.Log.Info("蓝牙已连接，跳过重复连接")
	} else if err := bt.Connect(ctx, id); err != nil {
		return nil, "", fmt.Errorf("%w", err)
	}
	return bt, id, nil
}
