# AGENTS.md — blat-go-fyne 项目规则

本文件记录本项目开发过程中沉淀的约定与陷阱。给 AI 协作者与新成员参考。

## Fyne 弹框输入框自动 focus

**规则**：所有弹框（dialog.NewCustomConfirm / NewCustom / NewForm 等）只要包含需要用户输入的 widget.Entry，**弹框打开后必须把焦点自动设置到该 Entry**。

**原因**：Fyne v2 的 `dialog.NewCustomConfirm` / `dialog.NewCustom` 不会自动 focus 内部 Entry；只有 `dialog.NewForm` 才会自动 focus 第一个 FormItem 的 widget。Custom 路径必须手动 focus。

**标准实现**：

```go
dlg := dialog.NewCustomConfirm(title, "确定", "取消", entry, onConfirm, a.win)
dlg.Show()
// fyne.Do 把 Focus 排到主线程下一帧——此时 dialog 已 mount 完，Focus 才有效
fyne.Do(func() {
    a.win.Canvas().Focus(entry)
})
```

**注意**：
- 直接 `dlg.Show()` 后立即 `win.Canvas().Focus(entry)` 通常无效——dialog 还没 mount 到 canvas 树。
- `time.AfterFunc(50*time.Millisecond, ...)` 是兜底方案；若 `fyne.Do` 路径不稳定再换。
- 多 Entry 弹框：focus 第一个即可，后续用 Tab 切换（Fyne v2.4+ Form 内置 Tab Order）。

**适用范围**：本项目所有 `internal/ui/fyne/` 下的弹框。PromptReq/ConfirmReq 异步通道路径下的弹框同样适用。

## 重构老代码

**规则**：写新代码时若遇到老代码结构不合理、阻碍扩展或违背当前需求，**该重构就重构**，遵循软件开发最佳实践（单一职责、清晰命名、最小惊讶、避免重复等），不必为兼容老结构而绕路。

**注意**：
- 重构以"不改变既有行为"为底线；涉及行为变化的改动需向用户说明。
- 改完后跑 `go build ./...` 与相关测试，确保重构没有破坏现有功能。
- 重构范围控制在当前任务相关代码内，不顺手大改无关模块。

## TDD 开发流程

**规则**：本项目开发遵循 TDD（测试驱动开发），先写测试、再写实现、最后重构。

**标准流程**：
1. **Red**：先写失败的测试，明确描述期望行为（函数签名、输入、预期输出/错误）。
2. **Green**：写最简实现让测试通过，不提前优化。
3. **Refactor**：在测试保护下重构，消除重复、改善命名与结构，测试保持绿色。

**注意**：
- 测试文件与被测代码同包（`foo_test.go` 放 `foo.go` 同目录），包名用 `package xxx` 或 `package xxx_test` 均可，视是否需要访问未导出符号而定。
- 纯逻辑（配置解析、上报字段组装、URL 构造、状态机等）必须有单元测试；依赖外部资源（网络、OSS、蓝牙、串口、Fyne UI）的逻辑通过接口抽象 + fake/mock 测试，不真实触网。
- 新增/修改功能时，先看是否已有对应测试；没有就先补测试再写实现。
- 每步跑相关测试（`go test ./<包>/`），全部完成后跑 `go build ./...` 与 `go test ./...` 收尾。
- 允许用 `httptest.Server` 模拟 BLAT 服务器响应，测试 HTTP 客户端逻辑（请求 URL、Header、重试、错误判定）。

## 项目结构

BLAT 工厂测试框架的 Go 移植版（原 Perl），带 Fyne 桌面 GUI。模块名 `blat`，Go 1.25，Windows 平台。核心依赖：`fyne.io/fyne/v2`（GUI）、`tinygo.org/x/bluetooth`（真实 BLE）、`aliyun-oss-go-sdk`（日志上传）、`fxamacker/cbor/v2`（蓝牙协议帧编解码）、`ulikunitz/xz`（日志压缩）、`gopkg.in/yaml.v3`（配置）。

```
blat-go-fyne/
├── cmd/hello/               # 唯一入口
│   ├── main.go              # flag 解析、env/plan 装配、GUI 或 Console 启动、跑完释放蓝牙
│   └── cases/               # 用例包：init() 自动 Register 到全局 Registry
│       ├── cases.go         # global Registry + Register(name, factory)
│       ├── sayhello.go / saybye.go / fail.go   # 演示用例（HelloSuite::*）
│       └── wire_valve_bluetooth_test.go        # HeatSuite::* 蓝牙用例（含 _ensureBluetooth 连接复用）
├── internal/
│   ├── core/                # 框架核心（镜像 Perl BLAT 布局）：Case / Suite / App / Runner / Env
│   ├── runtime/             # plan ↔ Case 装配：Registry（名字→Factory）、PlanRunner（执行+重试+上报事件）
│   ├── config/              # YAML 加载：LoadPlan / LoadEnv / SaveEnv / CleanVars / TestModeFromPlanPath
│   ├── device/
│   │   ├── device.go        # 低层驱动最小契约 Device 接口（Open/Close/Command）
│   │   └── bluetooth/       # 业务级蓝牙设备（mock/real 双模式）
│   ├── ui/
│   │   ├── ui.go            # Console 命令行 UI（-no-gui 模式，含日志环形缓冲 SnapshotLog）
│   │   └── fyne/app.go      # Fyne GUI：计划下拉框、用例树、TAP 视图、配置弹框、异步弹框通道
│   ├── report/              # Reporter 接口 + YAMLReporter / TAPReporter / MultiReporter
│   ├── uploader/            # hook_stop 上报：日志 LZMA 压缩→OSS，测试记录 POST→BLAT 后台
│   └── serial/              # Windows 串口枚举 ListPorts()（读注册表，无第三方库，cgo-free）
├── confs/                   # plan_*.yml / env.yml / uploader.yml（运行配置）
├── examples/                # 参考示例（hello、heat）
├── bin/                     # 构建产物（hello.exe）
└── AGENTS.md
```

