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


