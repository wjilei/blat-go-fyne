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
	"strconv"
	"strings"
	"sync"
	"time"

	"blat/internal/config"
	"blat/internal/core"
	"blat/internal/report"
	"blat/internal/runtime"

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
)

type row struct {
	title  string
	name   string
	result string
	detail string
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

	mu         sync.Mutex
	tapPartial bytes.Buffer // 暂存未完成（无换行结尾）的 TAP 半行
	rows       []row
	logBuf     []logEntry
	logCap     int
	cat        string

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
			a.startRun()
		case "结束测试":
			a.StopRun()
		}
	})

	tb := newBottomBorder(container.NewHBox(a.startBtn), theme.ColorNameInputBorder, 1)

	statusBar := newTopBorder(container.NewHBox(a.status, layout.NewSpacer(), a.prog), theme.ColorNameInputBorder, 1)

	// 程序主场口，左右两栏，左窄右宽
	mainFrame := container.NewHSplit(a.tree, a.logScroll)
	mainFrame.SetOffset(0.3)
	a.win.SetContent(container.NewBorder(
		tb, statusBar, nil, nil,
		mainFrame,
	))
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
	if a.running() {
		a.StopRun()
		a.Warn("已停止当前运行，请重新选择 plan")
	}
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
			report.NewYAMLFile("."),
			report.NewTAP(&tapWriter{a: a}), // TAP 文本重定向进 log 框，不再写 stdout（避免双写）
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
