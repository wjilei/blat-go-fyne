package cases

import (
	"context"
	"errors"
	"fmt"
	"time"

	"blat/internal/core"
	"blat/internal/device/mbus"
)

// WireValveMBusReadMotorCase 翻译自 Perl
// BLAT::APP::Heat::Cases::wire_valve::dev_normal_check_motor。
// 流程：
//  1. 通过 M-Bus 发起电机校准（CaliValveByMbus）；
//  2. 轮询读取电机状态，与期望值比对；未达期望则按轮询间隔重试；
//  3. 轮询成功后 sleep 1s，再读一次设备信息，校验 alarm==0 确认状态
//     已保存。
//
// 不判断 test_mode==normal——本 Run 场景本身就是 normal（产线 normal
// 计划下才会执行此用例，对应 Perl 里的 if 分支在 Run 路径恒为真）。
type WireValveMBusReadMotorCase struct {
	keyState  string        // 期望状态，默认 "01"（电机启动完成）
	pollTimes int           // 轮询次数，默认 180
	pollSleep time.Duration // 轮询间隔，默认 1s
}

func (c *WireValveMBusReadMotorCase) Name() string {
	return "wire_valve_mbus_read_motor"
}

// Configure 读取 plan 的自定义参数（对应 Perl
// BLAT::APP::Heat::Cases::wire_valve::dev_normal_check_motor_args 的默认值：
// 期望状态="01"、轮询次数=180、轮询间隔=1s）。键名中文优先，
// 同时兼容英文键 key_state；未知键忽略不报错。
func (c *WireValveMBusReadMotorCase) Configure(args map[string]any) error {
	// 默认值
	c.keyState = "01"
	c.pollTimes = 180
	c.pollSleep = time.Second

	// 期望状态：中文键优先，兼容英文 key_state
	if v, ok := args["期望状态"].(string); ok && v != "" {
		c.keyState = v
	} else if v, ok := args["key_state"].(string); ok && v != "" {
		c.keyState = v
	}

	if v := _int(args, "轮询次数"); v > 0 {
		c.pollTimes = v
	}
	if v := _int(args, "轮询间隔"); v > 0 {
		c.pollSleep = time.Duration(v) * time.Second
	}
	return nil
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

	// 1. 校准电机（对应 Perl L247 CaliValveByMbus）。mock 模式直接成功；
	// real 模式发 BB1F SET_VALVE 帧（open_pre=0, calc_day=255）。
	if err := dev.CaliValveByMbus(ctx, mac); err != nil {
		return fmt.Errorf("初始化电机失败: %w", err)
	}

	// 1.5 弹框让用户观察阀门是否转动（对齐 Perl `ui_show_judgment`）：
	// 校准命令发出后人工肉眼确认电机已转。选"否"→ 用例直接失败；
	// 选"是"→ 继续后面的轮询校验。回车 = 「是」（Fyne Confirm 默认
	// 焦点在「是」按钮上，参见 internal/ui/fyne/app.go yesNoCh 处理）。
	if err := askValveTurned(ctx, env); err != nil {
		return err
	}

	// 2. 轮询读取电机状态，直到匹配期望值或轮询次数耗尽
	for i := 0; i < c.pollTimes; i++ {
		ret, rerr := dev.MBusReadMotor(ctx, mac)
		retText := "undef"
		if rerr == nil {
			retText = ret
		}
		env.Log.Info(fmt.Sprintf("状态值: %s, 期望值：%s", retText, c.keyState))
		if rerr == nil && ret == c.keyState {
			// 电机启动完成，做一次"状态保存"校验
			if err := c.checkStateSaved(ctx, dev, mac, env); err != nil {
				return err
			}
			return nil
		}
		if err := _sleep(ctx, c.pollSleep); err != nil {
			return err
		}
	}
	// 对应 Perl ok 0 "电机未启动完成"
	return fmt.Errorf("电机未启动完成")
}

// checkStateSaved 对应 Perl L272-285：sleep 1 后读设备信息（BB1E
// _MbusReadInfo），校验 alarm==0 表示状态已保存。失败返回 error。
func (c *WireValveMBusReadMotorCase) checkStateSaved(ctx context.Context, dev *mbus.Device, mac string, env *core.Env) error {
	if err := _sleep(ctx, time.Second); err != nil {
		return err
	}
	info, err := dev.MbusReadInfo(ctx, mac)
	if err != nil {
		return fmt.Errorf("读取设备当前信息失败: %w", err)
	}
	if info.Alarm != 0 {
		return fmt.Errorf("状态保存失败: alarm: %d", info.Alarm)
	}
	return nil
}

func init() {
	Register("HeatSuite::wire_valve_mbus_read_motor", func() (core.Case, error) {
		return &WireValveMBusReadMotorCase{}, nil
	})
}

// askValveTurned 弹一个「是/否」确认框，让操作员确认电机校准后阀门已转动。
// 选「是」→ 返回 nil 继续；选「否」→ 返回 error（用例失败）；
// ctx 取消（Stop 按钮 / 关窗）→ 返回 ctx.Err()。回车默认「是」（参见
// internal/ui/fyne/app.go yesNoCh 处理，默认焦点在「是」上）。
func askValveTurned(ctx context.Context, env *core.Env) error {
	ok, err := env.UI.Confirm(ctx, "请观察阀门是否已转动（电机校准命令已发出）？选「否」将停止并失败")
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("用户确认阀门未转动")
	}
	env.Log.Info("已确认阀门转动")
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
// 要求 Info(args ...any)（core.Logger 只提供 Info(string)），故在调用侧
// 包装转发，避免改动 mbus 包。
type mbusLogAdapter struct {
	log core.Logger
}

func (a mbusLogAdapter) Info(args ...any) {
	if len(args) == 1 {
		if s, ok := args[0].(string); ok {
			a.log.Info(s)
			return
		}
	}
	a.log.Info(fmt.Sprint(args...))
}
