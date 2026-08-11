package uploader

import "runtime"

// GetPCOSVersion 返回本机 OS 标识（简化版，对应 BLAT app.pl:893-899
// _get_pc_osversion）。不调 wmic；server 端只把 osver 用于展示，
// runtime.GOOS 足够稳定且无 exec 依赖。
func GetPCOSVersion() string { return runtime.GOOS }
