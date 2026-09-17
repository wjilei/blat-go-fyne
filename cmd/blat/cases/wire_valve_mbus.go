package cases

import (
	"context"
	"errors"
	"fmt"

	"blat/internal/core"
	"blat/internal/device/mbus"
)

// WireValveMBusReadMotorCase 翻译自 Perl
// BLAT::APP::Heat::Cases::wire_valve::dev_normal_check_motor 的"两阶段
// 开度人工确认"变体。流程：
//  1. 通过 M-Bus 设置阀门开度到 step1Open（默认 80）；
//  2. 弹框询问用户确认电机已开始转动；
//  3. 弹框提示用户等电机转完到目标位置后手动点确认；
//  4. 读取 M-Bus 设备信息校验告警位（Alarm 必须为 0，非 0 视为电机
//     状态异常，立即失败不恢复开度）；
//  5. 再次设置阀门开度到 step2Open（默认 100，恢复开度）；
//  6. 在日志里提示用户等电机转完再断电，避免带电拔线烧驱动。
//
// 注意：本用例不读取 M-Bus 的开度反馈——设备回读的不是实时值，机械
// 到位要靠肉眼确认。流程靠两次人工弹框驱动。
//
// 不判断 test_mode==normal——本 Run 场景本身就是 normal（产线 normal
// 计划下才会执行此用例，对应 Perl 里的 if 分支在 Run 路径恒为真）。
type WireValveMBusReadMotorCase struct {
	step1Open int // 阶段 1 目标开度（%），默认 80
	step2Open int // 阶段 2 恢复开度（%），默认 100
}

func (c *WireValveMBusReadMotorCase) Name() string {
	return "wire_valve_mbus_read_motor"
}

// Configure 读取 plan 的自定义参数。键名中文优先，同时兼容英文键
// step1_open/step2_open；未知键忽略不报错。
// 整数读取兼容 YAML 解析出的 int/int64/float64。
func (c *WireValveMBusReadMotorCase) Configure(args map[string]any) error {
	// 默认值
	c.step1Open = 80
	c.step2Open = 100

	// 阶段开度：中文键优先，兼容英文；非 [0,100] 视为非法值忽略
	if v, ok := lookupInt(args, "第一阶段开度", "step1_open"); ok && v >= 0 && v <= 100 {
		c.step1Open = v
	}
	if v, ok := lookupInt(args, "第二阶段开度", "step2_open"); ok && v >= 0 && v <= 100 {
		c.step2Open = v
	}
	return nil
}

// lookupInt 从 args 中按顺序查 keys（中文→英文），命中第一个存在的键。
// 返回 (值, 是否命中)；YAML 解析的 int/int64/float64 都接受。无可识别值
// 返回 (0, false)，调用方据此判断是否覆盖默认值。
func lookupInt(args map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := args[k]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case int:
			return x, true
		case int64:
			return int(x), true
		case float64:
			return int(x), true
		}
	}
	return 0, false
}

func (c *WireValveMBusReadMotorCase) Run(ctx context.Context, env *core.Env) error {
	heatnote, _ := env.Vars["HeatNote"].(map[string]any)

	// mac 来源：优先 HeatNote["mac"]，为空则回退到 serial（12 位数字串，
	// 直接可用作 M-Bus 从站地址）。
	mac := _str(heatnote, "serial")
	if mac == "" {
		return fmt.Errorf("未配置mac或serial")
	}

	// 获取/复用 M-Bus 设备并连接（见 _ensureMBUS）
	dev, err := _ensureMBUS(ctx, env)
	if err != nil {
		return err
	}

	// 阶段 1：设置开度 step1Open → 弹框让用户确认电机开始转动
	if err := dev.SetValveOpenpreByMbus(ctx, mac, c.step1Open); err != nil {
		return fmt.Errorf("设置阶段1开度 %d 失败: %w", c.step1Open, err)
	}
	if err := askValveTurning(ctx, env, c.step1Open); err != nil {
		return err
	}

	// 阶段 1.5：弹框让用户等电机转完到目标位置后手动点确认，
	// 再恢复开度到 step2Open。机械到位要靠肉眼，开度回读不实时。
	if err := askValveDoneWait(ctx, env, c.step1Open, c.step2Open); err != nil {
		return err
	}

	// 阶段 1.6：读取 M-Bus 设备信息，确认告警位为 0 后再恢复开度。
	// 对齐 Perl dev_normal_check_motor 末尾的 alarm 校验：Alarm 非 0 说明
	// 电机状态异常（如堵转/过温），不应继续把开度恢复到 step2Open。
	info, err := dev.MbusReadInfo(ctx, mac)
	if err != nil {
		return fmt.Errorf("读取M-Bus设备信息失败: %w", err)
	}
	if info.Alarm != 0 {
		return fmt.Errorf("M-Bus读取到告警(Alarm=%d)，不继续恢复开度", info.Alarm)
	}
	env.Log.Info("", fmt.Sprintf("M-Bus 设备信息读取成功，无告警（Alarm=0），准备恢复开度到 %d%%", c.step2Open))

	// 阶段 2：恢复开度到 step2Open
	if err := dev.SetValveOpenpreByMbus(ctx, mac, c.step2Open); err != nil {
		return fmt.Errorf("恢复阶段2开度 %d 失败: %w", c.step2Open, err)
	}

	// 结束提示：电机仍在转，提醒用户等转完再断电，避免带电拔线烧驱动。
	env.Log.Warn("", fmt.Sprintf("已恢复开度到 %d%%，请等电机转完再断电", c.step2Open))
	return nil
}

