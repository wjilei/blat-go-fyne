package cases

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// Configure 读取 plan 的自定义参数。
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
	// 优先复用已持久化的蓝牙连接（存于 Vars.HeatNote["bluetooth"]，mock 或
	// real 由 bt_mock 标志决定构造）；无则创建并连接后存回，供后续 case 复用。
	heatnote, _ := env.Vars["HeatNote"].(map[string]any)
	bt, ok := heatnote["bluetooth"].(*bluetooth.Device)
	if !ok {
		// 注意：不能无条件 fallback 到 env.Devs["bluetooth"]——main 始终注入
		// NewDevice()（mock），无条件复用会让 -mock-bt=false 拿不到 real 实例。
		// 仅当 Devs 实例的模式与 bt_mock 请求的模式一致时才兜底复用。
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

	id := _str(heatnote, "mac")
	pipe := _int(heatnote, "pipe")
	testMode := _str(heatnote, "test_mode")
	// 软件版本：优先 HeatNote（demo confs/env.yml 的位置），兜底 env.Vars 顶层
	// （Perl 里是 env_args["软件版本"]）。
	softVer := _int(heatnote, "软件版本")
	if softVer == 0 {
		softVer = _int(env.Vars, "软件版本")
	}

	// 连接 + 重启
	if err := bt.Connect(ctx, id); err != nil {
		return fmt.Errorf("蓝牙连接失败: %w", err)
	}
	if err := bt.Reboot(ctx); err != nil {
		return fmt.Errorf("重启失败: %w", err)
	}
	env.Log.Info("重启成功")
	if err := _sleep(ctx, 2*time.Second); err != nil {
		return err
	}

	// 循环读取，等待 NB 信号就绪（最多 59 次）
	nbOk := false
	var obj *bluetooth.Status
	readCnt := 0
	for i := 0; i < 59; i++ {
		obj = bt.Read(ctx)
		if obj == nil {
			return errors.New("读取蓝牙失败")
		}
		if obj.ForceCloseNB != 0 {
			_ = bt.EnableNbiot(ctx)
			if err := _sleep(ctx, time.Second); err != nil {
				return err
			}
			continue
		}
		if obj.NbRssi == 0 && obj.NbSnr == 0 {
			readCnt++
			if readCnt >= 30 {
				_ = bt.DisableNbiot(ctx)
				if err := _sleep(ctx, 5*time.Second); err != nil {
					return err
				}
				_ = bt.EnableNbiot(ctx)
				readCnt = 0
			}
			if err := _sleep(ctx, time.Second); err != nil {
				return err
			}
			continue
		}
		if obj.NbRssi >= -81 || obj.NbSnr >= 0 {
			nbOk = true
			break
		}
		if err := _sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	if obj == nil {
		return errors.New("读取蓝牙失败")
	}
	if !nbOk {
		return errors.New("NB信号异常")
	}

	// 参数校验
	if obj.SoftVer != softVer {
		return fmt.Errorf("软件版本不匹配: got %d want %d", obj.SoftVer, softVer)
	}
	if strings.Contains(testMode, "single") && obj.Timestamp > 40 {
		return errors.New("蓝牙没有正确重启")
	}
	if obj.Sn != id {
		return fmt.Errorf("序列号不匹配: got %q want %q", obj.Sn, id)
	}
	if obj.DN != pipe {
		return fmt.Errorf("管径不一致: got %d want %d", obj.DN, pipe)
	}
	voltage := obj.Voltage/100 + 2
	env.Log.Info(fmt.Sprintf("电压: %d", voltage))

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
}
