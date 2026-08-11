# blat-go

Go port of the BLAT factory test framework, kept as a minimal reference.

## Layout

```
blat-go/
├── go.mod
├── cmd/blat/main.go        # the minimal reference application
├── internal/
│   ├── core/                # App, Suite, Case, Runner, Env
│   │   ├── app.go
│   │   ├── case.go
│   │   ├── env.go
│   │   ├── runner.go
│   │   └── suite.go
│   ├── device/              # Device interface (drivers plug in here)
│   │   └── device.go
│   └── ui/                  # Console UI implementation
│       └── ui.go
```

## Mapping to Perl BLAT

| Perl | Go |
|---|---|
| `BLAT::Core::AppBase` | `core.App` |
| `BLAT::Core::SuiteRole` | `core.Suite` |
| `lib/BLAT/APP/*/Cases/*` | `core.Case` |
| `BLAT::Core::Runner` | `core.Runner` |
| `BLAT::Device::serial` | `device.Device` (interface) |
| `BLAT::UI::*` | `core.UI` (interface, `ui.Console` default) |
| Test env / Runner state | `core.Env` |

## Run

```powershell
cd tools\win32\blat-go
go run .\cmd\hello
```

Expected output:

```
[INFO] == suite: HelloSuite
[INFO] >>> SayHello
[INFO] Hello, World
Your name [guest]: <type something>
[INFO] Welcome, <typed>
done (press Enter to continue)
```

## Extending

- Add a Suite: implement `core.Suite` and append to your `App.Suites()`.
- Add a Case: implement `core.Case` and append to your `Suite.Cases()`.
- Add a device driver: implement `device.Device` and put it in `Env.Devs`.
- Replace the UI: implement `core.UI` and `core.Logger` and pass it as `env.UI` / `env.Log`.
