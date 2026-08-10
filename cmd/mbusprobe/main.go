package main

import (
	"fmt"
	"os"
	"time"

	"blat/internal/serial"
)

// 用法：mbusprobe [port] [baud] [parity]
// 默认 COM10 2400 none（对齐 Perl parity_enable(0) 行为）

func parity8(p string) string {
	if p == "none" {
		return "N1"
	}
	return string(p[0]-'a'+'A') + "1"
}

// 直接连 COM10 试发 SET_VALVE + 读 BB1E，看 raw 回啥。
// 走真实串口，独立于 GUI/case 框架。
func main() {
	portName := "COM10"
	baud := uint32(2400)
	parity := "none"
	if len(os.Args) > 1 {
		portName = os.Args[1]
	}
	if len(os.Args) > 2 {
		var b uint64
		fmt.Sscanf(os.Args[2], "%d", &b)
		baud = uint32(b)
	}
	if len(os.Args) > 3 {
		parity = os.Args[3]
	}
	const mac = "222222430001" // 用日志里的 serial_num

	port, err := serial.OpenPort(portName, baud, parity)
	if err != nil {
		fmt.Fprintf(os.Stderr, "OpenPort %s 失败: %v\n", portName, err)
		os.Exit(1)
	}
	defer port.Close()
	fmt.Printf("已打开串口 %s (%d 8%s)\n", portName, baud, parity8(parity))

	// 构造 SET_VALVE 帧（与 mbus.buildSetValveFrame 同逻辑）
	// 先静默读 2 秒看设备是否主动广播（有些 M-Bus 适配器会自报）
	fmt.Println("发送任何命令前先静默读 2 秒看设备主动广播...")
	bufProbe := make([]byte, 256)
	deadlineSilent := time.Now().Add(2 * time.Second)
	silent := 0
	for time.Now().Before(deadlineSilent) {
		n, _ := port.Read(bufProbe)
		if n > 0 {
			silent += n
			fmt.Printf("  静默期收到 %d 字节: %s\n", n, toHex(bufProbe[:n]))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if silent == 0 {
		fmt.Println("  静默期未收到任何字节")
	}

	frame, err := buildSetValveFrame(mac, 0, 0xff)
	if err != nil {
		fmt.Fprintf(os.Stderr, "buildSetValveFrame 失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("发送 SET_VALVE 帧 (%d 字节):\n  %s\n",
		len(frame), toHex(frame))

	// raw 发送 + raw 接收（不走 device 抽象），看设备到底回啥
	if _, err := port.Write(frame); err != nil {
		fmt.Fprintf(os.Stderr, "Write 失败: %v\n", err)
		os.Exit(1)
	}

	// 收集 raw 数据 5 秒，看设备回啥
	fmt.Println("等待 5 秒接收 raw 数据...")
	deadline := time.Now().Add(3 * time.Second)
	var all []byte
	buf := make([]byte, 256)
	for time.Now().Before(deadline) {
		n, err := port.Read(buf)
		if err != nil {
			fmt.Printf("  Read err: %v\n", err)
			break
		}
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			all = append(all, chunk...)
			fmt.Printf("  +%d 字节: %s\n", n, toHex(chunk))
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if len(all) == 0 {
		fmt.Println("3 秒内没收到任何字节")
	} else {
		fmt.Printf("共收到 %d 字节: %s\n", len(all), toHex(all))
	}

	// 再读一次 BB1D（读电机状态）—— 如果 BB1D 能回，BB1F 也能回，
	// 说明串口配置正确但 SET 设备不回；BB1D 都不回则串口/接线有问题
	fmt.Println("\n发送 BB1D (read motor status)...")
	frameMotor := buildReadMotorFrame(mac)
	fmt.Printf("  帧: %s\n", toHex(frameMotor))
	if _, err := port.Write(frameMotor); err != nil {
		fmt.Fprintf(os.Stderr, "Write 失败: %v\n", err)
		os.Exit(1)
	}
	deadline = time.Now().Add(3 * time.Second)
	all = all[:0]
	for time.Now().Before(deadline) {
		n, err := port.Read(buf)
		if err != nil {
			break
		}
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			all = append(all, chunk...)
			fmt.Printf("  +%d 字节: %s\n", n, toHex(chunk))
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if len(all) == 0 {
		fmt.Println("3 秒内没收到任何字节")
	} else {
		fmt.Printf("共收到 %d 字节: %s\n", len(all), toHex(all))
	}

	// 再读一次 BB1E
	fmt.Println("\n发送 BB1E (read info)...")
	frameInfo, _ := buildReadInfoFrame(mac)
	fmt.Printf("  帧: %s\n", toHex(frameInfo))
	if _, err := port.Write(frameInfo); err != nil {
		fmt.Fprintf(os.Stderr, "Write 失败: %v\n", err)
		os.Exit(1)
	}
	deadline = time.Now().Add(3 * time.Second)
	all = all[:0]
	for time.Now().Before(deadline) {
		n, err := port.Read(buf)
		if err != nil {
			break
		}
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			all = append(all, chunk...)
			fmt.Printf("  +%d 字节: %s\n", n, toHex(chunk))
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if len(all) == 0 {
		fmt.Println("3 秒内没收到任何字节")
	} else {
		fmt.Printf("共收到 %d 字节: %s\n", len(all), toHex(all))
	}
}

// ---- 下面是从 mbus 包抄的 buildSetValveFrame / buildReadInfoFrame，
// 配合 parseMbusID 复刻一份，避免在 cmd 工具里 import 私有符号 ----

// buildSetValveFrame 构造 BB1F SET_VALVE 请求帧（21 字节）。
func buildSetValveFrame(mac string, openPre, calcDay byte) ([]byte, error) {
	frame := []byte{
		0xfe, 0xfe, 0xfe, 0x68, 0x20,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x05, 0xbb, 0x1f,
		0x01, 0x00, 0x00, 0x00, 0x16,
	}
	id, err := parseMbusID(mac)
	if err != nil {
		return nil, err
	}
	for i, v := range id {
		frame[5+i] = v
	}
	frame[17] = openPre
	frame[18] = calcDay
	var cs byte
	for i := 3; i <= 18; i++ {
		cs += frame[i]
	}
	frame[19] = cs
	return frame, nil
}

// buildReadInfoFrame 构造 BB1E READ_INFO 请求帧（19 字节）。
func buildReadInfoFrame(mac string) ([]byte, error) {
	frame := []byte{
		0xfe, 0xfe, 0xfe, 0x68, 0x20,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x03, 0xbb, 0x1e,
		0x01, 0x00, 0x16,
	}
	id, err := parseMbusID(mac)
	if err != nil {
		return nil, err
	}
	for i, v := range id {
		frame[5+i] = v
	}
	var cs byte
	for i := 3; i <= 16; i++ {
		cs += frame[i]
	}
	frame[17] = cs
	return frame, nil
}

// buildReadMotorFrame 构造 BB1D READ_MOTOR 请求帧（19 字节）。
func buildReadMotorFrame(mac string) []byte {
	frame := []byte{
		0xfe, 0xfe, 0xfe, 0x68, 0x20,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x03, 0xbb, 0x1d,
		0x01, 0x00, 0x16,
	}
	id, _ := parseMbusID(mac)
	for i, v := range id {
		frame[5+i] = v
	}
	var cs byte
	for i := 3; i <= 16; i++ {
		cs += frame[i]
	}
	frame[17] = cs
	return frame
}

func parseMbusID(mac string) ([]byte, error) {
	for _, c := range mac {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("%s 不可用于MBUS", mac)
		}
	}
	id := make([]byte, 6)
	switch len(mac) {
	case 10:
		id[5] = 0
		for i := 0; i < 5; i++ {
			v, _ := hexByte(mac, 2*(4-i))
			id[i] = v
		}
	case 12:
		for i := 0; i < 6; i++ {
			v, _ := hexByte(mac, 2*(5-i))
			id[i] = v
		}
	default:
		return nil, fmt.Errorf("%s 不可用于MBUS", mac)
	}
	return id, nil
}

func hexByte(s string, off int) (byte, error) {
	var v int
	for _, c := range s[off : off+2] {
		v = v*16 + int(c-'0')
	}
	return byte(v), nil
}

func toHex(b []byte) string {
	const hex = "0123456789ABCDEF"
	out := make([]byte, 0, len(b)*3)
	for i, c := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, hex[c>>4], hex[c&0xF])
	}
	return string(out)
}
