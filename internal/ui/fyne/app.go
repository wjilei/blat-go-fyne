// Package fyneui provides a Fyne v2 implementation of core.UI and
// core.Logger. It is a minimal port of the BLAT Perl TkGUI layout:
//
//	+--------------------------------------------------+
//	|  toolbar:  [load]  [start]  [stop]  [clear]      |
//	+----------------+---------------------------------+
//	|  case tree     |   log panel (RichText,          |
//	|  (planned /    |    color-coded by level)        |
//	|   running /    |                                 |
//	|   ok / fail)   |                                 |
//	+----------------+---------------------------------+
//	|  status: idle   |              [progress]        |
//	+--------------------------------------------------+
//
// Threading model (single process):
//
//	fyne main thread ── ShowAndRun() ── drives event loop, owns widgets
//	    ^                                 |
//	    | fyne.Do(fn)                     | win.SetOnClosed
//	    |                                 v
//	pump goroutine ── close(shutdown) ── <all blocking chans>
//
//	runner goroutine ── StartRun() ──> ctx with cancel
//	                      |
//	                      | Prompt(ctx) / WaitContinue(ctx)
//	                      v
//	                  pump goroutine  ── fyne.Do  ──> fyne main thread
//
// The toolbar Load/Start buttons own the plan lifecycle: Load reloads the
// plan into the case tree, Start launches the runner goroutine (see
// loadPlan / startRun).
//
// All widget creation happens on the Fyne main thread (fyne.Do). The
// runner goroutine and the pump goroutine only touch the log ring buffer
// and the request channels; both are mutex-guarded.
package fyneui

