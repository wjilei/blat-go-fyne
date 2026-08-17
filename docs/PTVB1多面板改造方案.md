# PTVB1 三工位多面板改造方案

> 目标：把 blat-go-fyne 的 Fyne GUI 改造成 Perl 原版 BLAT Heat 应用 PTVB1
> 整机测试的"多面板"形式，同时保留 PSAV 蓝牙测试的单跑界面。

## 1. 背景

- 参考实现：`..\BLAT\lib\BLAT\APP\Heat\PanelPage.pm`（703 行，三工位面板）、
  `HeatAppUI.pm`（面板显隐/兜底提示）。
- Perl 版形态：主窗口 Notebook 加"工位测试"Tab 页，页内横向并排 3 个独立工位面板
  （设备1/2/3），同时可见互不遮挡。每面板含：状态灯+状态文字、SN 输入框（回车即自动
  开始）、串口下拉（枚举注册表 SERIALCOMM）、停止按钮、实时日志框。6 态状态机
  （空闲/运行中/完成/失败/未知/已停止），状态灯颜色联动。Perl 版每工位起独立子进程
  + 500ms 轮询刷新；Go 版不需要子进程——每工位一个 goroutine + 独立 PlanRunner 实例。
- Perl 版"日志"页与"工位测试"页并存（Notebook Tab 切换），Go 版对应 AppTabs。

## 2. 硬约束（用户给定）

1. 只有整机模式，**不需要**单板/整机模式切换，**不需要**按产品名显隐面板。
2. 只有一个用例 `HeatSuite::wire_valve_mbus_read_motor`，不复刻 Perl 全部用例。
3. **PSAV 蓝牙测试必须保留现有单跑界面**（用例树+日志+工具栏"开始测试"那套）。
   计划切换时 UI 联动：选 PTVB1 计划 → 三工位面板；选 PSAV/其他计划 → 单跑界面。

## 3. 已确认决策

| 决策点 | 结论 |
|---|---|
| 面板模式下工具栏"开始测试"按钮 | **不隐藏**。点击时弹提示"请使用工位面板测试：在对应工位输入序列号，回车即开始测试"，中止标准流程（对齐 Perl hook_prepare_run 兜底） |
| 上报时机 | 每工位完成后**独立上报**（各自日志 + 测试记录） |
| 工位数量 | 固定 3 |

## 4. 总体架构

```
┌─ App（既有，单跑模式链路原样保留）
│   ├─ mode: single | panel        ← 新增，随计划切换
│   ├─ mainTabs *container.AppTabs ← 新增，替换 build() 里的 HSplit 主区域
│   │   ├─ Tab「测试」     = 现有 HSplit(tree, logScroll) 原封不动
│   │   └─ Tab「工位测试」 = HBox( stationPanel ×3 )
│   ├─ stations [3]*stationPanel   ← 新增
│   └─ 4 弹框通道 pump 不变（多工位共用，串行排队）
│
└─ stationPanel（新文件 station.go，每工位一份）
    ├─ UI：标题行(设备名+状态灯+状态文字) / SN Entry(OnSubmitted 启动) /
    │       串口 Select(ListPorts) / 停止按钮 / 日志框(SelectableRichText+Scroll)
    ├─ 运行上下文：ctx/cancel、深拷贝 env、独立 logfile(test_P1.log)、
    │             独立 reporter Multi(YAML_P1 + TAP→面板 + stationAdapter + HookStop)
    └─ 状态机：idle/running/done/fail/stopped（5 态，映射颜色）
```

核心原则：单跑模式（PSAV 蓝牙测试）链路一行不动；面板模式是全新并行旁路，
复用 `PlanRunner` / `logfile` / `report` / `uploader` 现成组件。

## 5. 关键设计

### 5.1 工位运行上下文（每工位启动时构建）

```go
// station_run.go（纯逻辑，无 widget）
type stationRun struct {
    ctx    context.Context
    cancel context.CancelFunc
    env    *core.Env            // 深拷贝
    logf   *logfile.FileLogger // test_P1.log，独立实例
    logOff int64
    logGen int
    plan   *config.Plan         // 从 App.plan 取（同 plan，只读共享安全）
    reg    *runtime.Registry
}
```

