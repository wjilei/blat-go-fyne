package device

import "context"

// Device is the minimum contract every hardware driver must satisfy in the
// Go port. It is intentionally small so concrete drivers (serial, network,
// Modbus, GPIB...) can wrap whatever native library they need.
//
// Usage:
//
//	d := serial.New("COM3", 9600)
//	if err := d.Open(ctx); err != nil { ... }
//	defer d.Close()
//	resp, err := d.Command(ctx, "*IDN?")
type Device interface {
	Open(ctx context.Context) error
	Close() error
	Command(ctx context.Context, cmd string) (string, error)
}