func init() {
	Register("HeatSuite::wire_valve_mbus_read_motor", func() (core.Case, error) {
		return &WireValveMBusReadMotorCase{}, nil
	})
}

// askValveTurning 弹一个「是/否」确认框，让操作员确认电机已开始转动。
// 选「是」→ 返回 nil 继续；选「否」→ 返回 error（用例失败）；
// ctx 取消（Stop 按钮 / 关窗）→ 返回 ctx.Err()。回车默认「是」（参见
// internal/ui/fyne/app.go yesNoCh 处理，默认焦点在「是」上）。
// openPre 参数告知用户刚设置的目标开度值，文案携带便于判断。
func askValveTurning(ctx context.Context, env *core.Env, openPre int) error {
	msg := fmt.Sprintf("已通过 M-Bus 设置阀门开度到 %d%%，请观察电机是否已开始转动？选「否」将停止并失败", openPre)
	ok, err := env.UI.Confirm(ctx, msg, true)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("用户确认电机未转动")
	}
	env.Log.Info("", fmt.Sprintf("已确认电机开始转动（开度 %d%%）", openPre))
	return nil
}

// askValveDoneWait 弹一个「确定」按钮的提示框，让用户等电机转到目标位置
// 完成后手动点确认，确认后用例再读取 M-Bus 信息校验告警、恢复开度。
// 机械到位要肉眼判断，开度回读不是实时值。ctx 取消（Stop 按钮 / 关窗）
// → 返回 ctx.Err()。fromOpen/toOpen 仅用于文案告知用户当前→目标的开度值。
func askValveDoneWait(ctx context.Context, env *core.Env, fromOpen, toOpen int) error {
	msg := fmt.Sprintf("请等待电机转到 %d%% 目标位置后点击「确定」，确认后用例将继续把开度恢复到 %d%%",
		fromOpen, toOpen)
	if err := env.UI.Message(ctx, msg, true); err != nil {
		return err
	}
	env.Log.Info("", fmt.Sprintf("用户已确认电机转到 %d%%，准备恢复开度到 %d%%", fromOpen, toOpen))
	return nil
}

// _ensureMBUS 获取/创建 M-Bus 设备并确保已连接（对应 Perl
// _ensure_mbus_connected）。优先复用 Vars.HeatNote["mbus_dev"] 已持久化的
// 连接（mock 或 real 由 mbus_mock 标志决定构造）；无则创建、连接后写回
// HeatNote 供后续 case 复用。串口从 HeatNote["mbus"].(map[string]any) 的
// 小写键 port 读取（如 "COM9"）。
func _ensureMBUS(ctx context.Context, env *core.Env) (*mbus.Device, error) {
	// 注意：不能无条件 fallback 到 env.Devs["mbus"]——main 默认注入
	// NewDevice()（real），无条件复用会让 -mock-mbus=true 拿不到 mock 实例。
	// 仅当 Devs 实例的模式与 mbus_mock 请求的模式一致时才兜底复用。
	heatnote, _ := env.Vars["HeatNote"].(map[string]any)
	dev, ok := heatnote["mbus_dev"].(*mbus.Device)
	if !ok {
		mock, _ := heatnote["mbus_mock"].(bool)
		if d, has := env.Devs["mbus"].(*mbus.Device); has && !d.IsReal() == mock {
			dev = d
		} else if mock {
			dev = mbus.NewMockDevice()
		} else {
			dev = mbus.NewRealDevice()
		}
		// 存回 vars 供后续 case 复用
		if heatnote == nil {
			heatnote = map[string]any{}
			env.Vars["HeatNote"] = heatnote
		}
		heatnote["mbus_dev"] = dev
	}

	// 串口配置：HeatNote["mbus"] 子 map 的小写键 port
	var port string
	if m, ok := heatnote["mbus"].(map[string]any); ok {
		port = _str(m, "port")

	}
	if port == "" {
		return nil, fmt.Errorf("未配置MBUS串口")
	}

	// 注入日志输出并连接（env.Log 是 core.Logger(Info(string))，mbus 契约
	// 要求 Info(args ...any)，用 adapter 适配，见 mbusLogAdapter）
	dev.SetLogger(mbusLogAdapter{log: env.Log})
	// --debug 模式：打印 MBus 发送/接收 hex（默认 false 与 Perl 行为一致）
	if debug, _ := heatnote["debug"].(bool); debug {
		dev.SetDebug(true)
	}
	if err := dev.Connect(ctx, port); err != nil {
		return nil, err
	}
	return dev, nil
}

// mbusLogAdapter 把 core.Logger 适配成 mbus.Logger。mbus 包契约的 Logger
// 要求 Info(args ...any)（core.Logger 提供 Info(category, msg)），故在调用侧
// 包装转发，避免改动 mbus 包。
type mbusLogAdapter struct {
	log core.Logger
}

func (a mbusLogAdapter) Info(args ...any) {
	if len(args) == 1 {
		if s, ok := args[0].(string); ok {
			a.log.Info("", s)
			return
		}
	}
	a.log.Info("", fmt.Sprint(args...))
}
