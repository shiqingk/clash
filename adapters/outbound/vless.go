package outbound

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/Dreamacro/clash/component/dialer"
	"github.com/Dreamacro/clash/component/resolver"
	"github.com/Dreamacro/clash/component/vless"
	"github.com/Dreamacro/clash/component/vmess"
	C "github.com/Dreamacro/clash/constant"
	xtls "github.com/xtls/go"
)

type Vless struct {
	*Base
	client *vless.Client
	option *VlessOption
}

type VlessOption struct {
	Name           string            `proxy:"name"`
	Server         string            `proxy:"server"`
	Port           int               `proxy:"port"`
	UUID           string            `proxy:"uuid"`
	UDP            bool              `proxy:"udp,omitempty"`
	TLS            bool              `proxy:"tls,omitempty"`
	Network        string            `proxy:"network,omitempty"`
	WSPath         string            `proxy:"ws-path,omitempty"`
	WSHeaders      map[string]string `proxy:"ws-headers,omitempty"`
	SkipCertVerify bool              `proxy:"skip-cert-verify,omitempty"`
	ServerName     string            `proxy:"servername,omitempty"`
	Flow           string            `proxy:"flow,omitempty"`
}

func (v *Vless) StreamConn(c net.Conn, metadata *C.Metadata) (net.Conn, error) {
	var err error
	switch v.option.Network {
	case "ws":
		host, port, _ := net.SplitHostPort(v.addr)
		wsOpts := &vmess.WebsocketConfig{
			Host: host,
			Port: port,
			Path: v.option.WSPath,
		}

		if len(v.option.WSHeaders) != 0 {
			header := http.Header{}
			for key, value := range v.option.WSHeaders {
				header.Add(key, value)
			}
			wsOpts.Headers = header
		}

		if v.option.TLS {
			wsOpts.TLS = true
			wsOpts.SessionCache = getClientSessionCache()
			wsOpts.SkipCertVerify = v.option.SkipCertVerify
			wsOpts.ServerName = v.option.ServerName
		}
		c, err = vmess.StreamWebsocketConn(c, wsOpts)
	default:
		// handle TLS
		if v.option.TLS {
			host, _, _ := net.SplitHostPort(v.addr)

			if v.option.Flow == vless.XRO {
				xtlsConfig := &xtls.Config{
					ServerName:         host,
					InsecureSkipVerify: v.option.SkipCertVerify,
					ClientSessionCache: getXTLSSessionCache(),
				}

				if v.option.ServerName != "" {
					xtlsConfig.ServerName = v.option.ServerName
				}
				xtlsConn := xtls.Client(c, xtlsConfig)
				if err = xtlsConn.Handshake(); err != nil {
					return nil, err
				}

				c = xtlsConn
			} else {
				tlsConfig := &tls.Config{
					ServerName:         host,
					InsecureSkipVerify: v.option.SkipCertVerify,
					ClientSessionCache: getClientSessionCache(),
				}
				if v.option.ServerName != "" {
					tlsConfig.ServerName = v.option.ServerName
				}
				tlsConn := tls.Client(c, tlsConfig)
				if err = tlsConn.Handshake(); err != nil {
					return nil, err
				}

				c = tlsConn
			}

		}
	}

	if err != nil {
		return nil, err
	}

	return v.client.StreamConn(c, parseVmessAddr(metadata))
}

func (v *Vless) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	c, err := dialer.DialContext(ctx, "tcp", v.addr)
	if err != nil {
		return nil, fmt.Errorf("%s connect error: %s", v.addr, err.Error())
	}
	tcpKeepAlive(c)

	c, err = v.StreamConn(c, metadata)
	return NewConn(c, v), err
}

func (v *Vless) DialUDP(metadata *C.Metadata) (C.PacketConn, error) {
	// vless use stream-oriented udp, so clash needs a net.UDPAddr
	if !metadata.Resolved() {
		ip, err := resolver.ResolveIP(metadata.Host)
		if err != nil {
			return nil, errors.New("can't resolve ip")
		}
		metadata.DstIP = ip
	}

	ctx, cancel := context.WithTimeout(context.Background(), tcpTimeout)
	defer cancel()
	c, err := dialer.DialContext(ctx, "tcp", v.addr)
	if err != nil {
		return nil, fmt.Errorf("%s connect error: %s", v.addr, err.Error())
	}
	tcpKeepAlive(c)
	c, err = v.StreamConn(c, metadata)
	if err != nil {
		return nil, fmt.Errorf("new vless client error: %v", err)
	}

	vc := &vmessPacketConn{Conn: c, rAddr: metadata.UDPAddr()}
	var pc net.PacketConn = vc
	if v.option.Flow == vless.XRO {
		pc = newLenghtPacketConn(vc)
	}
	return newPacketConn(pc, v), nil
}

func NewVless(option VlessOption) (*Vless, error) {
	var addons *vless.Addons
	if option.TLS && option.Flow == vless.XRO {
		addons = &vless.Addons{
			Flow: vless.XRO,
		}
	}

	client, err := vless.NewClient(option.UUID, addons)
	if err != nil {
		return nil, err
	}

	return &Vless{
		Base: &Base{
			name: option.Name,
			addr: net.JoinHostPort(option.Server, strconv.Itoa(option.Port)),
			tp:   C.Vless,
			udp:  true,
		},
		client: client,
		option: &option,
	}, nil
}

func newLenghtPacketConn(vc *vmessPacketConn) *lengthPacketConn {
	return &lengthPacketConn{vmessPacketConn: vc}
}

type lengthPacketConn struct {
	*vmessPacketConn
	remain int
	mux    sync.Mutex
}

func (c *lengthPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	buf := &bytes.Buffer{}
	binary.Write(buf, binary.BigEndian, uint16(len(b)))
	buf.Write(b)
	return c.Conn.Write(buf.Bytes())
}

func (c *lengthPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.mux.Lock()
	defer c.mux.Unlock()

	length := len(b)
	if c.remain > 0 {
		if c.remain < length {
			length = c.remain
		}

		n, err := c.Conn.Read(b[:length])
		if err != nil {
			return 0, nil, err
		}

		c.remain -= n
		return n, c.rAddr, nil
	}

	var packetLength uint16
	if err := binary.Read(c.Conn, binary.BigEndian, &packetLength); err != nil {
		return 0, nil, err
	}

	remain := int(packetLength)
	n, err := c.Conn.Read(b[:length])
	remain -= n
	if remain > 0 {
		c.remain = remain
	}
	return n, c.rAddr, err
}
