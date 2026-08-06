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
