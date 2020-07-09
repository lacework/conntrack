package conntrack

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"syscall"
	"unsafe"
)

type nfgenmsg struct {
	Family  uint8  /* AF_xxx */
	Version uint8  /* nfnetlink version */
	ResID   uint16 /* resource id */
}

const (
	sizeofGenmsg = uint32(unsafe.Sizeof(nfgenmsg{})) // TODO
)

type ConntrackListReq struct {
	Header syscall.NlMsghdr
	Body   nfgenmsg
}

func (c *ConntrackListReq) toWireFormat() []byte {
	// adapted from syscall/NetlinkRouteRequest.toWireFormat
	b := make([]byte, c.Header.Len)
	*(*uint32)(unsafe.Pointer(&b[0:4][0])) = c.Header.Len
	*(*uint16)(unsafe.Pointer(&b[4:6][0])) = c.Header.Type
	*(*uint16)(unsafe.Pointer(&b[6:8][0])) = c.Header.Flags
	*(*uint32)(unsafe.Pointer(&b[8:12][0])) = c.Header.Seq
	*(*uint32)(unsafe.Pointer(&b[12:16][0])) = c.Header.Pid
	b[16] = byte(c.Body.Family)
	b[17] = byte(c.Body.Version)
	*(*uint16)(unsafe.Pointer(&b[18:20][0])) = c.Body.ResID
	return b
}

func connectNetfilter(groups uint32) (int, *syscall.SockaddrNetlink, error) {
	s, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_NETFILTER)
	if err != nil {
		return 0, nil, err
	}
	lsa := &syscall.SockaddrNetlink{
		Family: syscall.AF_NETLINK,
		Groups: groups,
	}
	if err := syscall.Bind(s, lsa); err != nil {
		syscall.Close(s)
		return 0, nil, err
	}
	return s, lsa, nil
}

// Established lists all established TCP connections.
func Established() ([]ConnTCP, []Conn, error) {
	s, lsa, err := connectNetfilter(0)
	if err != nil {
		return nil, nil, err
	}
	defer syscall.Close(s)

	var conns []ConnTCP
	var conns2 []Conn
	msg := ConntrackListReq{
		Header: syscall.NlMsghdr{
			Len:   syscall.NLMSG_HDRLEN + sizeofGenmsg,
			Type:  (NFNL_SUBSYS_CTNETLINK << 8) | uint16(IpctnlMsgCtGet),
			Flags: syscall.NLM_F_REQUEST | syscall.NLM_F_DUMP,
			Pid:   0,
			Seq:   0,
		},
		Body: nfgenmsg{
			Family:  syscall.AF_INET,
			Version: NFNETLINK_V0,
			ResID:   0,
		},
	}
	wb := msg.toWireFormat()
	//fmt.Printf("msg bytes: %q\n", wb)
	if err := syscall.Sendto(s, wb, 0, lsa); err != nil {
		return nil, nil, err
	}

	local := localIPs()

	next := func(c *Conn) {
		conns2 = append(conns2, *c)
		if tc := c.ConnTCP(local); tc != nil {
			conns = append(conns, *tc)
		}
	}
	readMsgs(s, next)

	return conns, conns2, nil
}

// Follow gives a channel with all changes.
func Follow(newonly bool) (func(), func(cb func(*Conn)) error, error) {
	flags := NF_NETLINK_CONNTRACK_NEW | NF_NETLINK_CONNTRACK_UPDATE | NF_NETLINK_CONNTRACK_DESTROY
	if newonly {
		flags = NF_NETLINK_CONNTRACK_NEW
	}
	s, _, err := connectNetfilter(uint32(flags))
	stop := func() {
		syscall.Close(s)
	}
	if err != nil {
		syscall.Close(s)
		return nil, nil, err
	}
	next := func(cb func(*Conn)) error {
		defer syscall.Close(s)

		err := readMsgs(s, cb)
		return err
	}
	return stop, next, nil
}

func readMsgs(s int, cb func(*Conn)) error {

	rb := make([]byte, 2*syscall.Getpagesize()) // TODO: re-use
	conn := &Conn{}

	for {
		nr, _, err := syscall.Recvfrom(s, rb, 0)
		if err == syscall.ENOBUFS {
			// ENOBUF means we miss some events here. No way around it. That's life.
			continue
		} else if err != nil {
			return err
		}
		//fmt.Println(nr, rb)

		msgs, err := syscall.ParseNetlinkMessage(rb[:nr])
		if err != nil {
			return err
		}
		//fmt.Println(msgs)
		for _, msg := range msgs {
			if err := nfnlIsError(msg.Header); err != nil {
				return fmt.Errorf("msg is some error: %s\n", err)
			}
			if nflnSubsysID(msg.Header.Type) != NFNL_SUBSYS_CTNETLINK {
				return fmt.Errorf(
					"unexpected subsys_id: %d\n",
					nflnSubsysID(msg.Header.Type),
				)
			}
			err := parsePayload(msg.Data[sizeofGenmsg:], conn)
			if err != nil {
				if err.Error() == "Not supported" {
					continue
				}
				return err
			}
			if conn.Orig.Proto != syscall.IPPROTO_TCP {
				continue
			}
			cb(conn)
		}
	}
}

type Tuple struct {
	Src     string
	SrcPort uint16
	Dst     string
	DstPort uint16
	Proto   int
}

func (t Tuple) String() string {
	return fmt.Sprintf("%v:%d -> %v:%d", []byte(t.Src), t.SrcPort, []byte(t.Dst), t.DstPort)
}

type Conn struct {
	MsgType  NfConntrackMsg
	Orig     Tuple
	Reply    Tuple
	TCPState string
}

