//go:build !unix

package collect

import "errors"

// DiskUsage 在非 Unix 平台上不支持，返回错误以便调用方跳过上报。
func DiskUsage(string) (float64, float64, error) {
	return 0, 0, errors.New("disk usage is not supported on this platform")
}
