// Windows 串口读写层：用 golang.org/x/sys/windows 自带的 API 实现，
// 刻意不引入任何第三方串口库（go.bug.st/serial、tarm/serial 等），
// 保持项目 cgo-free。
//
// 非阻塞读语义（对应 Perl BLAT serial.pm 的 _BlockRead，L753-770）：
// Read 立即返回，串口无数据时返回 (0, nil)，由调用方自行轮询
// （BLAT 用 read+usleep(20000) 轮询，本项目由 mbus commandTrans 轮询）。
package serial

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

// DCB.Flags 相关位（winbase.h DCB 结构，标准值）：
// fBinary / fParity / 软件流控 fOutX/fInX / 错误中止 fAbortOnError。
const (
	fBinary       = 0x00000001
	fParity       = 0x00000002
	fOutX         = 0x00000100
	fInX          = 0x00000200
	fAbortOnError = 0x00004000
)

// comDevicePrefix 是 CreateFile 打开串口的设备路径前缀（COM10+ 必需）。
const comDevicePrefix = `\\.\`

// Port 是串口读写接口，便于测试注入 fake（见 internal/device/mbus 测试）。
type Port interface {
	Write(b []byte) (int, error)
	Read(buf []byte) (int, error)
	Close() error
}

// winPort 是基于 Windows 句柄的串口实现。
type winPort struct {
	handle   windows.Handle
	portName string
}

// OpenPort 打开串口 name（如 "COM3"），固定 8 数据位、1 停止位、二进制模式。
// parity 取值 "none"/"even"/"odd"（不区分大小写）。baud 如 2400。
//
// 配置流程对应 Perl BLAT serial.pm open_port（L133-177），每一步都对齐：
//   - CreateFile → GetCommState/SetCommState（baud/databits/parity/stopbits/
//     binary/无流控）→ SetupComm(4096,4096) → 非阻塞读超时 + 写超时 1s
//   - PurgeComm 打开时清空 TX/RX 缓冲（Perl L173 purge_all），去掉上次
//     残留数据；缺这一步会读到 mbus 帧前的杂字节
//   - EscapeCommFunction 显式 CLRRTS/CLRDTR（Perl L175-176 rts_active(0)
//     /dtr_active(0)），M-Bus 适配器据此切收发方向
func OpenPort(name string, baud uint32, parity string) (Port, error) {
	path := name
	if !strings.HasPrefix(strings.ToLower(path), `\\.\`) {
		path = comDevicePrefix + path
	}
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("serial: 打开 %s 失败: 非法端口名: %w", name, err)
	}
	h, err := windows.CreateFile(ptr,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		return nil, fmt.Errorf("serial: 打开 %s 失败: %w", name, err)
	}
	if err := configureDCB(h, baud, parity); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("serial: 设置 %s 串口参数失败: %w", name, err)
	}
	if err := windows.SetupComm(h, 4096, 4096); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("serial: 设置 %s 缓冲失败: %w", name, err)
	}
	// 非阻塞读（ReadIntervalTimeout=MAXDWORD，ReadFile 立即返回）；
	// 写超时 1000ms 防卡死。对应 Perl serial.pm L167-169。
	to := &windows.CommTimeouts{
		ReadIntervalTimeout:         0xFFFFFFFF, // MAXDWORD
		ReadTotalTimeoutMultiplier:  0,
		ReadTotalTimeoutConstant:    0,
		WriteTotalTimeoutMultiplier: 0,
		WriteTotalTimeoutConstant:   1000,
	}
	if err := windows.SetCommTimeouts(h, to); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("serial: 设置 %s 超时失败: %w", name, err)
	}
	// 对齐 Perl serial.pm L173 purge_all：清空 TX/RX 缓冲，否则上次
	// 残留数据会污染本次 mbus 命令的 recvHex。
	const purgeAll = 0x000F // PURGE_TXABORT(1) | PURGE_RXABORT(2) | PURGE_TXCLEAR(4) | PURGE_RXCLEAR(8)
	if err := windows.PurgeComm(h, purgeAll); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("serial: 清空 %s 缓冲失败: %w", name, err)
	}
	// 对齐 Perl serial.pm L175-176 rts_active(0)/dtr_active(0)：M-Bus
	// 适配器据此切收发方向。
	if err := windows.EscapeCommFunction(h, windows.CLRRTS); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("serial: 设置 %s RTS 失败: %w", name, err)
	}
	if err := windows.EscapeCommFunction(h, windows.CLRDTR); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("serial: 设置 %s DTR 失败: %w", name, err)
	}
	return &winPort{handle: h, portName: name}, nil
}

// configureDCB 通过 GetCommState/SetCommState 配置 DCB：
// 8 数据位、1 停止位、baud、parity（even/odd 置 fParity），强制二进制模式
// （fBinary），清掉软件流控（fOutX/fInX）与 fAbortOnError。
func configureDCB(h windows.Handle, baud uint32, parity string) error {
	var dcb windows.DCB
	if err := windows.GetCommState(h, &dcb); err != nil {
		return err
	}
	dcb.BaudRate = baud
	dcb.ByteSize = 8
	dcb.StopBits = windows.ONESTOPBIT

	p := parityConst(parity)
	dcb.Parity = p
	dcb.Flags |= fBinary
	if p != windows.NOPARITY {
		dcb.Flags |= fParity
	} else {
		dcb.Flags &^= fParity
	}
	dcb.Flags &^= fOutX | fInX | fAbortOnError
	return windows.SetCommState(h, &dcb)
}

// parityConst 把 "none"/"even"/"odd"（不区分大小写）映射为 Windows 常量，
// 其它值按 NOPARITY 处理。
func parityConst(parity string) uint8 {
	switch strings.ToLower(parity) {
	case "even":
		return windows.EVENPARITY
	case "odd":
		return windows.ODDPARITY
	default:
		return windows.NOPARITY
	}
}

// Write 把 b 写入串口，返回实际写入字节数。同步句柄 WriteFile。
func (p *winPort) Write(b []byte) (int, error) {
	var written uint32
	if err := windows.WriteFile(p.handle, b, &written, nil); err != nil {
		return 0, fmt.Errorf("serial: 写 %s 失败: %w", p.portName, err)
	}
	return int(written), nil
}

// Read 非阻塞读取：串口无数据时立即返回 (0, nil)（ReadIntervalTimeout=
// MAXDWORD）。同步句柄 ReadFile。
func (p *winPort) Read(buf []byte) (int, error) {
	var n uint32
	if err := windows.ReadFile(p.handle, buf, &n, nil); err != nil {
		return 0, fmt.Errorf("serial: 读 %s 失败: %w", p.portName, err)
	}
	return int(n), nil
}

// Close 关闭串口句柄，幂等（已关闭/无效句柄返回 nil）。
func (p *winPort) Close() error {
	if p.handle == 0 || p.handle == windows.InvalidHandle {
		return nil
	}
	if err := windows.CloseHandle(p.handle); err != nil {
		return fmt.Errorf("serial: 关闭 %s 失败: %w", p.portName, err)
	}
	p.handle = windows.InvalidHandle
	return nil
}