func (c Conn) String() string {
	return fmt.Sprintf("%v --- %v", c.Orig, c.Reply)
}

// ConnTCP decides which way this connection is going and makes a ConnTCP.
func (c Conn) ConnTCP(local map[string]struct{}) *ConnTCP {
	// conntrack gives us all connections, even things passing through, but it
	// doesn't tell us what the local IP is. So we use `local` as a guide
	// what's local.

	src := net.IP(c.Reply.Src).String()
	dst := net.IP(c.Reply.Dst).String()
	_, srcLocal := local[src]
	_, dstLocal := local[dst]

	// If both are local we must just order things predictably.
	if srcLocal && dstLocal {
		srcLocal = c.Reply.SrcPort < c.Reply.DstPort
	}
	if srcLocal {
		return &ConnTCP{
			Local:      src,
			LocalPort:  strconv.Itoa(int(c.Reply.SrcPort)),
			Remote:     dst,
			RemotePort: strconv.Itoa(int(c.Reply.DstPort)),
		}
	}
	if dstLocal {
		return &ConnTCP{
			Local:      dst,
			LocalPort:  strconv.Itoa(int(c.Reply.DstPort)),
			Remote:     src,
			RemotePort: strconv.Itoa(int(c.Reply.SrcPort)),
		}
	}
	// Neither is local. conntrack also reports NAT connections.
	return nil
}

func parsePayload(b []byte, conn *Conn) error {

	attrs := make([]Attr, 20)
	// Most of this comes from libnetfilter_conntrack/src/conntrack/parse_mnl.c

	idx, err := parseAttrs(b, attrs)
	if err != nil {
		return err
	}
	for i, attr := range attrs {
		if i == idx {
			break
		}
		switch CtattrType(attr.Typ) {
		case CtaTupleOrig:
			err = parseTuple(attr.Msg, &conn.Orig)
			if err != nil {
				return err
			}
		case CtaTupleReply:
			// fmt.Printf("It's a reply\n")
			// We take the reply, nor the orig.... Sure?
			err = parseTuple(attr.Msg, &conn.Reply)
			if err != nil {
				return err
			}
		case CtaStatus:
			// These are ip_conntrack_status
			// status := binary.BigEndian.Uint32(attr.Msg)
			// fmt.Printf("It's status %d\n", status)
		case CtaProtoinfo:
			//			parseProtoinfo(attr.Msg, conn)
		}
	}
	return nil
}

func parseTuple(b []byte, t *Tuple) error {

	attrs := make([]Attr, 20)
	idx, err := parseAttrs(b, attrs)
	if err != nil {
		return fmt.Errorf("invalid tuple attr: %s", err)
	}
	for i, attr := range attrs {
		if i == idx {
			break
		}
		// fmt.Printf("pl: %d, type: %d, multi: %t, bigend: %t\n", len(attr.Msg), attr.Typ, attr.IsNested, attr.IsNetByteorder)
		switch CtattrTuple(attr.Typ) {
		case CtaTupleUnspec:
			// fmt.Printf("It's a tuple unspec\n")
		case CtaTupleIp:
			// fmt.Printf("It's a tuple IP\n")
			if err := parseIP(attr.Msg, t); err != nil {
				return err
			}
		case CtaTupleProto:
			// fmt.Printf("It's a tuple proto\n")
			parseProto(attr.Msg, t)
		}
	}
	return nil
}

func parseIP(b []byte, t *Tuple) error {

	attrs := make([]Attr, 20)
	idx, err := parseAttrs(b, attrs)
	if err != nil {
		return fmt.Errorf("invalid tuple attr: %s", err)
	}
	for i, attr := range attrs {
		if i == idx {
			break
		}
		switch CtattrIp(attr.Typ) {
		case CtaIpV4Src:
			t.Src = string(attr.Msg)
		case CtaIpV4Dst:
			t.Dst = string(attr.Msg)
		case CtaIpV6Src:
			return fmt.Errorf("Not supported")
			// TODO
		case CtaIpV6Dst:
			return fmt.Errorf("Not supported")
			// TODO
		}
	}
	return nil
}

func parseProto(b []byte, t *Tuple) error {

	attrs := make([]Attr, 20)
	idx, err := parseAttrs(b, attrs)
	if err != nil {
		return fmt.Errorf("invalid tuple attr: %s", err)
	}
	for i, attr := range attrs {
		if i == idx {
			break
		}
		switch CtattrL4proto(attr.Typ) {
		case CtaProtoNum:
			t.Proto = int(uint8(attr.Msg[0]))
		case CtaProtoSrcPort:
			t.SrcPort = binary.BigEndian.Uint16(attr.Msg)
		case CtaProtoDstPort:
			t.DstPort = binary.BigEndian.Uint16(attr.Msg)
		}
	}
	return nil
}

/*
func parseProtoinfo(b []byte, conn *Conn) error {
	attrs, err := parseAttrs(b)
	if err != nil {
		return fmt.Errorf("invalid tuple attr: %s", err)
	}
	for _, attr := range attrs {
		switch CtattrProtoinfo(attr.Typ) {
		case CtaProtoinfoTcp:
			if err := parseProtoinfoTCP(attr.Msg, conn); err != nil {
				return err
			}
		default:
			// we're not interested in other protocols
		}
	}
	return nil
}

func parseProtoinfoTCP(b []byte, conn *Conn) error {
	attrs, err := parseAttrs(b)
	if err != nil {
		return fmt.Errorf("invalid tuple attr: %s", err)
	}
	for _, attr := range attrs {
		switch CtattrProtoinfoTcp(attr.Typ) {
		case CtaProtoinfoTcpState:
			conn.TCPState = tcpState[uint8(attr.Msg[0])]
		default:
			// not interested
		}
	}
	return nil
}
*/
