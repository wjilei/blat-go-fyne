# blat-go hello 示例

最小化参考应用：加载 `plan.yml`，按用例列表跑 Hello Case。示例包含
`SayHello` / `SayBye` / `FailCase` 三个 case。

## 文件

- `main.go`：入口，注册 `HelloSuite::SayHello` / `HelloSuite::SayBye` / `HelloSuite::FailCase` 三个 Case，启动 Fyne GUI。
- `plan.yml`：用例描述，字段含义见 `internal/config/plan.go`。

## 运行

```powershell
cd tools\win32\blat-go
$env:GOWORK = "off"
go build -ldflags="-w" -o .\bin\hello.exe .\cmd\hello
.\bin\hello.exe -plan .\examples\hello\plan.yml
```

> `-ldflags="-w"` 去掉 DWARF 调试信息。Go 1.25 在 Windows / Win11 24H2
> 上带 DWARF 的 PE 可能无法加载，必须去掉。

无 GUI 环境（CI / 容器）：

```powershell
.\bin\hello.exe -no-gui -plan .\examples\hello\plan.yml
```

## plan.yml 字段

```yaml
- name: HelloSuite::SayHello   # 注册表 key
  title: 招呼                   # UI 展示名（可选）
  case_seq: 1                   # 序号（可选）
  counts: 1                     # 重复次数（可选，默认 1）
  desc: 简单问候示例            # 描述（可选）
  who: World                    # 自定义参数，进 Args
```

`name` 是注册时的 key；`title/desc/case_seq/counts/parallel` 是保留字段；
其它键全部进 `Args`，由 Case 的 `Configure(map[string]any)` 接收。