**env 深拷贝**（关键，`_ensureMBUS` 会写回 `HeatNote["mbus_dev"]`，三工位绝不能共享
HeatNote）：

- `Vars` 递归深拷贝（map/slice/基本类型；指针如 `*bluetooth.Device` 原样传递，
  只拷容器不拷资源）。
- **从 HeatNote 副本中删除 `mbus_dev` / `bluetooth` 键**——防止单跑模式遗留的
  设备实例被三工位共享（串口独占冲突），让 `_ensureMBUS` 按各工位串口重建。
- 覆盖：`HeatNote["mbus"]["port"]` = 该工位所选串口、`HeatNote["serial"]` = SN 输入、
  `HeatNote["mbus_mock"]` = 全局 mock 标志。
- `Devs` 浅拷贝即可（bluetooth 面板模式不用；mbus.Device 在拷贝后的 HeatNote 里新建）。
- `Log` 换成工位 logger（写工位 logf + 面板日志框）；`UI` 包一层 `stationUI` adapter。

### 5.2 日志多流

`logfile.FileLogger` 每实例绑定一个文件路径（`Open(path)`），多实例天然可行：

- 单跑：`test.log`（现状不动）。
- 面板：`test_P1.log` / `test_P2.log` / `test_P3.log`，路径规则对齐
  `config.DefaultTestLogPath()`（release → `~/.blat/`，dev → cwd）。
- `refreshLog` 逻辑照抄参数化版本到 station.go（logOff/logGen 每工位私有），
  面板日志框复用 `SelectableRichText`。

### 5.3 弹框并发路由（`askValveTurned`）

共享 pump 串行排队可接受（操作员一次只能盯一个工位确认阀门）：

- 新增 `stationUI` adapter 实现 `core.UI`，4 个方法转发到 App 并在文案前加
  `【设备N】` 前缀，ctx 透传。
- 工位 env.UI = `stationUI{base: app, prefix: "设备1"}`。
- 弹框排队期间其他工位测试正常继续（只在 Confirm 处阻塞各自 goroutine），无死锁。

### 5.4 停止语义（两层）

| 层级 | 动作 | 实现 |
|---|---|---|
| 工位级 | 面板停止按钮 | `station.Stop()` → 该工位 cancel |
| 全局 | 关窗 | `SetOnClosed` 现有 `close(shutdown)` + 遍历 3 工位 cancel |

### 5.5 模式切换（计划联动）

- 判定：`config.IsPanelPlan(path string) bool`——文件名含 `PTVB1`（大小写不敏感），
  复用 `planModeRe`（`internal/config/plan.go:27-29`）思路，单测覆盖。
- `onPlanSelected` 加载 plan 后：mode=panel 时 `mainTabs.SelectTabIndex(1)`；
  mode=single 时 `SelectTabIndex(0)`。
- 工具栏：开始按钮保留（面板模式下点击仅提示用面板输入启动）；配置按钮、计划下拉
  两个模式都可用。
- 切换计划时若另一模式有运行中任务 → Message 提示"请先停止/完成测试"，**阻止切换**
  （不做自动强停）。

### 5.6 上报时机

每工位完成后独立上报（对齐 Perl 版每工位 HeatSaveTestData 入库）：

- 每工位 reporter Multi = `YAML(report_P1.yml, WithLogfile(工位logf), WithVars(工位env.Vars))`
  + `TAP→面板日志框` + `stationAdapter(状态机/状态灯)` +
  `uploader.NewHookStop(工位env, 工位logf.Snapshot, debug)`。
- 确认 `uploader.HookStopReporter` 无共享可变状态；若有，加上报互斥锁串行化
  三工位并发完成。
- 单跑模式上报链路不动。

### 5.7 状态机与 UI 细节

```
idle(灰) → running(黄) → done(绿) / fail(红) / stopped(红)
```

- SN `Entry.OnSubmitted` 触发启动；串口下拉选项来自 `serial.ListPorts()`。
- 面板布局：HBox 三等分，每面板 VBox：标题行 → 输入区 → 日志框拉伸。
- 所有 widget 更新走 `fyne.Do`；运行状态字段每工位独立 mutex。

