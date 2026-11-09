// Package portmap is a read-only portmap v2 (RFC 1833) server for an NFS
// server that has no rpcbind next to it.
//
// FreeBSD's mount_nfs has no mountport option before 14.x and always asks
// the server's portmapper where MOUNT (100005 v3) lives: 13.1 and 12.4 ask
// over UDP 111, 13.2 and later over TCP 111. rclone's serve nfs answers MOUNT
// and NFS on one TCP port but registers nowhere, so this serves just that
// mapping on both transports.
package portmap

import (
	"context"
	"net"
	"sync"

	"github.com/OpenListTeam/OpenList/v4/pkg/portmap/rfc1057"
	log "github.com/sirupsen/logrus"
)

const (
	progNFS   = 100003
	progMount = 100005
	versNFS3  = 3
)

type key struct {
	Prog, Vers, Prot uint32
}

// handlers answers the procedures of PMAP_VERS; the generated wrapper
// decodes the arguments, so a short argument list becomes GARBAGE_ARGS.
type handlers struct {
	lock sync.RWMutex
	maps map[key]uint32
}

var _ rfc1057.PMAP_PROG_PMAP_VERS_handler = (*handlers)(nil)

func newHandlers(nfsPort uint32) *handlers {
	h := &handlers{maps: map[key]uint32{}}
	for _, prog := range []uint32{progNFS, progMount} {
		h.maps[key{prog, versNFS3, rfc1057.IPPROTO_TCP}] = nfsPort
	}
	return h
}

func (h *handlers) PMAPPROC_NULL() {}

// SET and UNSET stay unimplemented: honouring them would let any client on
// the network redirect or remove the NFS mapping.
func (h *handlers) PMAPPROC_SET(rfc1057.Mapping) rfc1057.Xbool { return false }

func (h *handlers) PMAPPROC_UNSET(rfc1057.Mapping) rfc1057.Xbool { return false }

func (h *handlers) PMAPPROC_GETPORT(m rfc1057.Mapping) rfc1057.Uint32 {
	h.lock.RLock()
	port, ok := h.maps[key{m.Prog, m.Vers, m.Prot}]
	h.lock.RUnlock()
	if !ok {
		log.Debugf("[cfs-nfs] portmap: no mapping for prog=%d vers=%d prot=%d", m.Prog, m.Vers, m.Prot)
		return 0
	}
	log.Debugf("[cfs-nfs] portmap: prog=%d vers=%d prot=%d -> %d", m.Prog, m.Vers, m.Prot, port)
	return rfc1057.Uint32(port)
}

func (h *handlers) PMAPPROC_DUMP() rfc1057.Pmaplist {
	h.lock.RLock()
	defer h.lock.RUnlock()
	var head rfc1057.Pmaplist
	cur := &head
	for k, port := range h.maps {
		cur.P = &rfc1057.Pmaplistelem{Map: rfc1057.Mapping{Prog: k.Prog, Vers: k.Vers, Prot: k.Prot, Port: port}}
		cur = &cur.P.Next
	}
	return head
}

// PMAPPROC_CALLIT is not registered (PROC_UNAVAIL): forwarding calls is only
// useful for broadcast discovery, and an open forwarder is a reflector.
func (h *handlers) PMAPPROC_CALLIT(rfc1057.Call_args) rfc1057.Call_result {
	return rfc1057.Call_result{}
}

// Serve listens for portmap on UDP and TCP at host:111 and maps MOUNT v3 and
// NFS v3 (TCP) to nfsPort. stop closes both listeners.
func Serve(ctx context.Context, host string, nfsPort uint32) (stop func(), err error) {
	s, err := Start(ctx, net.JoinHostPort(host, "111"), nfsPort)
	if err != nil {
		return nil, err
	}
	return s.Close, nil
}