import (
	"bytes"
	"context"
	"fmt"
	"image/color"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"blat/internal/config"
	"blat/internal/core"
	"blat/internal/report"
	"blat/internal/runtime"
	"blat/internal/serial"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

const defaultLogCap = 1000

// Tree 节点 ID：根+两个分支用 sentinel ID，case 子节点用 "case:<idx>" 前缀。
const (
	nodePlan    = "__plan__"
	nodeCasePfx = "case:"
	// planPlaceholder 是计划下拉框的第一个选项；选中它表示不选任何计划。
	planPlaceholder = "请选择测试计划"
)

type row struct {
	title  string
	name   string
	result string
	detail string
}

// PlanItem 是计划下拉框里的一个选项：显示名 + 对应的 plan.yml 路径。
type PlanItem struct {
	Name string // 下拉框显示名
	Path string // plan.yml 路径（相对工作目录）
}

// logEntry is one ring-buffer record. The widget renders one colored
// RichText segment per entry.
type logEntry struct {
	Level    string
	Category string
	Text     string
}

type promptReq struct {
	label string
	def   string
	reply chan promptReply
}

type promptReply struct {
	val string
	err error
}

type confirmReq struct {
	msg   string
	reply chan struct{}
}

// App is the Fyne-based UI. It implements both core.UI and core.Logger.
type App struct {
	fa  fyne.App
	win fyne.Window

	log       *widget.RichText
	logScroll *container.Scroll
	tree      *widget.Tree
	status    *widget.Label
	prog      *widget.ProgressBar
	startBtn  *widget.Button
	configBtn *widget.Button
	planSel   *widget.Select // 测试计划下拉框
	planItems []PlanItem     // 下拉框选项（显示名→plan.yml 路径），受 mu 保护

	// varsFile 是配置（MBUS 串口等）持久化的目标文件路径；相对工作目录。
	// 启动时 main 用 config.LoadEnv(varsFile) 读入 env.Vars，配置弹框
	// 写回同一文件，保证"下次启动自动加载"。
	varsFile string

	mu         sync.Mutex
	tapPartial bytes.Buffer // 暂存未完成（无换行结尾）的 TAP 半行
	rows       []row
	logBuf     []logEntry
	logCap     int
	cat        string
	serNum     string // 启动弹框输入的序列号，受 mu 保护

	promptCh  chan promptReq
	confirmCh chan confirmReq
	shutdown  chan struct{}
	once      sync.Once

	runMu  sync.Mutex
	cancel context.CancelFunc
	runCtx context.Context

	plan *config.Plan
	env  *core.Env
	reg  *runtime.Registry
}

// New constructs and shows the main window. Call Run to start the Fyne
// event loop. The returned App is safe to use from any goroutine; all UI
// mutations are marshalled onto the Fyne main thread via fyne.Do.
func New(title string) *App {
	// app.New() is deprecated in fyne v2.8: it calls NewWithID(""), which
	// trips the Preferences API check and breaks widget.Entry's autofill
	// (and silently wedges the window on some platforms). Hard-code a
	// stable ID for now; later we can read it from a FyneApp.toml.
	fa := app.NewWithID("blat-go.hello")
	// 工厂车间多为浅色屏幕；固定浅色主题，避免系统深色模式下日志对比度过低。
	fa.Settings().SetTheme(theme.LightTheme())
	win := fa.NewWindow(title)
	a := &App{
		fa:        fa,
		win:       win,
		logCap:    defaultLogCap,
		varsFile:  "confs/env.yml",
		promptCh:  make(chan promptReq, 8),
		confirmCh: make(chan confirmReq, 8),
		shutdown:  make(chan struct{}),
	}
	a.build()
	win.SetOnClosed(func() {
		a.once.Do(func() { close(a.shutdown) })
		a.StopRun()
	})
	a.startPump()
	win.Show()
	return a
}

// themeColor 把 fyne.ThemeColorName 解析成 color.Color。Fyne v2 的
// canvas.* 字段类型是 color.Color，而 theme.ColorName* 是 string 别名，
// 不能直接赋——必须走 Theme().Color(name, variant)。
func themeColor(name fyne.ThemeColorName) color.Color {
	return fyne.CurrentApp().Settings().Theme().Color(name, fyne.CurrentApp().Settings().ThemeVariant())
}

// borderLayout 把第一个对象当 content 排版，第二个对象（通常是画线
// 的 canvas.Rectangle）铺满整个容器。pad 计入 MinSize。
type borderLayout struct {
	padTop, padBottom, padLeft, padRight float32
}

func (b borderLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	if len(objs) < 2 {
		return
	}
	objs[0].Move(fyne.NewPos(b.padLeft, b.padTop))
	objs[0].Resize(fyne.NewSize(
		size.Width-b.padLeft-b.padRight,
		size.Height-b.padTop-b.padBottom,
	))
	objs[1].Resize(size)
}

func (b borderLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	if len(objs) == 0 {
		return fyne.NewSize(0, 0)
	}
	inner := objs[0].MinSize()
	return fyne.NewSize(
		inner.Width+b.padLeft+b.padRight,
		inner.Height+b.padTop+b.padBottom,
	)
}

// newBordered 给任意 CanvasObject 包一层指定颜色/线宽的矩形边框。
// 颜色用 fyne.ThemeColorName，浅色/深色主题自动适配。
func newBordered(content fyne.CanvasObject, lineColor fyne.ThemeColorName, lineWidth float32) *fyne.Container {
	rect := canvas.NewRectangle(color.Transparent)
	rect.StrokeColor = themeColor(lineColor)
	rect.StrokeWidth = lineWidth
	return container.New(
		borderLayout{
			padLeft: lineWidth, padRight: lineWidth,
			padTop: lineWidth, padBottom: lineWidth,
		},
		content, rect,
	)
}

// newBottomBorder 只在 content 下方画一条水平线。
// 用 layout.BorderLayout 把 canvas.Line 贴底拉伸，content 区域不动。
func newBottomBorder(content fyne.CanvasObject, lineColor fyne.ThemeColorName, lineWidth float32) *fyne.Container {
	line := canvas.NewLine(themeColor(lineColor))
	line.StrokeWidth = lineWidth
	return container.New(layout.NewBorderLayout(nil, line, nil, nil), content, line)
}

// newBottomBorder 只在 content 下方画一条水平线。
// 用 layout.BorderLayout 把 canvas.Line 贴底拉伸，content 区域不动。
func newTopBorder(content fyne.CanvasObject, lineColor fyne.ThemeColorName, lineWidth float32) *fyne.Container {
	line := canvas.NewLine(themeColor(lineColor))
	line.StrokeWidth = lineWidth
	return container.New(layout.NewBorderLayout(line, nil, nil, nil), content, line)
}

// minSizeLayout 让容器 MinSize 至少为给定值，子对象铺满容器。
// 用 container.New(minSizeLayout, obj) 包装，而不是包装对象本身：
// Fyne v2.8 的 *fyne.Container 只实现 CanvasObject（不实现 fyne.Widget），
// 若在 SetContent 外包一层非 Container 对象会破坏 Fyne 对 Container 的
// 专用渲染路径 → 整窗空白。自定义 layout 的 Container 仍走正常渲染。
// 用途：规避 Fyne v2.8 glfw 在 Windows 上最小化恢复时把窗口 clamp 到
// content MinSize（极小值）的缺陷——固定合理下限让恢复路径回到初始尺寸。
type minSizeLayout struct {
	min fyne.Size
}

func (l minSizeLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	return l.min
}

func (l minSizeLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	for _, o := range objs {
		o.Resize(size)
		o.Move(fyne.NewPos(0, 0))
	}
}

// minWidthLayout 把单一子对象的 MinSize 宽度撑到至少 w 像素，高度保持
// 原样。用于把 Entry 等默认较窄的控件放进 PopUp / dialog 时撑出合理宽度，
// 同时让高度由 VBox 自然计算（不像 minSizeLayout 会锁死高度）。
type minWidthLayout struct{ w float32 }

func (l minWidthLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	if len(objs) == 0 {
		return fyne.NewSize(0, 0)
	}
	m := objs[0].MinSize()
	if m.Width < l.w {
		m.Width = l.w
	}
	return m
}

func (l minWidthLayout) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	for _, c := range objs {
		c.Resize(size)
	}
}

