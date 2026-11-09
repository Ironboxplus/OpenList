package portmap

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/portmap/rfc1057"
	"github.com/OpenListTeam/OpenList/v4/pkg/portmap/xdr"
	"github.com/stretchr/testify/require"
	"github.com/willscott/go-nfs-client/nfs/rpc"
)

const testNFSPort = 19392

func startTest(t *testing.T) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, "127.0.0.1:0", testNFSPort)
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); s.Close() })
	return s
}

// call encodes an RPC call message; args is the already encoded argument list.
func call(xid, rpcvers, prog, vers, proc uint32, args []byte) []byte {
	var m rfc1057.Rpc_msg
	m.Xid = xid
	m.Body.Mtype = rfc1057.CALL
	m.Body.Cbody = rfc1057.Call_body{Rpcvers: rpcvers, Prog: prog, Vers: vers, Proc: proc}
	w := xdr.MakeWriter(nil)
	m.Xdr(w)
	return append(w.WriteBuf(), args...)
}

func mappingArgs(prog, vers, prot uint32) []byte {
	m := rfc1057.Mapping{Prog: prog, Vers: vers, Prot: prot}
	w := xdr.MakeWriter(nil)
	m.Xdr(w)
	return w.WriteBuf()
}

// reply is a decoded reply message: the accept status, the mismatch range, and
// the procedure result that follows it.
type reply struct {
	xid    uint32
	denied bool
	stat   rfc1057.Accept_stat
	low    uint32
	high   uint32
	rest   []byte
}

func parseReply(t *testing.T, b []byte) reply {
	t.Helper()
	// Decode by hand rather than with Rpc_msg.Xdr: that one needs the result
	// length to be known, and it is the server's own encoder that is under test.
	u32 := func() uint32 {
		require.GreaterOrEqual(t, len(b), 4, "short reply")
		v := binary.BigEndian.Uint32(b)
		b = b[4:]
		return v
	}
	var r reply
	r.xid = u32()
	require.Equal(t, uint32(rfc1057.REPLY), u32())
	if u32() == uint32(rfc1057.MSG_DENIED) {
		r.denied = true
		require.Equal(t, uint32(rfc1057.RPC_MISMATCH), u32())
		r.low, r.high = u32(), u32()
		return r
	}
	u32() // verf flavor
	require.Zero(t, u32(), "verf body length")
	r.stat = rfc1057.Accept_stat(u32())
	if r.stat == rfc1057.PROG_MISMATCH {
		r.low, r.high = u32(), u32()
	}
	r.rest = b
	return r
}

func udpCall(t *testing.T, s *Server, pkt []byte) []byte {
	t.Helper()
	c, err := net.Dial("udp", s.UDPAddr().String())
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write(pkt)
	require.NoError(t, err)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	require.NoError(t, err)
	return buf[:n]
}

func tcpCall(t *testing.T, c net.Conn, pkt []byte) []byte {
	t.Helper()
	out := binary.BigEndian.AppendUint32(nil, 1<<31|uint32(len(pkt)))
	_, err := c.Write(append(out, pkt...))
	require.NoError(t, err)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var hdr [4]byte
	_, err = io.ReadFull(c, hdr[:])
	require.NoError(t, err)
	h := binary.BigEndian.Uint32(hdr[:])
	require.NotZero(t, h&(1<<31), "reply is a last fragment")
	buf := make([]byte, h&0x7fffffff)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	return buf
}

// transports runs f once over UDP datagrams and once over a TCP connection.
func transports(t *testing.T, s *Server, f func(t *testing.T, send func(pkt []byte) []byte)) {
	t.Run("udp", func(t *testing.T) {
		f(t, func(pkt []byte) []byte { return udpCall(t, s, pkt) })
	})
	t.Run("tcp", func(t *testing.T) {
		c, err := net.Dial("tcp", s.TCPAddr().String())
		require.NoError(t, err)
		defer c.Close()
		f(t, func(pkt []byte) []byte { return tcpCall(t, c, pkt) })
	})
}

func getport(t *testing.T, send func([]byte) []byte, prog, vers, prot uint32) uint32 {
	t.Helper()
	r := parseReply(t, send(call(7, 2, rfc1057.PMAP_PROG, rfc1057.PMAP_VERS, rfc1057.PMAPPROC_GETPORT, mappingArgs(prog, vers, prot))))
	require.False(t, r.denied)
	require.Equal(t, rfc1057.SUCCESS, r.stat)
	require.Len(t, r.rest, 4)
	return binary.BigEndian.Uint32(r.rest)
}

