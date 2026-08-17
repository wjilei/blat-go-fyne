package fyneui

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"blat/internal/config"
	"blat/internal/core"
	"blat/internal/logfile"
	"blat/internal/report"
	"blat/internal/runtime"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// stationState 工位状态机：idle→running→done/fail/stopped。
type stationState int

const (
	stIdle stationState = iota
	stRunning
	stDone
	stFail
	stStopped
)

// String 返回状态的可读名。
func (s stationState) String() string {
	switch s {
	case stIdle:
		return "idle"
	case stRunning:
		return "running"
	case stDone:
		return "done"
	case stFail:
		return "fail"
	case stStopped:
		return "stopped"
	default:
		return fmt.Sprintf("stationState(%d)", int(s))
	}
}

// ColorName 返回状态灯颜色：idle=灰 disabled、running=黄 warning、
// done=绿 success、fail/stopped=红 error。
func (s stationState) ColorName() fyne.ThemeColorName {
	switch s {
	case stRunning:
		return theme.ColorNameWarning
	case stDone:
		return theme.ColorNameSuccess
	case stFail, stStopped:
		return theme.ColorNameError
	default: // stIdle
		return theme.ColorNameDisabled
	}
}

// stationRun 一个工位的运行上下文（与 widget 无关，仅逻辑+资源句柄）。
type stationRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	env    *core.Env
	logf   *logfile.FileLogger
	logOff int64
	logGen int
	plan   *config.Plan
	reg    *runtime.Registry
	// sn 当前正在测试的序列号；空表示未开始。用于三面板序列号互斥检查
	// （startStation 持 a.mu 读，占位/真 run 构造时写入）。
	sn    string
	mu    sync.Mutex
	state stationState
	// summary 完成时的最终汇总（SetState 每次调用都会更新）。
	summary report.Summary
	// onState 可选回调，UI 层注册；仅在状态真正迁移（old != new）时触发，
	// 在锁外调用避免死锁。
	onState func(old, new stationState, sum report.Summary)
}

// SetState 状态迁移（带锁）。迁移发生时调用 onState（在锁外调用避免死锁）；
// 相同状态不触发回调，但 summary 始终更新。
func (r *stationRun) SetState(s stationState, sum report.Summary) {
	r.mu.Lock()
	old := r.state
	r.state = s
	r.summary = sum
	cb := r.onState
	r.mu.Unlock()
	if cb != nil && old != s {
		cb(old, s, sum)
	}
}

// State 返回当前状态。
func (r *stationRun) State() stationState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// deepCopyVars 递归深拷贝 env.Vars（map[string]any）：
//   - map/slice 递归拷贝；*mbus.Device/*bluetooth.Device 等指针与基本类型原样引用
//   - 特殊规则：若拷贝结果里存在 HeatNote map，删除其 "mbus_dev" 和 "bluetooth" 键
//     （防止单跑模式遗留的设备实例被三工位共享——串口独占冲突；_ensureMBUS 会按
//     工位串口重建）。HeatNote 不存在则跳过。
func deepCopyVars(vars map[string]any) map[string]any {
	if vars == nil {
		return nil
	}
	out := make(map[string]any, len(vars))
	for k, v := range vars {
		out[k] = deepCopyValue(v)
	}
	if hn, ok := out["HeatNote"].(map[string]any); ok {
		delete(hn, "mbus_dev")
		delete(hn, "bluetooth")
	}
	return out
}

// deepCopyValue 递归深拷贝 map/slice；指针与基本类型原样引用。
func deepCopyValue(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map:
		out := reflect.MakeMapWithSize(rv.Type(), rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			val := iter.Value()
			if val.Kind() == reflect.Interface && val.IsNil() {
				out.SetMapIndex(iter.Key(), reflect.Zero(rv.Type().Elem()))
				continue
			}
			out.SetMapIndex(iter.Key(), reflect.ValueOf(deepCopyValue(val.Interface())))
		}
		return out.Interface()
	case reflect.Slice:
		out := reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
		for i := 0; i < rv.Len(); i++ {
			ev := rv.Index(i)
			if ev.Kind() == reflect.Interface && ev.IsNil() {
				continue
			}
			out.Index(i).Set(reflect.ValueOf(deepCopyValue(ev.Interface())))
		}
		return out.Interface()
	default:
		return v
	}
}

// stationUI 给工位 env.UI 加前缀的 core.UI 适配器：Prompt/WaitContinue/
// Message/Confirm 的文案前加 "【设备N】" 前缀，ctx 与错误原样透传。
// Info 是日志方法，不属于交互文案，直接透传不加前缀。
type stationUI struct {
	base   core.UI
	prefix string
}

// newStationUI 构造 stationUI。stationName 如 "设备1"，前缀为 "【设备1】"。
func newStationUI(base core.UI, stationName string) *stationUI {
	return &stationUI{base: base, prefix: "【" + stationName + "】"}
}

func (u *stationUI) Info(category, msg string) {
	u.base.Info(category, msg)
}

func (u *stationUI) Prompt(ctx context.Context, label, def string) (string, error) {
	return u.base.Prompt(ctx, u.prefix+label, def)
}

func (u *stationUI) WaitContinue(ctx context.Context, msg string) error {
	return u.base.WaitContinue(ctx, u.prefix+msg)
}

func (u *stationUI) Message(ctx context.Context, msg string, danger bool) error {
	return u.base.Message(ctx, u.prefix+msg, danger)
}

func (u *stationUI) Confirm(ctx context.Context, msg string, danger bool) (bool, error) {
	return u.base.Confirm(ctx, u.prefix+msg, danger)
}

// stationAdapter 实现 report.Reporter，把 runner 事件映射到 stationRun 状态：
//
//	OnPlanStart → running（summary 记录 TotalNum）
//	OnCaseStart/OnCaseStop → 无状态变化（仅透传，不动状态）
//	OnPlanStop(sum) → ctx.Err()!=nil ? stopped : (sum.FailNum>0 || sum.Result==0 ? fail : done)
type stationAdapter struct {
	run *stationRun
}

func (a *stationAdapter) OnPlanStart(total int, startTime time.Time) {
	a.run.SetState(stRunning, report.Summary{TotalNum: total})
}

func (a *stationAdapter) OnCaseStart(seq int, cr report.CaseReport) {
	// 无状态变化
}

func (a *stationAdapter) OnCaseStop(seq int, cr report.CaseReport) {
	// 无状态变化
}

func (a *stationAdapter) OnPlanStop(sum report.Summary) {
	if a.run.ctx != nil && a.run.ctx.Err() != nil {
		a.run.SetState(stStopped, sum)
		return
	}
	if sum.FailNum > 0 || sum.Result == 0 {
		a.run.SetState(stFail, sum)
		return
	}
	a.run.SetState(stDone, sum)
}