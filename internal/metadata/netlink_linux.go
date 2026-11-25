package metadata

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// addLoopbackAddress adds ip/32 to lo over rtnetlink; already there is fine.
func addLoopbackAddress(ip net.IP) error {
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	attr := func(typ uint16, data []byte) []byte {
		b := make([]byte, unix.SizeofRtAttr, unix.SizeofRtAttr+len(data)+3)
		*(*unix.RtAttr)(unsafePointer(&b[0])) = unix.RtAttr{Len: uint16(unix.SizeofRtAttr + len(data)), Type: typ}
		b = append(b, data...)
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		return b
	}
	msg := make([]byte, unix.SizeofNlMsghdr+unix.SizeofIfAddrmsg)
	*(*unix.IfAddrmsg)(unsafePointer(&msg[unix.SizeofNlMsghdr])) = unix.IfAddrmsg{
		Family: unix.AF_INET, Prefixlen: 32, Scope: unix.RT_SCOPE_HOST, Index: uint32(lo.Index),
	}
	msg = append(msg, attr(unix.IFA_LOCAL, ip)...)
	msg = append(msg, attr(unix.IFA_ADDRESS, ip)...)
	*(*unix.NlMsghdr)(unsafePointer(&msg[0])) = unix.NlMsghdr{
		Len: uint32(len(msg)), Type: unix.RTM_NEWADDR, Flags: unix.NLM_F_REQUEST | unix.NLM_F_ACK | unix.NLM_F_CREATE | unix.NLM_F_EXCL, Seq: 1,
	}
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, _, err := unix.Recvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	if n < unix.SizeofNlMsghdr+4 {
		return fmt.Errorf("short netlink answer")
	}
	hdr := (*unix.NlMsghdr)(unsafePointer(&buf[0]))
	if hdr.Type != unix.NLMSG_ERROR {
		return fmt.Errorf("unexpected netlink answer %d", hdr.Type)
	}
	errno := -int32(nativeUint32(buf[unix.SizeofNlMsghdr:]))
	switch syscall.Errno(errno) {
	case 0, unix.EEXIST:
		return nil
	default:
		return syscall.Errno(errno)
	}
}