func (a *App) build() {
	a.log = widget.NewRichText()
	a.log.Wrapping = fyne.TextWrapWord
	a.logScroll = container.NewVScroll(a.log)
	a.status = widget.NewLabel("就绪")
	a.prog = widget.NewProgressBar()

	// 单行模板：图标 + 标题 + 弹性间隔 + 状态标签。
	// update 回调依赖此结构顺序，索引 0/1/3 是固定位。
	// ⚠ create 必须每次返回新对象：Fyne 按可见行数创建多个 cell，
	// 共享同一个 CanvasObject 会让所有行显示为最后一次 update 的内容。
	a.tree = widget.NewTree(
		// childIDs: 根返回两个分支；plan 分支返回所有 case id。
		func(uid string) []string {
			switch uid {
			case "":
				return []string{nodePlan}
			case nodePlan:
				a.mu.Lock()
				ids := make([]string, len(a.rows))
				for i := range a.rows {
					ids[i] = nodeCasePfx + strconv.Itoa(i)
				}
				a.mu.Unlock()
				return ids
			}
			return nil
		},
		// isBranch: 根与两个 sentinel 分支可展开。
		func(uid string) bool {
			return uid == "" || uid == nodePlan
		},
		// create: 每次返回新的 cell 模板（不可共享）。
		func(branch bool) fyne.CanvasObject {
			return container.NewHBox(
				widget.NewIcon(theme.DocumentIcon()), // [0] 图标
				widget.NewLabel("template"),          // [1] 标题
				layout.NewSpacer(),                   // [2] 占位
				widget.NewLabel(""),                  // [3] 状态
			)
		},
		// update: 按 uid 渲染。
		func(uid string, branch bool, o fyne.CanvasObject) {
			c := o.(*fyne.Container)
			ic := c.Objects[0].(*widget.Icon)
			lb := c.Objects[1].(*widget.Label)
			st := c.Objects[3].(*widget.Label)

			switch uid {
			case nodePlan:
				ic.SetResource(theme.ListIcon())
				a.mu.Lock()
				n := len(a.rows)
				a.mu.Unlock()
				lb.SetText(fmt.Sprintf("测试计划(总数:%d)", n))
				st.SetText("")

			default:
				if !strings.HasPrefix(uid, nodeCasePfx) {
					return
				}
				idx, err := strconv.Atoi(strings.TrimPrefix(uid, nodeCasePfx))
				if err != nil {
					return
				}
				a.mu.Lock()
				var r row
				if idx >= 0 && idx < len(a.rows) {
					r = a.rows[idx]
				} else {
					a.mu.Unlock()
					return
				}
				a.mu.Unlock()
				ic.SetResource(theme.FileIcon())
				lb.SetText(fmt.Sprintf("%d. %s", idx, r.title))
				st.SetText(fmt.Sprintf("[%s]", r.result))
			}
		},
	)

	a.startBtn = widget.NewButtonWithIcon("开始测试", theme.MediaPlayIcon(), func() {
		switch a.startBtn.Text {
		case "开始测试":
			a.promptSerialThenRun()
		case "结束测试":
			a.StopRun()
		}
	})

	a.configBtn = widget.NewButtonWithIcon("配置", theme.SettingsIcon(), func() {
		a.promptConfig()
	})

	// 测试计划下拉框：第一项固定为"请选择测试计划"（等价于未选择计划），
	// 选项列表由 main 通过 SetPlanList 注入。
	a.planSel = widget.NewSelect([]string{planPlaceholder}, func(name string) {
		a.onPlanSelected(name)
	})
	a.planSel.PlaceHolder = planPlaceholder

	tb := newBottomBorder(container.NewHBox(a.startBtn, a.configBtn, a.planSel), theme.ColorNameInputBorder, 1)

	statusBar := newTopBorder(container.NewHBox(a.status, layout.NewSpacer(), a.prog), theme.ColorNameInputBorder, 1)

	// 程序主场口，左右两栏，左窄右宽
	mainFrame := container.NewHSplit(a.tree, a.logScroll)
	mainFrame.SetOffset(0.3)
	// 包一层 MinSize 下限：Fyne v2.8 glfw 在 Windows 上最小化恢复时
	// 会把窗口 clamp 到 content 的 MinSize（极小），导致窗口变小。
	// 用自定义 layout 的 Container 保证 center 区域 MinSize ≥ 960x640，
	// 恢复路径会回到初始尺寸。
	center := container.New(minSizeLayout{min: fyne.NewSize(960, 640)}, mainFrame)
	a.win.SetContent(container.NewBorder(tb, statusBar, nil, nil, center))
	// Fyne Tree 的根节点（t.Root=""）不可见，且 IsBranchOpen 默认 false：
	// 若不显式展开根，walk() 不会下钻到 childUIDs("") 返回的子节点。
	// 在此一次性把根与 plan/config 分支全部 open，使默认状态可见、可点击。
	a.tree.OpenAllBranches()
	a.win.Resize(fyne.NewSize(960, 640))
}