// The client FreeBSD's mount_nfs and Linux's mount both are, as far as the wire
// goes: a real RPC client (go-nfs-client) asking over TCP.
func TestGetportRealClient(t *testing.T) {
	s := startTest(t)
	c, err := rpc.DialTCP("tcp", s.TCPAddr().String(), false)
	require.NoError(t, err)
	defer c.Close()
	pm := &rpc.Portmapper{Client: c}

	for _, prog := range []uint32{100005, 100003} {
		port, err := pm.Getport(rpc.Mapping{Prog: prog, Vers: 3, Prot: rpc.IPProtoTCP})
		require.NoError(t, err)
		require.Equal(t, testNFSPort, port, "prog %d", prog)
	}
	for _, m := range []rpc.Mapping{
		{Prog: 100005, Vers: 3, Prot: rpc.IPProtoUDP}, // we serve TCP only
		{Prog: 100003, Vers: 4, Prot: rpc.IPProtoTCP},
		{Prog: 100005, Vers: 1, Prot: rpc.IPProtoTCP},
		{Prog: 100021, Vers: 4, Prot: rpc.IPProtoTCP}, // nlockmgr is not here
	} {
		port, err := pm.Getport(m)
		require.NoError(t, err)
		require.Zero(t, port, "%+v", m)
	}
}

func TestGetport(t *testing.T) {
	transports(t, startTest(t), func(t *testing.T, send func([]byte) []byte) {
		require.Equal(t, uint32(testNFSPort), getport(t, send, 100005, 3, rfc1057.IPPROTO_TCP))
		require.Equal(t, uint32(testNFSPort), getport(t, send, 100003, 3, rfc1057.IPPROTO_TCP))
		require.Zero(t, getport(t, send, 100005, 3, rfc1057.IPPROTO_UDP))
		require.Zero(t, getport(t, send, 100003, 4, rfc1057.IPPROTO_TCP))
		require.Zero(t, getport(t, send, 1, 1, rfc1057.IPPROTO_TCP))
	})
}

func TestNullAndDump(t *testing.T) {
	transports(t, startTest(t), func(t *testing.T, send func([]byte) []byte) {
		r := parseReply(t, send(call(1, 2, rfc1057.PMAP_PROG, rfc1057.PMAP_VERS, rfc1057.PMAPPROC_NULL, nil)))
		require.Equal(t, rfc1057.SUCCESS, r.stat)
		require.Empty(t, r.rest)

		r = parseReply(t, send(call(2, 2, rfc1057.PMAP_PROG, rfc1057.PMAP_VERS, rfc1057.PMAPPROC_DUMP, nil)))
		require.Equal(t, rfc1057.SUCCESS, r.stat)
		var list rfc1057.Pmaplist
		in := xdr.MakeReader(r.rest)
		list.Xdr(in)
		require.NoError(t, in.Error())
		got := map[uint32]uint32{}
		for e := list.P; e != nil; e = e.Next.P {
			require.Equal(t, uint32(3), e.Map.Vers)
			require.Equal(t, rfc1057.IPPROTO_TCP, e.Map.Prot)
			got[e.Map.Prog] = e.Map.Port
		}
		require.Equal(t, map[uint32]uint32{100003: testNFSPort, 100005: testNFSPort}, got)
	})
}

// SET and UNSET would let any client on the network redirect NFS traffic.
func TestSetUnsetRefused(t *testing.T) {
	transports(t, startTest(t), func(t *testing.T, send func([]byte) []byte) {
		for _, proc := range []uint32{rfc1057.PMAPPROC_SET, rfc1057.PMAPPROC_UNSET} {
			r := parseReply(t, send(call(3, 2, rfc1057.PMAP_PROG, rfc1057.PMAP_VERS, proc, mappingArgs(100003, 3, rfc1057.IPPROTO_TCP))))
			require.Equal(t, rfc1057.SUCCESS, r.stat)
			require.Equal(t, []byte{0, 0, 0, 0}, r.rest, "proc %d must return false", proc)
		}
		// Port 1 would have been the attacker's; the mapping must be untouched.
		require.Equal(t, uint32(testNFSPort), getport(t, send, 100003, 3, rfc1057.IPPROTO_TCP))
	})
}

