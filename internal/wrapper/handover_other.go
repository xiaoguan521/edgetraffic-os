//go:build !linux

package wrapper

import "errors"

func dropAndExec(h handover, argv0 string) error {
	return errors.New("handover is only supported on linux")
}