## 6. 文件改动清单

| 文件 | 动作 | 职责 |
|---|---|---|
| `docs/PTVB1多面板改造方案.md` | 新增 | 本文档 |
| `internal/config/plan.go` | 小改 | `IsPanelPlan` + `DefaultPanelLogPath(i int)` + 单测 |
| `internal/ui/fyne/station_run.go` | 新增 ~250 行 | stationRun、5 态状态机、deepCopyVars、stationUI、stationAdapter + 单测 |
| `internal/ui/fyne/station.go` | 新增 ~450 行 | stationPanel widget：布局、状态灯、SN 启动、日志框刷新（@designer） |
| `internal/ui/fyne/panelpage.go` | 新增 ~200 行 | AppTabs 组装、mode 判定与切换、3 工位管理、关窗级联停止 |
| `internal/ui/fyne/app.go` | 小改 | build() 主区域换 mainTabs；App 加 mode/stations 字段；onPlanSelected 模式分支；SetOnClosed 级联停止；开始按钮面板模式提示 |
| `cmd/blat/main.go` | 小改 | 面板日志路径/工位数注入（固定 3 可先硬编码） |
| `internal/uploader/` | 视情况 | HookStop 共享状态检查/加锁 |

## 7. 风险点

1. **HeatNote 深拷贝不彻底**（嵌套 map 引用共享）→ `deepCopyVars` 递归拷贝 +
   删除 `mbus_dev`/`bluetooth` 键，单测验证 station1 写 mbus_dev 不影响 station2。
2. **关窗时 3 工位 goroutine 泄漏** → 级联 cancel + 测试验证。
3. **uploader 三工位并发完成同时上报** → 检查并加锁。
4. **面板 mock 调试**：`mbus_mock` 标志注入路径要打通，保证无硬件可跑
   （现有 `-mock-bt` 只覆盖蓝牙，需确认 mbus mock 的 flag/注入点）。
5. **单跑模式回归**：PSAV 链路一行不动的约束必须守住，每步跑
   `go test ./internal/ui/fyne/` 防回归。

## 8. TDD 测试清单

- `config`：`IsPanelPlan`（PSAV/PTVB1/其他文件名各一例 + 大小写）；面板日志路径
  dev/release 行为。
- `station_run.go`：状态机迁移、`deepCopyVars` 隔离性（含 mbus_dev 键删除）、
  `stationUI` 前缀透传、`stationAdapter` 事件序列→状态映射。
- 日志：logfile 多实例独立 TailFrom（两个 FileLogger 写不同文件互不干扰）。
- 面板收尾：report_P1.yml 生成、HookStop 触发（--debug 模式不触网，沿用现有测试思路）。

## 9. 实施顺序（每步可独立编译验证）

| 步 | 内容 | 负责人 | 验证 |
|---|---|---|---|
| 1 | config：IsPanelPlan + DefaultPanelLogPath + 单测 | @fixer | `go test ./internal/config/` |
| 2 | station_run.go 纯逻辑 + 单测 | @fixer | `go test ./internal/ui/fyne/` |
| 3 | 面板 UI：stationPanel widget + AppTabs 组装 + 状态灯配色 | @designer | `go build ./...` + 人工看效果 |
| 4 | 运行编排：工位启动 goroutine、级联停止、模式切换、开始按钮面板提示 | @fixer | `go build ./...` + mock 三工位并行跑 |
| 5 | 上报收尾：per-station Multi/HookStop、uploader 并发检查 | @fixer | `go build ./... && go test ./...` |

## 10. 验证基准（参照当前行为）

- 单跑模式：PSAV 计划下"开始测试"流程与现有行为完全一致（日志、report.yml、上报）。
- 面板模式：PTVB1 计划下三个工位可独立输入 SN + 选串口 + 回车启动 + 独立停止，
  各自日志独立落盘 `test_P1.log`~`test_P3.log`，各自完成独立上报。
- mock：`mbus_mock` 生效时无硬件可全流程跑通。
