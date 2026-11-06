//go:build !unix

package alerts

import "errors"

// diskUsage：非 Unix 平台（Windows 版控制台二进制）不提供磁盘使用率告警，评估时跳过该规则并记录警告。
func diskUsage(string) (float64, float64, error) {
	return 0, 0, errors.New("disk usage is not supported on this platform")
}
