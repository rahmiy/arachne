// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package ip

import (
	"net"
	"syscall"

	d "github.com/uber/arachne/defines"
	"github.com/uber/arachne/internal/log"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"golang.org/x/net/bpf"
)

type recvSource struct {
	fd int
}

// Conn represents the underyling functionality to send and recv Arachne echo requests.
type Conn struct {
	SrcAddr net.IP
	AF      int
	sendFD  int
	recvSrc recvSource
}

const (
	ip4HeaderLength uint32 = 20 // v4 header bytes
	ip6HeaderLength uint32 = 40 // v6 header bytes
)

const maxPacketSizeBytes int = 1500 // size of packet buffer

// Recvfrom mirrors the syscall of the same name, operating on a recvSource file descriptor
func (r *recvSource) Recvfrom(b []byte) (int, syscall.Sockaddr, error) {
	return syscall.Recvfrom(r.fd, b, 0)
}

// Close is used to close a Conn's send file descriptor and recv source file desciptor.
func (c *Conn) Close(logger *log.Logger) {
	if err := syscall.Close(c.recvSrc.fd); err != nil {
		logger.Error("Error closing Conn recv file descriptor", zap.Error(err))
	}
	if err := syscall.Close(c.sendFD); err != nil {
		logger.Error("Error closing Conn send file descriptor", zap.Error(err))
	}
}

// NextPacket gets bytes of next available packet, and returns them in a decoded gopacket.Packet
func (c *Conn) NextPacket() (gopacket.Packet, error) {
	buf := make([]byte, maxPacketSizeBytes)
	_, _, err := c.recvSrc.Recvfrom(buf)
	if err != nil {
		return nil, err
	}

	switch c.AF {
	case d.AfInet:
		return gopacket.NewPacket(buf, layers.LayerTypeIPv4, gopacket.DecodeOptions{Lazy: true}), nil
	case d.AfInet6:
		return gopacket.NewPacket(buf, layers.LayerTypeIPv6, gopacket.DecodeOptions{Lazy: true}), nil
	}

	return nil, errors.New("no valid decoder available for packet")

}

func ipToSockaddr(family int, ip net.IP, port int) (syscall.Sockaddr, error) {
	switch family {
	case syscall.AF_INET:
		if len(ip) == 0 {
			ip = net.IPv4zero
		}
		ip4 := ip.To4()
		if ip4 == nil {
			return nil, &net.AddrError{Err: "non-IPv4 address", Addr: ip.String()}
		}
		sa := &syscall.SockaddrInet4{Port: port}
		copy(sa.Addr[:], ip4)
		return sa, nil
	case syscall.AF_INET6:
		if len(ip) == 0 || ip.Equal(net.IPv4zero) {
			ip = net.IPv6zero
		}
		ip6 := ip.To16()
		if ip6 == nil {
			return nil, &net.AddrError{Err: "non-IPv6 address", Addr: ip.String()}
		}
		sa := &syscall.SockaddrInet6{Port: port}
		copy(sa.Addr[:], ip6)
		return sa, nil
	}
	return nil, &net.AddrError{Err: "unhandled AF family", Addr: ip.String()}
}

// SendTo operates on a Conn file descriptor and mirrors the Sendto syscall.
func (c *Conn) SendTo(b []byte, to net.IP) error {
	sockAddr, err := ipToSockaddr(c.AF, to, 0)
	if err != nil {
		return err
	}

	return syscall.Sendto(c.sendFD, b, 0, sockAddr)
}

// getSendSocket will create a raw socket for sending data.
func getSendSocket(af int) (int, error) {
	fd, err := syscall.Socket(af, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
	if err != nil {
		return 0, err
	}

	if err = syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_HDRINCL, 1); err != nil {
		return 0, err
	}

	return fd, nil
}