// startPump launches a dispatcher that takes prompt/confirm requests from
// the runner goroutine and creates their dialogs on the Fyne main thread
// via fyne.Do. It exits when shutdown is closed.
func (a *App) startPump() {
	go func() {
		for {
			select {
			case <-a.shutdown:
				return
			case req := <-a.promptCh:
				fyne.Do(func() {
					entry := widget.NewEntry()
					entry.SetText(req.def)
					d := dialog.NewCustomConfirm(req.label, "OK", "取消", entry, func(ok bool) {
						var r promptReply
						if ok {
							r.val = entry.Text
						} else {
							r.err = fmt.Errorf("cancelled")
						}
						select {
						case req.reply <- r:
						case <-a.shutdown:
						}
					}, a.win)
					d.Show()
				})
			case req := <-a.confirmCh:
				fyne.Do(func() {
					d := dialog.NewConfirm("请继续", req.msg, func(_ bool) {
						select {
						case req.reply <- struct{}{}:
						case <-a.shutdown:
						}
					}, a.win)
					d.Show()
				})
			}
		}
	}()
}

// Run blocks until the window is closed.
func (a *App) Run() { a.win.ShowAndRun() }

// ---- run control ----

// StartRun cancels any previous run and returns a fresh cancellable
// context plus the cancel function. The cancel function is also stored
// internally so the toolbar Stop button can invoke it.
func (a *App) StartRun() (context.Context, context.CancelFunc) {
	a.runMu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.runCtx = ctx
	a.runMu.Unlock()
	return ctx, cancel
}

// StopRun cancels the in-flight run, if any. Safe to call multiple times.
func (a *App) StopRun() {
	a.runMu.Lock()
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.runMu.Unlock()
}

// running reports whether a run is currently in flight (its ctx not done).
func (a *App) running() bool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.cancel != nil && a.runCtx != nil && a.runCtx.Err() == nil
}

// runFinished clears the in-flight run state. It is called by the runner
// goroutine after RunPlan returns. The ctx guard keeps a stale completion
// from clearing a newer run (e.g. after the user restarts mid-run).
// runFinished clears the in-flight run state. It is called by the runner
// goroutine after RunPlan returns. The ctx guard keeps a stale completion
// from clearing a newer run (e.g. after the user restarts mid-run).
//
// 注意：本函数不能直接改 widget——它从 runner goroutine 调用，widget
// 必须在 fyne 主线程改。UI 收尾交给 guiAdapter.OnPlanStop。
func (a *App) runFinished(ctx context.Context) {
	a.runMu.Lock()
	if a.runCtx == ctx {
		a.cancel = nil
		a.runCtx = nil
	}
	a.runMu.Unlock()
}

// ---- plan loading & running (toolbar) ----

// Attach wires the plan, environment and registry that the toolbar Start
// button uses to launch a run. Call it once after New and before Run.
func (a *App) Attach(plan *config.Plan, env *core.Env, reg *runtime.Registry) {
	a.mu.Lock()
	a.plan = plan
	a.env = env
	a.reg = reg
	a.mu.Unlock()
}

// loadPlan opens a file picker for a YAML plan, reloads it and resets the
// case tree. If a run is in flight it is stopped first.
func (a *App) loadPlan() {
	a.stopIfRunning()
	fd := dialog.NewFileOpen(func(reader fyne.URIReadCloser, err error) {
		if err != nil {
			a.Error("open plan: " + err.Error())
			return
		}
		if reader == nil {
			return // user cancelled the dialog
		}
		defer reader.Close()
		path := reader.URI().Path()
		plan, err := config.LoadPlan(path)
		if err != nil {
			a.Error(err.Error())
			return
		}
		a.mu.Lock()
		a.plan = plan
		a.rows = a.rows[:0]
		a.mu.Unlock()
		a.tree.Refresh()
		for _, c := range plan.Cases {
			title := c.Title
			if title == "" {
				title = c.Name
			}
			a.AddRow(title, c.Name)
		}
		a.SetStatus("loaded " + path)
	}, a.win)
	fd.SetFilter(storage.NewExtensionFileFilter([]string{".yml", ".yaml"}))
	fd.Show()
}

// stopIfRunning 若正在运行则先停止并提示。切换/清空 plan 前调用。
func (a *App) stopIfRunning() {
	if a.running() {
		a.StopRun()
		a.Warn("已停止当前运行，请重新选择 plan")
	}
}

// SetPlanList 注入下拉框的计划选项并设置初始选中项。
//
// selectPath 为空时停留在"请选择测试计划"（即不选任何计划，左侧树为空）；
// 非空时按路径匹配选项并触发加载。必须放在 Attach 之后、Run 之前调用——
// 设置初始选中会触发计划加载，加载逻辑需要 a.env 写入 env.Vars 的计划路径。
func (a *App) SetPlanList(items []PlanItem, selectPath string) {
	a.mu.Lock()
	a.planItems = append([]PlanItem(nil), items...)
	opts := make([]string, 0, len(items)+1)
	opts = append(opts, planPlaceholder)
	for _, it := range items {
		opts = append(opts, it.Name)
	}
	a.mu.Unlock()

	a.planSel.SetOptions(opts)
	idx := 0
	if selectPath != "" {
		for i, it := range items {
			if filepath.Clean(it.Path) == filepath.Clean(selectPath) {
				idx = i + 1
				break
			}
		}
	}
	a.planSel.SetSelectedIndex(idx)
}

