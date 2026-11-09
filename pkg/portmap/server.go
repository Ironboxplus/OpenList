package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/portmap/rfc1057"
	"github.com/OpenListTeam/OpenList/v4/pkg/portmap/xdr"
	log "github.com/sirupsen/logrus"
)

const (
	// A portmap call is a few dozen bytes; anything near this is not a client.
	maxRecord  = 64 << 10
	tcpIdle    = 2 * time.Minute
	rpcVersion = 2
)

// Server answers portmap calls on one UDP socket and one TCP listener.
type Server struct {
	procs map[uint32]xdr.ProcRegistration
	tcp   net.Listener
	udp   net.PacketConn

	wg    sync.WaitGroup
	once  sync.Once
	conns sync.Map // open TCP connections, closed with the server
}

// Start binds addr (host:port) for UDP and TCP and serves until Close or until
// ctx is done. With port 0 the two transports get different ports; see
// TCPAddr and UDPAddr.
func Start(ctx context.Context, addr string, nfsPort uint32) (*Server, error) {
	tcp, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	udp, err := net.ListenPacket("udp", addr)
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	s := &Server{procs: map[uint32]xdr.ProcRegistration{}, tcp: tcp, udp: udp}
	for _, r := range rfc1057.PMAP_PROG_PMAP_VERS_regs(newHandlers(nfsPort)) {
		if r.Proc != rfc1057.PMAPPROC_CALLIT {
			s.procs[r.Proc] = r
		}
	}
	s.wg.Add(2)
	go s.serveUDP()
	go s.serveTCP()
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	log.Infof("[cfs-nfs] portmap on tcp %s and udp %s: mount/nfs v3 -> %d", tcp.Addr(), udp.LocalAddr(), nfsPort)
	return s, nil
}

func (s *Server) TCPAddr() net.Addr { return s.tcp.Addr() }
func (s *Server) UDPAddr() net.Addr { return s.udp.LocalAddr() }

// Close stops both transports and waits for the loops to end.
func (s *Server) Close() {
	s.once.Do(func() {
		_ = s.tcp.Close()
		_ = s.udp.Close()
		s.conns.Range(func(c, _ any) bool { _ = c.(net.Conn).Close(); return true })
	})
	s.wg.Wait()
}

func (s *Server) serveUDP() {
	defer s.wg.Done()
	buf := make([]byte, maxRecord)
	for {
		n, from, err := s.udp.ReadFrom(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Warnf("[cfs-nfs] portmap udp: %v", err)
			}
			return
		}
		if reply := s.handle(buf[:n]); reply != nil {
			if _, err := s.udp.WriteTo(reply, from); err != nil {
				log.Debugf("[cfs-nfs] portmap udp reply to %s: %v", from, err)
			}
		}
	}
}

func (s *Server) serveTCP() {
	defer s.wg.Done()
	for {
		c, err := s.tcp.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Warnf("[cfs-nfs] portmap tcp: %v", err)
			}
			return
		}
		s.conns.Store(c, struct{}{})
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.conns.Delete(c)
			defer c.Close()
			s.runConn(c)
		}()
	}
}

// runConn reads record-marked calls (RFC 5531 section 11) one at a time and
// writes the reply of each as a single last fragment.
func (s *Server) runConn(c net.Conn) {
	for {
		_ = c.SetReadDeadline(time.Now().Add(tcpIdle))
		var hdr [4]byte
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			if !errors.Is(err, io.EOF) {
				log.Debugf("[cfs-nfs] portmap tcp %s: %v", c.RemoteAddr(), err)
			}
			return
		}
		h := binary.BigEndian.Uint32(hdr[:])
		n := h & 0x7fffffff
		if h&(1<<31) == 0 || n > maxRecord {
			log.Debugf("[cfs-nfs] portmap tcp %s: unsupported record (fragmented or %d bytes)", c.RemoteAddr(), n)
			return
		}
		req := make([]byte, n)
		if _, err := io.ReadFull(c, req); err != nil {
			return
		}
		reply := s.handle(req)
		if reply == nil {
			continue
		}
		out := make([]byte, 4, 4+len(reply))
		binary.BigEndian.PutUint32(out, 1<<31|uint32(len(reply)))
		if _, err := c.Write(append(out, reply...)); err != nil {
			return
		}
	}
}

// handle turns one RPC call message into its reply message without record
// marking; nil means there is nothing to answer (undecodable header, so no
// xid to reply to, or not a CALL). It is rfc1057.Server's request handling cut
// loose from the stream so that UDP datagrams and TCP records share it, with
// the version range filled into PROG_MISMATCH and RPC_MISMATCH (RFC 5531
// mismatch_info), which the original leaves zero.
func (s *Server) handle(req []byte) []byte {
	in := xdr.MakeReader(req)
	var call rfc1057.Rpc_msg
	call.Xdr(in)
	if in.Error() != nil || call.Body.Mtype != rfc1057.CALL {
		return nil
	}
	cb := &call.Body.Cbody

	var res rfc1057.Rpc_msg
	res.Xid = call.Xid
	res.Body.Mtype = rfc1057.REPLY
	rb := &res.Body.Rbody
	var body xdr.Xdrable
	if cb.Rpcvers != rpcVersion {
		rb.Stat = rfc1057.MSG_DENIED
		rb.Rreply.Stat = rfc1057.RPC_MISMATCH
		rb.Rreply.Mismatch_info.Low, rb.Rreply.Mismatch_info.High = rpcVersion, rpcVersion
	} else {
		rb.Stat = rfc1057.MSG_ACCEPTED
		rdata := &rb.Areply.Reply_data
		p, ok := s.procs[cb.Proc]
		switch {
		case cb.Prog != rfc1057.PMAP_PROG:
			rdata.Stat = rfc1057.PROG_UNAVAIL
		case cb.Vers != rfc1057.PMAP_VERS:
			rdata.Stat = rfc1057.PROG_MISMATCH
			rdata.Mismatch_info.Low, rdata.Mismatch_info.High = rfc1057.PMAP_VERS, rfc1057.PMAP_VERS
		case !ok:
			rdata.Stat = rfc1057.PROC_UNAVAIL
		default:
			var err error
			if body, err = p.Handler(in); err != nil {
				body = nil
				rdata.Stat = rfc1057.GARBAGE_ARGS
			} else {
				rdata.Stat = rfc1057.SUCCESS
			}
		}
	}

	wr := xdr.MakeWriter(nil)
	res.Xdr(wr)
	if body != nil {
		body.Xdr(wr)
	}
	if wr.Error() != nil {
		return nil
	}
	return wr.WriteBuf()
}