func TestMismatchAndUnavailable(t *testing.T) {
	transports(t, startTest(t), func(t *testing.T, send func([]byte) []byte) {
		// rpcbind v3 and v4 are what a modern client tries first.
		for _, vers := range []uint32{3, 4} {
			r := parseReply(t, send(call(10+vers, 2, rfc1057.PMAP_PROG, vers, 3, mappingArgs(100005, 3, rfc1057.IPPROTO_TCP))))
			require.Equal(t, 10+vers, r.xid)
			require.False(t, r.denied)
			require.Equal(t, rfc1057.PROG_MISMATCH, r.stat, "vers %d", vers)
			require.Equal(t, [2]uint32{2, 2}, [2]uint32{r.low, r.high})
		}
		r := parseReply(t, send(call(20, 2, 100003, 3, 0, nil)))
		require.Equal(t, rfc1057.PROG_UNAVAIL, r.stat)
		r = parseReply(t, send(call(21, 2, rfc1057.PMAP_PROG, rfc1057.PMAP_VERS, 99, nil)))
		require.Equal(t, rfc1057.PROC_UNAVAIL, r.stat)
		// CALLIT could be abused as a forwarder; it is not offered.
		r = parseReply(t, send(call(22, 2, rfc1057.PMAP_PROG, rfc1057.PMAP_VERS, rfc1057.PMAPPROC_CALLIT, nil)))
		require.Equal(t, rfc1057.PROC_UNAVAIL, r.stat)
		r = parseReply(t, send(call(23, 1, rfc1057.PMAP_PROG, rfc1057.PMAP_VERS, 0, nil)))
		require.True(t, r.denied)
		require.Equal(t, [2]uint32{2, 2}, [2]uint32{r.low, r.high})
	})
}

func TestGarbageArgsKeepsServing(t *testing.T) {
	transports(t, startTest(t), func(t *testing.T, send func([]byte) []byte) {
		// GETPORT with half a mapping.
		r := parseReply(t, send(call(30, 2, rfc1057.PMAP_PROG, rfc1057.PMAP_VERS, rfc1057.PMAPPROC_GETPORT, mappingArgs(100005, 3, 6)[:8])))
		require.Equal(t, uint32(30), r.xid)
		require.Equal(t, rfc1057.GARBAGE_ARGS, r.stat)
		// The same connection (and socket) still answers.
		require.Equal(t, uint32(testNFSPort), getport(t, send, 100005, 3, rfc1057.IPPROTO_TCP))
	})
}

// Packets with no usable header get no reply (there is no xid to put in it)
// and must not take the server down.
func TestUndecodableDropped(t *testing.T) {
	s := startTest(t)
	good := call(40, 2, rfc1057.PMAP_PROG, rfc1057.PMAP_VERS, rfc1057.PMAPPROC_NULL, nil)
	reply := func(xid uint32) []byte { // a REPLY, not a CALL
		b := append([]byte(nil), good...)
		binary.BigEndian.PutUint32(b[0:], xid)
		binary.BigEndian.PutUint32(b[4:], 1)
		return b
	}

	uc, err := net.Dial("udp", s.UDPAddr().String())
	require.NoError(t, err)
	defer uc.Close()
	for _, bad := range [][]byte{{}, {1, 2, 3}, good[:10], reply(41)} {
		_, err := uc.Write(bad)
		require.NoError(t, err)
	}
	require.Equal(t, rfc1057.SUCCESS, parseReply(t, udpCall(t, s, good)).stat)

	tc, err := net.Dial("tcp", s.TCPAddr().String())
	require.NoError(t, err)
	defer tc.Close()
	// Records that are fine as framing but not as RPC are skipped...
	for _, bad := range [][]byte{good[:10], reply(42)} {
		out := binary.BigEndian.AppendUint32(nil, 1<<31|uint32(len(bad)))
		_, err := tc.Write(append(out, bad...))
		require.NoError(t, err)
	}
	require.Equal(t, rfc1057.SUCCESS, parseReply(t, tcpCall(t, tc, good)).stat)

	// ...and a record claiming to be huge or fragmented only costs its connection.
	for _, hdr := range []uint32{1<<31 | 0x7fffffff, 100} {
		c, err := net.Dial("tcp", s.TCPAddr().String())
		require.NoError(t, err)
		_, err = c.Write(binary.BigEndian.AppendUint32(nil, hdr))
		require.NoError(t, err)
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, err = c.Read(make([]byte, 1))
		require.Error(t, err, "server drops the connection")
		c.Close()
	}
	require.Equal(t, rfc1057.SUCCESS, parseReply(t, tcpCall(t, tc, good)).stat)
	require.Equal(t, rfc1057.SUCCESS, parseReply(t, udpCall(t, s, good)).stat)
}

func TestCloseStopsListeners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Start(ctx, "127.0.0.1:0", testNFSPort)
	require.NoError(t, err)
	tcpAddr := s.TCPAddr().String()
	c, err := net.Dial("tcp", tcpAddr)
	require.NoError(t, err)
	defer c.Close()
	s.Close()
	_, err = net.DialTimeout("tcp", tcpAddr, time.Second)
	require.Error(t, err)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = c.Read(make([]byte, 1))
	require.Error(t, err, "open connections are closed too")
}