// onPlanSelected 是下拉框的回调：选中某个测试计划则加载并填入左侧树，
// 选中占位项"请选择测试计划"则清空 plan 与树。
func (a *App) onPlanSelected(name string) {
	if name == "" || name == planPlaceholder {
		a.clearPlan()
		return
	}
	path := a.planPath(name)
	if path == "" {
		a.Warn("未知计划: " + name)
		return
	}
	a.loadPlanByPath(path)
}

// planPath 按显示名查 plan.yml 路径。
func (a *App) planPath(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, it := range a.planItems {
		if it.Name == name {
			return it.Path
		}
	}
	return ""
}

// loadPlanByPath 加载 plan.yml：清空并重建左侧用例树，把计划文件路径写入
// env.Vars["HeatNote"]["plan"] 供 case 运行时做判断。
func (a *App) loadPlanByPath(path string) {
	a.stopIfRunning()
	plan, err := config.LoadPlan(path)
	if err != nil {
		a.Error(err.Error())
		return
	}
	a.mu.Lock()
	a.plan = plan
	a.rows = a.rows[:0]
	a.mu.Unlock()
	a.tree.Refresh()
	for _, c := range plan.Cases {
		title := c.Title
		if title == "" {
			title = c.Name
		}
		a.AddRow(title, c.Name)
	}
	a.setPlanVar(path)
	a.SetStatus("loaded " + path)
}

// clearPlan 清空当前 plan 与用例树（下拉框停在"请选择测试计划"）。
func (a *App) clearPlan() {
	a.stopIfRunning()
	a.mu.Lock()
	a.plan = nil
	a.rows = a.rows[:0]
	a.mu.Unlock()
	a.tree.Refresh()
	a.setPlanVar("")
	a.SetStatus("未选择测试计划")
}

// setPlanVar 把当前计划文件路径写入 env.Vars["HeatNote"]["plan"]；
// path 为空时删除该键。env 尚未 Attach 时直接返回。
func (a *App) setPlanVar(path string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.env == nil {
		return
	}
	hn, _ := a.env.Vars["HeatNote"].(map[string]any)
	if hn == nil {
		hn = map[string]any{}
	}
	if path == "" {
		delete(hn, "plan")
	} else {
		hn["plan"] = path
	}
	a.env.Vars["HeatNote"] = hn
}

// promptConfig 弹出配置表单：当前只有一项——MBUS 串口下拉框。
//
// 数据流：
//  1. 列串口（serial.ListPorts）。列表为空时弹一个"无可用串口"的提示对话框。
//  2. 从 env.Vars["HeatNote"]["mbus"] 读已保存的端口作为 Select 初值。
//  3. dialog.NewForm 提交时把新端口写回 env.Vars["HeatNote"]["mbus"]["port"]，
//     再用 config.SaveEnv 覆盖 confs/env.yml。
//
// 失败一律弹 dialog.ShowError，不静默吞。
func (a *App) promptConfig() {
	ports, err := serial.ListPorts()
	if err != nil {
		dialog.ShowError(fmt.Errorf("枚举串口失败: %w", err), a.win)
		return
	}
	if len(ports) == 0 {
		dialog.ShowInformation("配置", "未发现可用串口", a.win)
		return
	}

	// 当前选中的串口：env.Vars["HeatNote"]["mbus"]["port"]。env 可能尚未
	// Attach，这种情况下 mbusPort 留空，由用户从下拉里挑。
	current := ""
	a.mu.Lock()
	if a.env != nil {
		if hn, ok := a.env.Vars["HeatNote"].(map[string]any); ok {
			if m, ok := hn["mbus"].(map[string]any); ok {
				if p, ok := m["port"].(string); ok {
					current = p
				}
			}
		}
	}
	a.mu.Unlock()

	// 当前值若不在枚举结果里（设备被拔了），把它插到下拉首位以便显示。
	if current != "" {
		found := false
		for _, p := range ports {
			if p == current {
				found = true
				break
			}
		}
		if !found {
			ports = append([]string{current}, ports...)
		}
	}

	sel := widget.NewSelect(ports, nil)
	sel.SetSelected(current)

	items := []*widget.FormItem{
		widget.NewFormItem("MBUS 串口", sel),
	}
	dialog.NewForm("配置", "保存", "取消", items, func(ok bool) {
		if !ok {
			return
		}
		picked := sel.Selected
		if picked == "" {
			a.Warn("未选择串口")
			return
		}
		a.applyMBUSPort(picked)
	}, a.win).Show()
}

