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
	deviceType string // 设备类型：PFW / PSAV
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
	return nil
}

func (c *WireValveBluetoothTestAllParamsCase) Run(ctx context.Context, env *core.Env) error {
	// 连接蓝牙（复用/创建并持久化连接，见 _ensureBluetooth）
	bt, _, err := _ensureBluetooth(ctx, env, c.deviceType)
	if err != nil {
		return err
	}
	// // 重启
	// if err := bt.Reboot(ctx); err != nil {
	// 	return fmt.Errorf("重启失败: %w", err)
	// }
	// env.Log.Info("重启成功")
	// if err := _sleep(ctx, 2*time.Second); err != nil {
	// 	return err
	// }

	var obj *bluetooth.Status

	obj = bt.Read(ctx)
	if obj == nil {
		return errors.New("读取蓝牙失败")
	}

	env.Log.Info(fmt.Sprintf("软件版本: %v", obj.SoftVer))
	env.Log.Info(fmt.Sprintf("蓝牙版本: %v", obj.BlVer))
	env.Log.Info(fmt.Sprintf("管径: %v", obj.DN))
	heatnote, _ := env.Vars["HeatNote"].(map[string]any)

	pipe := _int(heatnote, "pipe")
	if obj.DN != pipe {
		return errors.New(fmt.Sprintf("管径%v跟预期管径%v不一致", obj.DN, pipe))
	}
	voltage := float32(obj.Voltage)/100 + 2
	env.Log.Info(fmt.Sprintf("电压: %.2f", voltage))

	// 非 PFW（PSAV）：循环 30s 等待阀门状态
	deadline := time.Now().Add(30 * time.Second)
	for {
		obj = bt.Read(ctx)
		if obj == nil {
			return errors.New("读取蓝牙失败")
		}
		env.Log.Info(fmt.Sprintf("阀门状态: %d", obj.ValveState))
		switch obj.ValveState {
		case 0:
			env.Log.Info("阀门状态正确")
			return nil
		case 1:
			return errors.New("阀门状态异常")
		case 3:
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
	Register("HeatSuite::wire_valve_observe_valve", func() (core.Case, error) {
		return &WireValveObserveValveCase{}, nil
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

	if err := _sleep(ctx, 3*time.Second); err != nil {
		return err
	}

	// 读取并打印设备信息（重置后确认状态）
	obj := bt.Read(ctx)
	if obj == nil {
		return errors.New("读取蓝牙失败")
	}
	env.Log.Info(fmt.Sprintf("软件版本: %v", obj.SoftVer))
	env.Log.Info(fmt.Sprintf("蓝牙版本: %v", obj.BlVer))
	env.Log.Info(fmt.Sprintf("序列号: %v", obj.Sn))
	env.Log.Info(fmt.Sprintf("管径: %v", obj.DN))
	voltage := float32(obj.Voltage)/100 + 2
	env.Log.Info(fmt.Sprintf("电压: %.2f", voltage))
	env.Log.Info(fmt.Sprintf("流量: %v", obj.Flow))
	env.Log.Info(fmt.Sprintf("进水温: %v", obj.InTemp))
	env.Log.Info(fmt.Sprintf("NB 信号强度: %v", obj.NbRssi))
	env.Log.Info(fmt.Sprintf("阀门状态: %v", obj.ValveState))
	env.Log.Info(fmt.Sprintf("错误标志: %v", obj.Err))

	return nil
}

// WireValveObserveValveCase 提示操作员观察阀门状态。
// 用于重置阀门（bluetooth_reset_valve）之后人工确认执行器是否动作：
// 通过 UI 弹出提示信息，等待操作员观察后确认继续（GUI 弹框 / Console 回车）。
type WireValveObserveValveCase struct{}

func (c *WireValveObserveValveCase) Name() string {
	return "wire_valve_observe_valve"
}

func (c *WireValveObserveValveCase) Run(ctx context.Context, env *core.Env) error {
	// 触发 UI 提示；ctx-aware（Stop/关窗时返回 ctx.Err()）
	if err := env.UI.Message(ctx, "请观察阀门是否转动，确认后继续", true); err != nil {
		return err
	}
	env.Log.Info("已确认观察阀门")
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