func getBPFFilter(ipHeaderOffset uint32, listenPort uint32) ([]bpf.RawInstruction, error) {
	// The Arachne BPF Filter reads values starting from the TCP Header by adding ipHeaderOffset to all
	// offsets. It filters for packets of destination port equal to listenPort, or src port equal to HTTP or HTTPS ports
	// and for packets containing a TCP SYN flag (SYN, or SYN+ACK packets)
	return bpf.Assemble([]bpf.Instruction{
		bpf.LoadAbsolute{Off: ipHeaderOffset + 2, Size: 2},              // Starting from TCP Header, load DstPort (2nd word)
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: listenPort, SkipTrue: 3},   // Return packet if DstPort is listen Port
		bpf.LoadAbsolute{Off: ipHeaderOffset, Size: 2},                  // Starting from TCP Header, load SrcPort (1st word)
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: d.PortHTTP, SkipTrue: 1},   // Return packet if SrcPort is HTTP Port
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: d.PortHTTPS, SkipFalse: 2}, // Discard packet if not HTTPS
		bpf.LoadAbsolute{Off: ipHeaderOffset + 13, Size: 1},             // Starting from TCP Header, load Flags byte (not including NS bit)
		bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: 2, SkipTrue: 1},          // AND Flags byte with 00000010 (SYN), and drop packet if 0
		bpf.RetConstant{Val: 0},                                         // Drop packet
		bpf.RetConstant{Val: 4096},                                      // Return up to 4096 bytes from packet
	})
}

func getRecvSource(af int, listenPort uint32, intf string, logger *log.Logger) (recvSource, error) {
	var (
		rs             recvSource
		ipHeaderOffset uint32
	)

	fd, err := syscall.Socket(af, syscall.SOCK_RAW, syscall.IPPROTO_TCP)
	if err != nil {
		return rs, err
	}

	if err = bindToDevice(fd, intf); err != nil {
		return rs, err
	}

	rs.fd = fd

	switch af {
	case d.AfInet:
		ipHeaderOffset = ip4HeaderLength
	case d.AfInet6:
		ipHeaderOffset = ip6HeaderLength
	}

	filter, err := getBPFFilter(ipHeaderOffset, uint32(listenPort))
	if err != nil {
		logger.Warn("Failed to compile BPF Filter", zap.Error(err))
		return rs, nil
	}

	// Attempt to attach the BPF filter.
	// This is currently only supported on Linux systems.
	if err := rs.attachBPF(filter); err != nil {
		logger.Warn("Failed to attach BPF filter to recvSource. All incoming packets will be processed",
			zap.Error(err))
	}

	return rs, nil
}

// NewConn returns a raw socket connection to send and receive packets.
func NewConn(af int, listenPort uint32, intf string, srcAddr net.IP, logger *log.Logger) *Conn {
	fdSend, err := getSendSocket(af)
	if err != nil {
		logger.Fatal("Error creating send socket",
			zap.Int("address_family", af),
			zap.Error(err))
	}

	rs, err := getRecvSource(af, listenPort, intf, logger)
	if err != nil {
		logger.Fatal("Error creating recv source",
			zap.Uint32("listenPort", listenPort),
			zap.String("interface", intf),
			zap.Error(err))
	}

	return &Conn{
		SrcAddr: srcAddr,
		AF:      af,
		sendFD:  fdSend,
		recvSrc: rs,
	}
}

func getIPHeaderLayerV6(tos uint8, tcpLen uint16, srcIP net.IP, dstIP net.IP) *layers.IPv6 {
	return &layers.IPv6{
		Version:      6, // IP Version 6
		TrafficClass: tos,
		Length:       tcpLen,
		NextHeader:   layers.IPProtocolTCP,
		SrcIP:        srcIP,
		DstIP:        dstIP,
	}
}

// GetIPHeaderLayer returns the appriately versioned gopacket IP layer
func GetIPHeaderLayer(af int, tos uint8, tcpLen uint16, srcIP net.IP, dstIP net.IP) (gopacket.NetworkLayer, error) {
	switch af {
	case d.AfInet:
		return getIPHeaderLayerV4(tos, tcpLen, srcIP, dstIP), nil
	case d.AfInet6:
		return getIPHeaderLayerV6(tos, tcpLen, srcIP, dstIP), nil
	}

	return nil, errors.New("unhandled AF family")
}