// applyMBUSPort 把新串口写进 env.Vars 并落盘到 confs/env.yml。env 为 nil
// （未 Attach）时直接返回——这种情况在产品流程里不会出现，防御性兜底。
func (a *App) applyMBUSPort(port string) {
	a.mu.Lock()
	if a.env == nil {
		a.mu.Unlock()
		a.Warn("env 尚未初始化，无法保存配置")
		return
	}
	hn, _ := a.env.Vars["HeatNote"].(map[string]any)
	if hn == nil {
		hn = map[string]any{}
	}
	mbus, _ := hn["mbus"].(map[string]any)
	if mbus == nil {
		mbus = map[string]any{}
	}
	mbus["port"] = port
	hn["mbus"] = mbus
	a.env.Vars["HeatNote"] = hn
	path := a.varsFile
	a.mu.Unlock()

	// 落盘前剔除不可序列化的运行时对象（如 Vars.HeatNote["bluetooth"] 里的
	// *bluetooth.Device），否则 yaml.Marshal 会失败。HeatNote.plan 是当前
	// 下拉框选中的计划路径，属临时运行状态，不随配置落盘持久化。
	clean := config.CleanVars(a.env.Vars, "bluetooth")
	if hn, ok := clean["HeatNote"].(map[string]any); ok {
		delete(hn, "plan")
	}
	if err := config.SaveEnv(path, clean); err != nil {
		dialog.ShowError(fmt.Errorf("保存 %s 失败: %w", path, err), a.win)
		return
	}
	a.Info("MBUS 串口已保存: " + port)
}

// serialFormatRe 序列号格式：可选 W 开头（不区分大小写）后接 12 位数字。
// 对应 Perl 侧 BLAT 主程序的 $tmp_sn =~ /^W?\d{12}/i。
var serialFormatRe = regexp.MustCompile(`^W?\d{12}`)

// promptSerialThenRun 弹出一个输入框让用户填序列号，回车或点"确定"都会
// 触发格式校验，校验通过后启动 startRun；取消或校验失败则保持弹框直到
// 用户改对。序列号存到 a.serNum 备用。
//
// 不用 dialog.NewCustom / NewCustomConfirm：前者即便 dismissText 为空也
// 会渲染一个空按钮占位栏（"第三个空按钮"的来源），后者在点"确定"后必
// 然自动 dismiss 无法阻止弹框关闭。改用 widget.NewPopUp 自管全部布局。
func (a *App) promptSerialThenRun() {
	entry := widget.NewEntry()
	entry.SetPlaceHolder("请输入序列号（12 位数字，可选 W 开头）")
	// Entry 默认 MinSize 较窄，撑出 360 像素让弹窗整体更合理（避免
	// 按钮被挤到换行）。
	entryWrap := container.New(minWidthLayout{w: 360}, entry)

	errLabel := widget.NewLabel("")
	// DangerImportance 让错误提示以主题错误色（红色）渲染。
	errLabel.Importance = widget.DangerImportance
	errLabel.Hide()

	var popup *widget.PopUp
	tryConfirm := func() {
		text := strings.TrimSpace(entry.Text)
		switch {
		case text == "":
			errLabel.SetText("序列号不能为空")
		case !serialFormatRe.MatchString(text):
			errLabel.SetText("序列号格式不对，应为可选 W 开头后接 12 位数字")
		default:
			if popup != nil {
				popup.Hide()
			}
			a.mu.Lock()
			a.serNum = text
			a.mu.Unlock()
			a.startRun()
			return
		}
		// 校验失败：popup 保持显示，重新聚焦 entry 便于重输。
		errLabel.Show()
		fyne.Do(func() {
			a.win.Canvas().Focus(entry)
		})
	}
	// 回车键：直接走 tryConfirm
	entry.OnSubmitted = func(string) { tryConfirm() }

	okBtn := widget.NewButton("确定", tryConfirm)
	cancelBtn := widget.NewButton("取消", func() {
		if popup != nil {
			popup.Hide()
		}
	})
	// 取消靠左、确定靠右
	buttonRow := container.NewHBox(layout.NewSpacer(), cancelBtn, okBtn)

	title := widget.NewLabelWithStyle("输入设备序列号", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})

	content := container.NewVBox(
		title,
		widget.NewSeparator(),
		entryWrap,
		errLabel,
		buttonRow,
	)
	// 四周 padding 模拟 Fyne dialog 默认风格
	padded := container.New(layout.NewPaddedLayout(), content)

	popup = widget.NewPopUp(padded, a.win.Canvas())
	popup.Show()
	// Fyne 的 PopUp 不会自动 focus 内部 Entry；规则见项目 AGENTS.md。
	// 用 fyne.Do 把 Focus 排到主线程下一帧——届时 popup 已 mount 完。
	fyne.Do(func() {
		a.win.Canvas().Focus(entry)
	})
}

// startRun executes the attached plan with the registered cases in a fresh
// goroutine. It is invoked by the toolbar Start button; pressing it again
// while a run is active cancels the old run via StartRun and restarts.
func (a *App) startRun() {
	a.mu.Lock()
	plan, env, reg := a.plan, a.env, a.reg
	a.mu.Unlock()
	if plan == nil || env == nil || reg == nil {
		a.Warn("没有可运行的 plan，请先加载")
		return
	}
	if len(plan.Cases) == 0 {
		a.Warn("plan 为空")
		return
	}
	a.startBtn.SetIcon(theme.MediaStopIcon())
	a.startBtn.SetText("结束测试")
	ctx, _ := a.StartRun()
	a.SetStatus("running...")
	go func() {
		pr := runtime.NewPlanRunner(reg)
		adp := &guiAdapter{gui: a} // 留出引用以便退出前标记取消态
		rep := report.NewMulti(
			report.NewYAMLPath("report.yml"), // 固定文件名 + 每次开始时清空
			report.NewTAP(&tapWriter{a: a}),  // TAP 文本重定向进 log 框，不再写 stdout（避免双写）
			adp,
		)
		err := pr.RunPlan(ctx, plan, env, rep)
		// 在调 runFinished（清掉 runCtx）前标记取消态，OnPlanStop 仍能读到。
		if ctx.Err() != nil {
			adp.cancelled = true
		}
		a.runFinished(ctx)
		// 状态文字 / 按钮 / 进度条 收尾统一交给 guiAdapter.OnPlanStop。
		// 这里只记录 err 日志，避免与 reporter 双写。
		if err != nil {
			a.Error(err.Error())
		}
	}()
}

