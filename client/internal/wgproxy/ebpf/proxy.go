//go:build linux && !android

package ebpf

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/hashicorp/go-multierror"
	"github.com/pion/transport/v3"
	log "github.com/sirupsen/logrus"

	"github.com/netbirdio/netbird/client/internal/ebpf"
	ebpfMgr "github.com/netbirdio/netbird/client/internal/ebpf/manager"
	nbnet "github.com/netbirdio/netbird/util/net"
)

// WGEBPFProxy definition for proxy with EBPF support
type WGEBPFProxy struct {
	localWGListenPort int

	ebpfManager   ebpfMgr.Manager
	turnConnStore map[uint16]net.Conn
	turnConnMutex sync.Mutex

	lastUsedPort uint16
	rawConn      net.PacketConn
	conn         transport.UDPConn
	closed       bool

	ctx       context.Context
	ctxCancel context.CancelFunc
}

// NewWGEBPFProxy create new WGEBPFProxy instance
func NewWGEBPFProxy(wgPort int) *WGEBPFProxy {
	log.Debugf("instantiate ebpf proxy")
	wgProxy := &WGEBPFProxy{
		localWGListenPort: wgPort,
		ebpfManager:       ebpf.GetEbpfManagerInstance(),
		turnConnStore:     make(map[uint16]net.Conn),
	}
	return wgProxy
}

// Listen load ebpf program and listen the proxy
func (p *WGEBPFProxy) Listen() error {
	pl := portLookup{}
	wgPorxyPort, err := pl.searchFreePort()
	if err != nil {
		return err
	}

	p.rawConn, err = p.prepareSenderRawSocket()
	if err != nil {
		return err
	}

	err = p.ebpfManager.LoadWgProxy(wgPorxyPort, p.localWGListenPort)
	if err != nil {
		return err
	}

	addr := net.UDPAddr{
		Port: wgPorxyPort,
		IP:   net.ParseIP("127.0.0.1"),
	}

	p.ctx, p.ctxCancel = context.WithCancel(context.Background())

	conn, err := nbnet.ListenUDP("udp", &addr)
	if err != nil {
		cErr := p.Free()
		if cErr != nil {
			log.Errorf("Failed to close the wgproxy: %s", cErr)
		}
		return err
	}
	p.conn = conn

	go p.proxyToRemote()
	log.Infof("local wg proxy listening on: %d", wgPorxyPort)
	return nil
}

// AddTurnConn add new turn connection for the proxy
func (p *WGEBPFProxy) AddTurnConn(ctx context.Context, turnConn net.Conn) (net.Addr, error) {
	wgEndpointPort, err := p.storeTurnConn(turnConn)
	if err != nil {
		return nil, err
	}

	go p.proxyToLocal(ctx, wgEndpointPort, turnConn)
	log.Infof("turn conn added to wg proxy store: %s, endpoint port: :%d", turnConn.RemoteAddr(), wgEndpointPort)

	wgEndpoint := &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: int(wgEndpointPort),
	}
	return wgEndpoint, nil
}

// Free resources
func (p *WGEBPFProxy) Free() error {
	log.Debugf("free up ebpf wg proxy")
	if p.ctx != nil && p.ctx.Err() != nil {
		return nil
	}

	p.ctxCancel()

	var result error
	if err := p.conn.Close(); err != nil {
		result = multierror.Append(result, err)
	}

	if err := p.ebpfManager.FreeWGProxy(); err != nil {
		result = multierror.Append(result, err)
	}

	if err := p.rawConn.Close(); err != nil {
		result = multierror.Append(result, err)
	}
	return result
}

func (p *WGEBPFProxy) proxyToLocal(ctx context.Context, endpointPort uint16, remoteConn net.Conn) {
	defer func() {
		p.removeTurnConn(endpointPort)
	}()

	var err error
	buf := make([]byte, 1500)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			var n int
			n, err = remoteConn.Read(buf)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if err != io.EOF {
					log.Errorf("failed to read from turn conn (endpoint: :%d): %s", endpointPort, err)
				}
				return
			}
			err = p.sendPkg(buf[:n], endpointPort)
			if err != nil {
				if ctx.Err() != nil {
					return
				}

				if p.ctx.Err() != nil {
					return
				}
				log.Errorf("failed to write out turn pkg to local conn: %v", err)
			}
		}
	}
}

