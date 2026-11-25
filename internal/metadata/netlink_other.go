//go:build !linux

package metadata

import (
	"errors"
	"net"
)

func addLoopbackAddress(net.IP) error { return errors.New("the relay runs on Linux only") }
