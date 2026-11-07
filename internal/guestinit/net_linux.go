package guestinit

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"syscall"
)

// network brings up lo and the first other interface, with ip (a CIDR) and a
// default route through gateway. It speaks netlink itself: the image's ip
// command is the repo's code, and this runs before the token is spent.
func (machine) network(ip, gateway string) error {
	if ip == "" {
		return nil // the kernel's ip= did it
	}
	addr, err := netip.ParsePrefix(ip)
	if err != nil || !addr.Addr().Is4() {
		return fmt.Errorf("bad forecrew.ip %q", ip)
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	lo := slices.IndexFunc(ifaces, func(i net.Interface) bool { return i.Flags&net.FlagLoopback != 0 })
	nic := slices.IndexFunc(ifaces, func(i net.Interface) bool { return i.Flags&net.FlagLoopback == 0 })
	if nic < 0 {
		return errors.New("no network device")
	}
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	msgs := [][]byte{linkUp(int32(ifaces[nic].Index)), addAddress(int32(ifaces[nic].Index), addr)} // #nosec G115 -- an interface index
	if lo >= 0 {
		msgs = append(msgs, linkUp(int32(ifaces[lo].Index))) // #nosec G115 -- an interface index
	}
	if gateway != "" {
		gw, err := netip.ParseAddr(gateway)
		if err != nil || !gw.Is4() {
			return fmt.Errorf("bad forecrew.gw %q", gateway)
		}
		msgs = append(msgs, defaultRoute(gw))
	}
	for i, m := range msgs {
		if err := request(fd, uint32(i+1), m); err != nil { // #nosec G115 -- a few messages
			return err
		}
	}
	return nil
}

// request sends one message and reads its acknowledgement.
func request(fd int, seq uint32, msg []byte) error {
	binary.NativeEndian.PutUint32(msg[8:], seq)
	if err := syscall.Sendto(fd, msg, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	return parseAck(buf[:n])
}

// parseAck reads a netlink acknowledgement.
func parseAck(buf []byte) error {
	msgs, err := syscall.ParseNetlinkMessage(buf)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		return errors.New("no answer from netlink")
	}
	for _, m := range msgs {
		if m.Header.Type == syscall.NLMSG_ERROR && len(m.Data) >= 4 {
			if code := int32(binary.NativeEndian.Uint32(m.Data)); code != 0 { // #nosec G115 -- a negative errno
				return syscall.Errno(-code)
			}
		}
	}
	return nil
}

// message builds a netlink request of the type, with its fixed part and
// attributes.
func message(kind uint16, flags uint16, fixed []byte, attrs ...[]byte) []byte {
	b := make([]byte, syscall.SizeofNlMsghdr)
	b = append(b, fixed...)
	for _, a := range attrs {
		b = append(b, a...)
	}
	binary.NativeEndian.PutUint32(b[0:], uint32(len(b))) // #nosec G115 -- a short message
	binary.NativeEndian.PutUint16(b[4:], kind)
	binary.NativeEndian.PutUint16(b[6:], syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|flags)
	return b
}

// attr builds a four-byte-valued attribute, which needs no padding.
func attr(kind uint16, addr netip.Addr) []byte {
	b := make([]byte, 4, 8)
	binary.NativeEndian.PutUint16(b[0:], 8)
	binary.NativeEndian.PutUint16(b[2:], kind)
	v := addr.As4()
	return append(b, v[:]...)
}

func linkUp(index int32) []byte {
	info := make([]byte, syscall.SizeofIfInfomsg)
	binary.NativeEndian.PutUint32(info[4:], uint32(index)) // #nosec G115 -- an interface index
	binary.NativeEndian.PutUint32(info[8:], syscall.IFF_UP)
	binary.NativeEndian.PutUint32(info[12:], syscall.IFF_UP)
	return message(syscall.RTM_NEWLINK, 0, info)
}

func addAddress(index int32, addr netip.Prefix) []byte {
	info := make([]byte, syscall.SizeofIfAddrmsg)
	info[0], info[1] = syscall.AF_INET, byte(addr.Bits())  // #nosec G115 -- 0 to 32
	binary.NativeEndian.PutUint32(info[4:], uint32(index)) // #nosec G115 -- an interface index
	return message(syscall.RTM_NEWADDR, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, info,
		attr(syscall.IFA_LOCAL, addr.Addr()), attr(syscall.IFA_ADDRESS, addr.Addr()))
}

func defaultRoute(gateway netip.Addr) []byte {
	info := make([]byte, syscall.SizeofRtMsg)
	info[0] = syscall.AF_INET
	info[4], info[5], info[6], info[7] = syscall.RT_TABLE_MAIN, syscall.RTPROT_BOOT, syscall.RT_SCOPE_UNIVERSE, syscall.RTN_UNICAST
	return message(syscall.RTM_NEWROUTE, syscall.NLM_F_CREATE|syscall.NLM_F_EXCL, info, attr(syscall.RTA_GATEWAY, gateway))
}