// guiAdapter forwards plan reporter events to the Fyne widgets so the
// runner only talks to report.Reporter.
type guiAdapter struct {
	gui            *App
	total          int
	cancelled      bool                     // run goroutine 在退出前根据 ctx.Err() 设置
	OnPlanStopHook func(sum report.Summary) // 业务方可选钩子；在 OnPlanStop 顶部调用
}

func (g *guiAdapter) OnPlanStart(total int, startTime time.Time) {
	g.total = total
}

func (g *guiAdapter) OnCaseStart(seq int, cr report.CaseReport) {
	// seq is the 1-based test number; the case tree rows are 0-based.
	g.gui.SetResult(seq-1, string(cr.Result), "")
	g.gui.SetStatus("running " + cr.Name)
}

func (g *guiAdapter) OnCaseStop(seq int, cr report.CaseReport) {
	g.gui.SetResult(seq-1, string(cr.Result), cr.Error)
	if g.total > 0 {
		g.gui.SetProgress(float64(seq) / float64(g.total))
	}
}

// OnPlanStop 收集一次 plan 的最终结果并把界面收尾集中做掉。
// 必须在 fyne.Do 里改 widget——reporter 在 runner goroutine 上回调。
func (g *guiAdapter) OnPlanStop(sum report.Summary) {
	// 1. 业务钩子（可选；nil 时跳过）
	if g.OnPlanStopHook != nil {
		g.OnPlanStopHook(sum)
	}

	// 2. 默认 UI 收尾
	total := sum.TotalNum
	ok, fail := sum.OKNum, sum.FailNum
	skipped := total - ok - fail
	if skipped < 0 {
		skipped = 0
	}
	status := fmt.Sprintf("完成: %d/%d 通过", ok, total)
	if fail > 0 {
		status = fmt.Sprintf("完成: %d 通过, %d 失败, %d 跳过", ok, fail, skipped)
	}
	if g.cancelled {
		status = "已取消"
	}
	g.gui.Info(status)

	fyne.Do(func() {
		g.gui.prog.SetValue(0)
		g.gui.tree.Refresh()
		g.gui.startBtn.SetText("开始测试")
		g.gui.startBtn.SetIcon(theme.MediaPlayIcon())
		g.gui.startBtn.Enable()
		g.gui.SetStatus(status)
	})
}

// tapWriter 把 TAPReporter 的 io.Writer 输出切行后投到 log 框。
// 实现 io.Writer；不缓冲——每段写入都按 \n 切分并 appendLog。
type tapWriter struct{ a *App }

func (w *tapWriter) Write(p []byte) (int, error) {
	w.a.appendTAP(p)
	return len(p), nil
}

// appendTAP 收到 TAPReporter 字节流，按 \n 切行，每行走 appendLog("TAP", ...)。
// 末段无 \n 暂存到 tapPartial，下次 Write 拼接。
func (a *App) appendTAP(p []byte) {
	a.mu.Lock()
	a.tapPartial.Write(p)
	full := a.tapPartial.String()
	a.tapPartial.Reset()
	a.mu.Unlock()
	// 按 \n 切；最后一段若无换行说明是 partial，重新塞回
	lines := strings.SplitAfter(full, "\n")
	var keep []byte
	for i, line := range lines {
		if i == len(lines)-1 && !strings.HasSuffix(line, "\n") {
			keep = []byte(line)
			continue
		}
		a.appendTAPLine(strings.TrimRight(line, "\n"))
	}
	if len(keep) > 0 {
		a.mu.Lock()
		a.tapPartial.Write(keep)
		a.mu.Unlock()
	}
}

func (a *App) appendTAPLine(line string) {
	if line == "" {
		return
	}
	level := classifyTAPLine(line)
	a.appendLog(level, "TAP", line)
}

// classifyTAPLine 决定 TAP 行的 log level。返回 "info|warn|error|debug"，
// 由 colorForEntry 统一配色。
func classifyTAPLine(line string) string {
	switch {
	case strings.HasPrefix(line, "ok "):
		return "info"
	case strings.HasPrefix(line, "not ok "):
		return "error"
	case strings.HasPrefix(line, "Bail out"):
		return "warn"
	case strings.HasPrefix(line, "TAP version"),
		strings.HasPrefix(line, "1.."),
		strings.HasPrefix(line, "#"):
		return "debug"
	default:
		return "info" // YAML 诊断缩进行
	}
}

// ---- rows ----

