package fyneui

import (
	"fmt"
	"time"
)

// formatDuration 把 d 格式化为 "MM:SS.mmm" 形式（毫秒精度），便于在
// 测试计划树节点里实时显示已用时长。产线单次测试基本不超过 1h，分钟位
// 两位宽足够；超过 99 分钟仍按 99:59.999 上限显示，不溢出。
//
// 负值被夹到 0（防御性：time.Since 在系统时钟回拨时可能为负）。
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalMs := d.Milliseconds()
	totalSec := totalMs / 1000
	m := totalSec / 60
	s := totalSec % 60
	ms := totalMs % 1000
	return fmt.Sprintf("%02d:%02d.%03d", m, s, ms)
}