// proxyToRemote read messages from local WireGuard interface and forward it to remote conn
// From this go routine has only one instance.
func (p *WGEBPFProxy) proxyToRemote() {
	buf := make([]byte, 1500)
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
			n, addr, err := p.conn.ReadFromUDP(buf)
			if err != nil {
				if p.ctx.Err() != nil {
					return
				}
				log.Errorf("failed to read UDP pkg from WG: %s", err)
				return
			}

			p.turnConnMutex.Lock()
			conn, ok := p.turnConnStore[uint16(addr.Port)]
			p.turnConnMutex.Unlock()
			if !ok {
				if p.ctx.Err() != nil {
					return
				}
				log.Debugf("turn conn not found by port because conn already has been closed: %d", addr.Port)
				continue
			}

			_, err = conn.Write(buf[:n])
			if err != nil {
				if p.ctx.Err() != nil {
					return
				}
				log.Debugf("failed to forward local wg pkg (%d) to remote turn conn: %s", addr.Port, err)
			}
		}
	}
}

func (p *WGEBPFProxy) storeTurnConn(turnConn net.Conn) (uint16, error) {
	p.turnConnMutex.Lock()
	defer p.turnConnMutex.Unlock()

	np, err := p.nextFreePort()
	if err != nil {
		return np, err
	}
	p.turnConnStore[np] = turnConn
	return np, nil
}

func (p *WGEBPFProxy) removeTurnConn(turnConnID uint16) {
	p.turnConnMutex.Lock()
	defer p.turnConnMutex.Unlock()

	_, ok := p.turnConnStore[turnConnID]
	if ok {
		log.Debugf("remove turn conn from store by port: %d", turnConnID)
	}
	delete(p.turnConnStore, turnConnID)
}

func (p *WGEBPFProxy) nextFreePort() (uint16, error) {
	if len(p.turnConnStore) == 65535 {
		return 0, fmt.Errorf("reached maximum turn connection numbers")
	}
generatePort:
	if p.lastUsedPort == 65535 {
		p.lastUsedPort = 1
	} else {
		p.lastUsedPort++
	}

	if _, ok := p.turnConnStore[p.lastUsedPort]; ok {
		goto generatePort
	}
	return p.lastUsedPort, nil
}

func (p *WGEBPFProxy) prepareSenderRawSocket() (net.PacketConn, error) {
	// Create a raw socket.
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
	if err != nil {
		return nil, fmt.Errorf("creating raw socket failed: %w", err)
	}

	// Set the IP_HDRINCL option on the socket to tell the kernel that headers are included in the packet.
	err = syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_HDRINCL, 1)
	if err != nil {
		return nil, fmt.Errorf("setting IP_HDRINCL failed: %w", err)
	}

	// Bind the socket to the "lo" interface.
	err = syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, "lo")
	if err != nil {
		return nil, fmt.Errorf("binding to lo interface failed: %w", err)
	}

	// Set the fwmark on the socket.
	err = nbnet.SetSocketOpt(fd)
	if err != nil {
		return nil, fmt.Errorf("setting fwmark failed: %w", err)
	}

	// Convert the file descriptor to a PacketConn.
	file := os.NewFile(uintptr(fd), fmt.Sprintf("fd %d", fd))
	if file == nil {
		return nil, fmt.Errorf("converting fd to file failed")
	}
	packetConn, err := net.FilePacketConn(file)
	if err != nil {
		return nil, fmt.Errorf("converting file to packet conn failed: %w", err)
	}

	return packetConn, nil
}

func (p *WGEBPFProxy) sendPkg(data []byte, port uint16) error {
	localhost := net.ParseIP("127.0.0.1")

	payload := gopacket.Payload(data)
	ipH := &layers.IPv4{
		DstIP:    localhost,
		SrcIP:    localhost,
		Version:  4,
		TTL:      64,
		Protocol: layers.IPProtocolUDP,
	}
	udpH := &layers.UDP{
		SrcPort: layers.UDPPort(port),
		DstPort: layers.UDPPort(p.localWGListenPort),
	}

	err := udpH.SetNetworkLayerForChecksum(ipH)
	if err != nil {
		return fmt.Errorf("set network layer for checksum: %w", err)
	}

	layerBuffer := gopacket.NewSerializeBuffer()

	err = gopacket.SerializeLayers(layerBuffer, gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true}, ipH, udpH, payload)
	if err != nil {
		return fmt.Errorf("serialize layers: %w", err)
	}
	if _, err = p.rawConn.WriteTo(layerBuffer.Bytes(), &net.IPAddr{IP: localhost}); err != nil {
		return fmt.Errorf("write to raw conn: %w", err)
	}
	return nil
}
