package pgmux

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgproto3/v2"
)

// cancelKey is the (ProcessID, SecretKey) pair a client must present to cancel
// a running query.
type cancelKey struct {
	pid    uint32
	secret uint32
}

// cancelTarget is where a cancel for that key must actually be sent, together
// with the backend's own key. Clients never learn either.
type cancelTarget struct {
	backend *BackendConfig
	pid     uint32
	secret  uint32
}

// cancelDialTimeout bounds the out-of-band connection a cancel needs. Short:
// the client has already given up on the query, and this holds a session slot.
const cancelDialTimeout = 5 * time.Second

// registerCancelKey issues a proxy-side key for a session and records where its
// cancels go. The backend's own key is never forwarded — it would tell every
// client the backend's PID, and it is only valid on the backend's own address.
func (ps *ProxyServer) registerCancelKey(backend *BackendConfig, real *pgproto3.BackendKeyData) (cancelKey, error) {
	ps.cancelMu.Lock()
	defer ps.cancelMu.Unlock()

	// Retry only guards against a collision with a live session, which is
	// vanishingly unlikely across 64 bits but cheap to rule out.
	for range 8 {
		key, err := newCancelKey()
		if err != nil {
			return cancelKey{}, err
		}
		if _, taken := ps.cancelKeys[key]; taken {
			continue
		}
		ps.cancelKeys[key] = cancelTarget{
			backend: backend,
			pid:     real.ProcessID,
			secret:  real.SecretKey,
		}
		return key, nil
	}
	return cancelKey{}, errors.New("pgmux: could not allocate a cancel key")
}

// newCancelKey draws a key from crypto/rand: a guessable one would let anyone
// cancel a stranger's query.
func newCancelKey() (cancelKey, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return cancelKey{}, err
	}
	key := cancelKey{
		pid:    binary.BigEndian.Uint32(buf[0:4]),
		secret: binary.BigEndian.Uint32(buf[4:8]),
	}
	if key == (cancelKey{}) {
		// The zero key marks "no key registered", so it cannot be a real one.
		key.pid = 1
	}
	return key, nil
}

func (ps *ProxyServer) forgetCancelKey(key cancelKey) {
	ps.cancelMu.Lock()
	defer ps.cancelMu.Unlock()
	delete(ps.cancelKeys, key)
}

func (ps *ProxyServer) lookupCancelKey(key cancelKey) (cancelTarget, bool) {
	ps.cancelMu.Lock()
	defer ps.cancelMu.Unlock()
	target, ok := ps.cancelKeys[key]
	return target, ok
}

// forwardCancel relays a client's cancel to the backend running the query.
//
// PostgreSQL cancels arrive on a fresh, unauthenticated connection: possession
// of the key is the whole authorisation, and the server answers nothing at all,
// success or failure. Both properties are preserved here — an unknown key is
// closed silently rather than refused, so this cannot be used to test whether a
// key is live.
func (ps *ProxyServer) forwardCancel(ctx context.Context, msg *pgproto3.CancelRequest, clientConn net.Conn) {
	client := addrString(clientConn.RemoteAddr())

	if ps.tlsRequired() {
		if _, ok := clientConn.(*tls.Conn); !ok {
			ps.log().Debug("plaintext cancel request refused", "client", client)
			return
		}
	}

	target, ok := ps.lookupCancelKey(cancelKey{pid: msg.ProcessID, secret: msg.SecretKey})
	if !ok {
		ps.log().Debug("cancel request for an unknown key", "client", client)
		return
	}

	addr := net.JoinHostPort(target.backend.Host, strconv.Itoa(target.backend.Port))
	dialer := net.Dialer{Timeout: cancelDialTimeout}
	backendConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		ps.log().Warn("cancel request: backend dial failed", "client", client, "error", err)
		return
	}
	defer backendConn.Close()

	if target.backend.TLS != nil {
		upgraded, err := upgradeBackendToTLS(ctx, backendConn, target.backend.TLS)
		if err != nil {
			ps.log().Warn("cancel request: backend TLS failed", "client", client, "error", err)
			return
		}
		backendConn = upgraded
	}

	// The backend's own key, not the client's: the client only ever held ours.
	buf, _ := (&pgproto3.CancelRequest{ProcessID: target.pid, SecretKey: target.secret}).Encode(nil)
	backendConn.SetWriteDeadline(time.Now().Add(cancelDialTimeout))
	if _, err := backendConn.Write(buf); err != nil {
		ps.log().Warn("cancel request: backend write failed", "client", client, "error", err)
		return
	}
	ps.log().Debug("cancel request forwarded", "client", client)
}