### 核心流程（数据流）

`cmd/hello/main.go` 加载 `confs/uploader.yml`（凭据注入 uploader）→ 加载 `--env`（默认 `confs/env.yml`，写入 `Env.Vars`）→ 注入 `Vars.HeatNote.bt_mock`（bool）与 `Vars.HeatNote.plan` → 用 `cases.Global()` 构造 `runtime.PlanRunner` → `RunPlan` 按 plan 顺序 Invoke 每个 case（counts 次）→ 每步发 Reporter 事件 → 结束后 `report.NewMulti(YAML, TAP, uploader.HookStopReporter)` 落盘 + 上报 → 跑完 `disconnectBluetooth` 释放连接。

### 各包要点

- **core**：`Case{Name() string; Run(ctx, env) error}`；`Env{Log, UI, Vars map[string]any, Devs map[string]any, Out}`。`Devs` 值类型是 any：低层驱动实现 `Device` 接口，业务级设备（bluetooth）把方法直接挂具体类型上，由拥有它的 Case 类型断言取用。`UI.Prompt/WaitContinue` 必须 ctx-aware（Stop 按钮/关窗时返回 ctx.Err()）。可选接口 `Configurable`：case 实现它即可在 Run 前收到 plan 里的自定义参数。
- **runtime**：case 注册名沿用 Perl 的 `<Suite>::<Method>` 风格（如 `HeatSuite::wire_valve_bluetooth_test_all_params`），用显式 Registry 避免反射。
- **config**：plan 为顶层 YAML 序列，保留字段 `name/title/desc/case_seq/counts/parallel`，其余键平铺进 `Args` 传给 case。`TestModeFromPlanPath` 从文件名解析模式：`confs/plan_PSAV_ut_check_state.yml` → `ut_check_state`。
- **device/bluetooth**：**故意不实现 core.Device**。挂在 `env.Devs["bluetooth"]`（main 注入 `NewDevice()` = real）。mock/real 由 `NewMockDevice`/`NewRealDevice` 构造；`bt_mock` 标志决定 case 端选哪个。协议帧头字节：PSAV→`f9`、其它→`f8`；GATT 服务 `0xfff0`、特征 `0xfff2`。mac 由序列号经 `ParseIdToMac(id)` 派生。真实 BLE 的所有 tinygo 调用投递到专用串行 executor goroutine 依次执行（规避 Windows 上非主线程并发调用崩溃，tinygo issue #294）。常用方法：`Connect/Disconnect/Reboot/Read/ResetValve/EnableNbiot/DisableNbiot/SetDevType/SetLogger/SetMockStatus/IsConnected`。mock 默认数据保证 PSAV 流程首读通过（NbRssi≥−81 → NB ok、ValveState=0）。
- **ui/fyne**：UI 变更一律 `fyne.Do` 上主线程；`Prompt`/`WaitContinue` 通过 `promptReq`/`confirmReq` 异步通道弹框实现；配置弹框把 MBUS 端口等持久化到 `confs/env.yml`（`config.SaveEnv`）。
- **uploader**：`HookStopReporter` 实现 `report.Reporter`，`OnPlanStop` 时把 Console/GUI 环形缓冲的完整日志 LZMA 压缩上传 OSS，并把测试记录 POST 到 BLAT 后台（对齐 Perl `hook_stop`）；`--debug` 时不触网仅打印。`GetTestRecord` 查询整机测试记录（devType=2）。
- **serial**：`ListPorts()` 读注册表 `HKLM\HARDWARE\DEVICEMAP\SERIALCOMM`，按 COM 数字排序去重。刻意不用第三方串口库，保持 cgo-free。

### 运行方式

```powershell
go run ./cmd/hello                                  # Fyne GUI（工具栏选择计划并开始）
go run ./cmd/hello -no-gui --plan confs/plan_PSAV_ut_resetvalve.yml --env confs/env.yml   # 无头控制台
go run ./cmd/hello -mock-bt=true                    # 蓝牙用 mock（无硬件调试）
go run ./cmd/hello --debug                          # 上报只打印不触网
```

### 关键约定

- **HeatNote**：`env.yml` 顶层 key，存放产线参数（`serial/lot/model/pn/mbus/tenant_id/user/bt_mock/plan` 等），case 运行期也把蓝牙实例写回 `HeatNote["bluetooth"]` 供后续 case 复用。键名大写。
- **蓝牙连接复用**：所有蓝牙用例统一走 `_ensureBluetooth`（cases 包）：优先取 `HeatNote["bluetooth"]`；否则仅当 `Devs["bluetooth"]` 实例模式与 `bt_mock` 一致才兜底复用，再否则按 `bt_mock` 新建 mock/real 并写回。**不要**无条件 fallback 到 `env.Devs["bluetooth"]`——main 默认注入 real，会让 `-mock-bt=true` 拿不到 mock。
- **新加用例**：在 `cmd/hello/cases/` 下新建文件，实现 `core.Case`（可加 `Configure`），`init()` 里 `Register("<Suite>::<方法名>", ...)`，然后在 plan YAML 里按名字引用。
- **新加计划**：plan 文件放 `confs/`，按 `plan_<设备类型>_<模式>.yml` 命名；GUI 下拉框选项在 `main.go` 的 `builtinPlans` 里追加。