func (a *App) AddRow(title, name string) {
	a.mu.Lock()
	a.rows = append(a.rows, row{title: title, name: name, result: "planned"})
	a.mu.Unlock()
	fyne.Do(func() { a.tree.Refresh() })
}

func (a *App) SetResult(i int, result, detail string) {
	a.mu.Lock()
	if i >= 0 && i < len(a.rows) {
		a.rows[i].result = result
		a.rows[i].detail = detail
	}
	a.mu.Unlock()
	fyne.Do(func() { a.tree.Refresh() })
}

func (a *App) SetStatus(s string) {
	fyne.Do(func() { a.status.SetText(s) })
}

func (a *App) SetProgress(v float64) {
	fyne.Do(func() { a.prog.SetValue(v) })
}

// ---- log (ring buffer) ----

func (a *App) Info(s string)  { a.appendLog("info", "APP", s) }
func (a *App) Warn(s string)  { a.appendLog("warn", "APP", s) }
func (a *App) Error(s string) { a.appendLog("error", "APP", s) }

// Debug appends a gray log line tagged with the current category (see
// SetCategory). It is an extension of core.Logger.
func (a *App) Debug(s string) { a.appendLog("debug", a.category(), s) }

// SetCategory sets the category tag used by Debug log lines.
func (a *App) SetCategory(c string) {
	a.mu.Lock()
	a.cat = c
	a.mu.Unlock()
}

func (a *App) category() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cat == "" {
		return "APP"
	}
	return a.cat
}

func (a *App) clearLog() {
	a.mu.Lock()
	a.logBuf = a.logBuf[:0]
	a.mu.Unlock()
	fyne.Do(func() {
		a.log.Segments = a.log.Segments[:0]
		a.log.Refresh()
	})
}

// appendLog records one entry into the ring buffer and appends a matching
// colored segment to the RichText widget. It is safe to call from any
// goroutine; the widget mutation is marshalled onto the Fyne main thread
// via fyne.Do.
func (a *App) appendLog(level, category, s string) {
	// 严格按 Perl BLAT::Core::LogAnyConf.pm 的 PatternLayout 输出：
	//   %d{ABSOLUTE} %p %x %c %L - %m%n
	// Go 侧无 NDC(%x) 与行号(%L)，用 ABSOLUTE 时间 + 大写 level + category
	// 替代；`-` 分隔符照搬。时间戳用 Log4perl ABSOLUTE（HH:mm:ss,SSS），
	// 不用 ISO8601/RFC3339（与 Perl 实际输出一致）。
	ts := time.Now().Format("15:04:05,000")
	text := fmt.Sprintf("%s %s %s - %s", ts, strings.ToUpper(level), category, s)
	entry := logEntry{Level: level, Category: category, Text: text}
	a.mu.Lock()
	a.logBuf = append(a.logBuf, entry)
	if len(a.logBuf) > a.logCap {
		// drop oldest in one slice op
		a.logBuf = a.logBuf[len(a.logBuf)-a.logCap:]
	}
	a.mu.Unlock()
	fyne.Do(func() {
		a.log.Segments = append(a.log.Segments, &widget.TextSegment{
			Style: widget.RichTextStyle{
				ColorName: colorForEntry(level, category),
				SizeName:  theme.SizeNameText,
			},
			Text: text,
		})
		// keep the widget in sync with the ring-buffer cap
		if len(a.log.Segments) > a.logCap {
			a.log.Segments = a.log.Segments[len(a.log.Segments)-a.logCap:]
		}
		a.log.Refresh()
		// auto-scroll to bottom
		a.logScroll.ScrollToBottom()
	})
}

// colorForEntry maps a (level, category) pair to a theme color name.
// RUNNER 固定用主题色（蓝），其余按 level：info=foreground、warn=yellow、
// error=red、debug=gray。
func colorForEntry(level, category string) fyne.ThemeColorName {
	if category == "RUNNER" {
		return theme.ColorNamePrimary
	}
	switch level {
	case "warn":
		return theme.ColorNameWarning
	case "error":
		return theme.ColorNameError
	case "debug":
		return theme.ColorNameDisabled
	default: // info
		return theme.ColorNameForeground
	}
}

// ---- core.UI (ctx-aware) ----

// Prompt blocks until the user confirms, cancels, ctx is done, or the
// window closes.
func (a *App) Prompt(ctx context.Context, label, def string) (string, error) {
	req := promptReq{label: label, def: def, reply: make(chan promptReply, 1)}
	select {
	case a.promptCh <- req:
	case <-a.shutdown:
		return "", fmt.Errorf("ui shutdown")
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case r := <-req.reply:
		return r.val, r.err
	case <-a.shutdown:
		return "", fmt.Errorf("ui shutdown")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// WaitContinue blocks until the user confirms, ctx is done, or the window
// closes.
func (a *App) WaitContinue(ctx context.Context, msg string) error {
	req := confirmReq{msg: msg, reply: make(chan struct{}, 1)}
	select {
	case a.confirmCh <- req:
	case <-a.shutdown:
		return fmt.Errorf("ui shutdown")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-req.reply:
		return nil
	case <-a.shutdown:
		return fmt.Errorf("ui shutdown")
	case <-ctx.Done():
		return ctx.Err()
	}
}
