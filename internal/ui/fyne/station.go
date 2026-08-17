// Package fyneui provides a Fyne v2 implementation of core.UI and
// core.Logger.
package fyneui

import (
	"fmt"
	"strings"
	"sync"

	"blat/internal/logfile"
	"blat/internal/report"
	"blat/internal/serial"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// stationPanel 是一个工位的 UI 面板。与运行逻辑解耦：启动/停止的真实动作
// 由 App 层通过 SetOnStart / SetOnStop 注入；校验/提示由 App 层通过
// SetOnMessage 注入（在 Fyne 主线程弹 Message 框）。
type stationPanel struct {
	idx int

	onStart   func(sn, port string) error
	onStop    func()
	onMessage func(msg string)

	root *fyne.Container

	title       *widget.Label
	statusDot   *canvas.Circle
	statusLabel *widget.Label

	// currentSN 记录当前/最近一次测试的序列号；currentSNLabel 在标题下方
	// 常驻显示，方便操作员对账"这台面板测的是哪台设备"。
	currentSN      string
	currentSNLabel *widget.Label

	snEntry   *widget.Entry
	portSel   *widget.Select
	refreshBtn *widget.Button
	stopBtn   *widget.Button

	log      *SelectableRichText
	logScroll *container.Scroll

	mu     sync.Mutex
	state  stationState
	logf   *logfile.FileLogger
	logOff int64
	logGen int
}

// newStationPanel 创建第 idx 号工位面板（idx 从 1 开始，标题"设备1"…）。
func newStationPanel(idx int) *stationPanel {
	p := &stationPanel{idx: idx}

	p.title = widget.NewLabel(fmt.Sprintf("设备%d", idx))
	p.title.TextStyle = fyne.TextStyle{Bold: true}
	p.title.SizeName = theme.SizeNameHeadingText

	p.statusDot = canvas.NewCircle(themeColor(stIdle.ColorName()))
	p.statusDot.Resize(fyne.NewSize(12, 12))

	p.statusLabel = widget.NewLabel("空闲")
	p.statusLabel.Importance = importanceForState(stIdle)

	// 当前测试序列号显示：放在标题下方、输入区上方，独立一行常驻可见。
	// 无历史 SN 时显示"等待输入"，有 SN 时显示"当前：123456789012"。
	p.currentSNLabel = widget.NewLabel("当前：等待输入")
	p.currentSNLabel.TextStyle = fyne.TextStyle{Bold: true}

	snPlaceholder := widget.NewLabel("序列号")
	snPlaceholder.TextStyle = fyne.TextStyle{Bold: true}
	p.snEntry = widget.NewEntry()
	p.snEntry.SetPlaceHolder("输入序列号回车开始")
	p.snEntry.OnSubmitted = func(sn string) {
		p.handleStart(sn)
	}

	portPlaceholder := widget.NewLabel("串口")
	portPlaceholder.TextStyle = fyne.TextStyle{Bold: true}
	p.portSel = widget.NewSelect(nil, func(string) {})
	p.portSel.PlaceHolder = "选择串口"
	p.refreshBtn = widget.NewButton("刷新", func() {
		p.refreshPorts()
	})
	p.stopBtn = widget.NewButton("停止", func() {
		p.handleStop()
	})
	p.stopBtn.Importance = widget.DangerImportance
	p.stopBtn.Disable()

	p.log = NewSelectableRichText()
	p.logScroll = container.NewScroll(p.log)
	// 给日志框一个温和的边框，与单跑模式主日志框视觉一致。
	logFrame := newBottomBorder(
		newTopBorder(p.logScroll, theme.ColorNameInputBorder, 1),
		theme.ColorNameInputBorder, 1,
	)

	// 标题行：设备名 + 状态灯 + 状态文字，靠左紧凑排列。
	titleRow := container.NewHBox(p.title, layout.NewSpacer(), p.statusDot, p.statusLabel)

	// 当前序列号行：放在标题下方，独立且醒目。
	currentSNRow := container.NewHBox(p.currentSNLabel)

	// 输入区：序列号占整行；串口行标签+下拉+刷新；停止按钮占整行。
	portRow := container.NewHBox(portPlaceholder, p.portSel, p.refreshBtn)
	inputArea := container.NewVBox(snPlaceholder, p.snEntry, portRow, p.stopBtn)

	// 用分隔线把输入区与日志框分开，对齐工具栏底边风格。
	separator := newBottomBorder(container.NewVBox(), theme.ColorNameInputBorder, 1)

	// 根布局：顶部固定输入区，中部日志框拉伸。
	p.root = container.NewBorder(
		container.NewVBox(titleRow, currentSNRow, inputArea, separator),
		nil, nil, nil,
		logFrame,
	)

	// 初始填充串口列表。
	p.refreshPorts()

	return p
}

// SetOnStart 注入启动回调：操作员在 SN 输入框回车触发，参数(SN, 所选串口)；
// 回调返回 error 时通过 SetOnMessage 弹框提示。
func (p *stationPanel) SetOnStart(f func(sn, port string) error) {
	p.mu.Lock()
	p.onStart = f
	p.mu.Unlock()
}

// SetOnStop 注入停止回调：点停止按钮触发。
func (p *stationPanel) SetOnStop(f func()) {
	p.mu.Lock()
	p.onStop = f
	p.mu.Unlock()
}

// SetOnMessage 注入提示回调：校验失败等需要阻塞操作员的情况调用。
// 由 App 层实现为弹 Message 框（建议 danger=true）。
func (p *stationPanel) SetOnMessage(f func(msg string)) {
	p.mu.Lock()
	p.onMessage = f
	p.mu.Unlock()
}

// BindLog 绑定工位日志文件（工位启动成功后由 App 调用）；绑定后 RefreshLog 生效。
func (p *stationPanel) BindLog(lf *logfile.FileLogger) {
	p.mu.Lock()
	p.logf = lf
	p.logOff = 0
	p.logGen = 0
	if lf != nil {
		// 跳过文件已有内容，避免把旧日志灌进 UI。
		_, p.logOff, p.logGen, _ = lf.TailFrom(0, 0)
	}
	p.mu.Unlock()

	fyne.Do(func() {
		p.log.Clear()
	})
}

// RefreshLog 从绑定的 logf 增量读取并刷新日志框（内部 fyne.Do 保护；
// 供工位 logger 每次写入后调用）。逻辑照抄 app.go refreshLog 参数化版，
// 复用 parseLogLine/colorForEntry 恢复配色。
func (p *stationPanel) RefreshLog() {
	p.mu.Lock()
	lf := p.logf
	off := p.logOff
	gen := p.logGen
	p.mu.Unlock()

	if lf == nil || p.log == nil {
		return
	}

	text, size, newGen, cleared := lf.TailFrom(off, gen)

	fyne.Do(func() {
		if cleared {
			p.log.Clear()
		}
		if text != "" {
			var cur strings.Builder
			curColor := theme.ColorNameForeground
			curColorValid := false
			flush := func() {
				if cur.Len() > 0 {
					p.log.AppendSegment(curColor, cur.String())
					cur.Reset()
				}
			}
			for line := range strings.SplitSeq(text, "\n") {
				line = strings.TrimRight(line, "\r")
				if line == "" {
					continue
				}
				color := theme.ColorNameForeground
				if level, category, ok := parseLogLine(line); ok {
					color = colorForEntry(level, category)
				}
				if curColorValid && color != curColor {
					flush()
				}
				if cur.Len() > 0 {
					cur.WriteByte('\n')
				}
				cur.WriteString(line)
				curColor = color
				curColorValid = true
			}
			flush()
		}
		if p.logScroll != nil && (text != "" || cleared) {
			p.logScroll.ScrollToBottom()
		}
	})

	p.mu.Lock()
	p.logOff = size
	p.logGen = newGen
	p.mu.Unlock()
}

// SetState 驱动状态灯与状态文字（颜色用 stationState.ColorName()）：
// idle="空闲"、running="运行中"、done="完成"、fail="失败"、stopped="已停止"。
// 同时维护控件可用性：running 时 SN 输入/串口下拉/刷新禁用、停止启用；
// 其余状态反之。
func (p *stationPanel) SetState(s stationState, sum report.Summary) {
	p.mu.Lock()
	p.state = s
	p.mu.Unlock()

	_ = sum

	fyne.Do(func() {
		stateText := stateDisplayName(s)
		p.statusLabel.SetText(stateText)
		p.statusLabel.Importance = importanceForState(s)
		p.statusLabel.Refresh()

		p.statusDot.FillColor = themeColor(s.ColorName())
		p.statusDot.Refresh()

		if s == stRunning {
			p.snEntry.Disable()
			p.portSel.Disable()
			p.refreshBtn.Disable()
			p.stopBtn.Enable()
		} else {
			p.snEntry.Enable()
			p.portSel.Enable()
			p.refreshBtn.Enable()
			p.stopBtn.Disable()
			// 回到空闲时让 SN 框自动聚焦，方便操作员连续扫码。
			if s == stIdle {
				if c := fyne.CurrentApp().Driver().CanvasForObject(p.root); c != nil {
					c.Focus(p.snEntry)
				}
			}
		}
	})
}

// CanvasObject 返回面板根容器（供后续 HBox 三面板并排组装）。
func (p *stationPanel) CanvasObject() fyne.CanvasObject {
	return p.root
}

// handleStart 处理 SN 输入框回车。非运行中状态都可重新启动
//（idle/done/fail/stopped），运行中直接忽略。
func (p *stationPanel) handleStart(sn string) {
	p.mu.Lock()
	f := p.onStart
	port := p.portSel.Selected
	state := p.state
	p.mu.Unlock()

	if f == nil || state == stRunning {
		return
	}

	sn = strings.TrimSpace(sn)
	if sn == "" {
		// 必须异步弹框：onMessage 通常注入为 a.Message，其在主线程调用会
		// 阻塞等 pump 的 fyne.Do 弹框回复；而 pump 的 fyne.Do 又依赖主线程
		// 处理事件队列。go 起 goroutine 打破自死锁。
		go p.showMessage(fmt.Sprintf("设备%d：请输入序列号", p.idx))
		return
	}
	if err := validateStationSN(sn); err != nil {
		go p.showMessage(fmt.Sprintf("设备%d：%s", p.idx, err.Error()))
		return
	}
	if port == "" {
		go p.showMessage(fmt.Sprintf("设备%d：请选择串口", p.idx))
		return
	}

	if err := f(sn, port); err != nil {
		go p.showMessage(fmt.Sprintf("设备%d：%s", p.idx, err.Error()))
		return
	}

	// 启动成功：记录当前 SN、清空输入框、立即显示运行中。
	// startStation 已改为轻量外壳（重活放后台 goroutine），这里先置 running
	// 填上中间几百毫秒的空白期；adapter.OnPlanStart 再次 SetState(running) 时
	// old==new 不触发 onState（stationRun.SetState 语义），无重复渲染问题。
	p.mu.Lock()
	p.currentSN = sn
	p.mu.Unlock()
	fyne.Do(func() {
		p.currentSNLabel.SetText(fmt.Sprintf("当前：%s", sn))
		p.snEntry.SetText("")
		p.SetState(stRunning, report.Summary{})
	})
}

// handleStop 处理停止按钮点击。
func (p *stationPanel) handleStop() {
	p.mu.Lock()
	f := p.onStop
	p.mu.Unlock()

	if f != nil {
		f()
	}
}

// refreshPorts 重新枚举串口并更新下拉选项。
func (p *stationPanel) refreshPorts() {
	ports, err := serial.ListPorts()
	if err != nil {
		ports = nil
	}
	selected := ""
	fyne.Do(func() {
		if p.portSel.Selected != "" {
			// 保留当前选项，若新列表中仍存在。
			for _, opt := range ports {
				if opt == p.portSel.Selected {
					selected = opt
					break
				}
			}
		}
		p.portSel.Options = ports
		p.portSel.SetSelected(selected)
	})
}

// showMessage 通过注入的 onMessage 提示操作员（通常实现为弹框）。
func (p *stationPanel) showMessage(msg string) {
	p.mu.Lock()
	f := p.onMessage
	p.mu.Unlock()
	if f != nil {
		f(msg)
	}
}

// validateStationSN 校验工位面板输入的序列号格式。
// 面板模式为 PTVB1 MBUS 整机测试，真实用例 parseMbusID 要求纯数字；
// 为统一工厂扫码枪输入并提前拦截错误，固定为 12 位纯数字（不兼容蓝牙单跑
// 模式的 W 前缀，那是另一套 plan 与界面）。
func validateStationSN(sn string) error {
	if len(sn) != 12 {
		return fmt.Errorf("序列号必须是 12 位数字（当前 %d 位）", len(sn))
	}
	for _, c := range sn {
		if c < '0' || c > '9' {
			return fmt.Errorf("序列号只能包含数字")
		}
	}
	return nil
}

// stateDisplayName 返回状态的中文显示名。
func stateDisplayName(s stationState) string {
	switch s {
	case stIdle:
		return "空闲"
	case stRunning:
		return "运行中"
	case stDone:
		return "完成"
	case stFail:
		return "失败"
	case stStopped:
		return "已停止"
	default:
		return s.String()
	}
}

// importanceForState 把 stationState.ColorName() 映射到 widget.Importance，
// 让状态文字颜色与状态灯一致。
func importanceForState(s stationState) widget.Importance {
	switch s.ColorName() {
	case theme.ColorNameDisabled:
		return widget.LowImportance
	case theme.ColorNameWarning:
		return widget.WarningImportance
	case theme.ColorNameSuccess:
		return widget.SuccessImportance
	case theme.ColorNameError:
		return widget.DangerImportance
	default:
		return widget.MediumImportance
	}
}
