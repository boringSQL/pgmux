package pgmux

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgproto3/v2"
)

type (
	// ConnectionPool manages a pool of connections to a backend server
	ConnectionPool struct {
		mu          sync.Mutex
		connections []*BackendConnection
		maxSize     int
		config      *BackendConfig
	}

	// BackendConnection represents a connection to a backend PostgreSQL server
	BackendConnection struct {
		conn     net.Conn
		inUse    bool
		lastUsed time.Time
	}

	// TLSConfig holds TLS configuration for the proxy server
	TLSConfig struct {
		// Enable TLS support
		Enabled bool
		// Path to certificate file
		CertFile string
		// Path to key file
		KeyFile string
		// Optional TLS config for advanced settings
		Config *tls.Config
	}

	// Limits configures runtime resource limits on the proxy.
	// Zero or negative values fall back to defaults.
	Limits struct {
		MaxConnections    int           // default 1024
		MaxMessageSize    int           // bytes, default 16 MiB; applies to client-facing reads only
		ClientIdleTimeout time.Duration // kills conn if client sends nothing for this long; 0 disables (default)
	}

	// ProxyServer is a PostgreSQL proxy server that routes connections based on username
	ProxyServer struct {
		listenAddr     string
		router         Router
		pools          map[string]*ConnectionPool
		mu             sync.RWMutex
		tlsConfig      *TLSConfig
		limits         *Limits
		logger         *slog.Logger
		startupRewrite StartupRewriteFunc
		resolvedTLS    *tls.Config
	}

	// StartupRewriteFunc may mutate the startup parameters sent to the backend.
	// It runs after routing and the user/database rewrites, immediately before
	// the StartupMessage is encoded. pgmux ships no policy of its own here.
	StartupRewriteFunc func(clientAddr net.Addr, params map[string]string)
)

// Client-visible failure messages are fixed strings: a wrapped Go error would
// leak the backend host, port and topology. Detail goes to the log.
const (
	msgBackendUnavailable = "backend unavailable"
	msgNotAuthorized      = "no backend available for this connection"
)

const (
	defaultMaxConnections = 1024
	defaultMaxMessageSize = 16 * 1024 * 1024
	tlsHandshakeTimeout   = 10 * time.Second
	// startupTimeout bounds how long a connection may occupy a slot without
	// sending its StartupMessage.
	startupTimeout = 10 * time.Second
)

// cappedChunkReader rejects Next(n) when n exceeds max, before pgproto3
// allocates buffer sized from an attacker-controlled wire length prefix.
type cappedChunkReader struct {
	inner pgproto3.ChunkReader
	max   int
}

func (c *cappedChunkReader) Next(n int) ([]byte, error) {
	if n > c.max {
		return nil, fmt.Errorf("pgmux: client message size %d exceeds max %d", n, c.max)
	}
	return c.inner.Next(n)
}

func (ps *ProxyServer) newClientChunkReader(r io.Reader) pgproto3.ChunkReader {
	return &cappedChunkReader{inner: pgproto3.NewChunkReader(r), max: ps.maxMessageSize()}
}

// NewProxyServer creates a new ProxyServer with the given listen address and router
func NewProxyServer(listenAddr string, router Router) *ProxyServer {
	return &ProxyServer{
		listenAddr: listenAddr,
		router:     router,
		pools:      make(map[string]*ConnectionPool),
	}
}

// WithTLS configures TLS support for the proxy server.
//
// All With… setters must be called before Start: they write fields the accept
// loop reads without synchronisation.
func (ps *ProxyServer) WithTLS(config *TLSConfig) *ProxyServer {
	ps.tlsConfig = config
	return ps
}

// WithLimits configures resource limits for the proxy server.
func (ps *ProxyServer) WithLimits(limits *Limits) *ProxyServer {
	ps.limits = limits
	return ps
}

// WithLogger sets the structured logger. Defaults to slog.Default().
// Per-connection detail (startup parameters, protocol chatter) is logged at
// debug level; enable it with a handler whose level is slog.LevelDebug.
func (ps *ProxyServer) WithLogger(logger *slog.Logger) *ProxyServer {
	ps.logger = logger
	return ps
}

// WithStartupRewrite installs a hook that may mutate the startup parameters
// forwarded to the backend. Nil (the default) is a no-op.
func (ps *ProxyServer) WithStartupRewrite(fn StartupRewriteFunc) *ProxyServer {
	ps.startupRewrite = fn
	return ps
}

func (ps *ProxyServer) log() *slog.Logger {
	if ps.logger != nil {
		return ps.logger
	}
	return slog.Default()
}

func (ps *ProxyServer) maxConnections() int {
	if ps.limits != nil && ps.limits.MaxConnections > 0 {
		return ps.limits.MaxConnections
	}
	return defaultMaxConnections
}

func (ps *ProxyServer) maxMessageSize() int {
	if ps.limits != nil && ps.limits.MaxMessageSize > 0 {
		return ps.limits.MaxMessageSize
	}
	return defaultMaxMessageSize
}

func (ps *ProxyServer) clientIdleTimeout() time.Duration {
	if ps.limits != nil && ps.limits.ClientIdleTimeout > 0 {
		return ps.limits.ClientIdleTimeout
	}
	return 0
}

// Start starts the proxy server and listens for connections
func (ps *ProxyServer) Start(ctx context.Context) error {
	// Resolve TLS once, at startup, so a bad path fails here rather than on
	// the first client connection.
	if ps.tlsConfig != nil && ps.tlsConfig.Enabled {
		resolved, err := resolveServerTLS(ps.tlsConfig)
		if err != nil {
			return err
		}
		ps.resolvedTLS = resolved
	}

	// Always start with a plain TCP listener
	// TLS upgrade happens after SSL negotiation
	listener, err := net.Listen("tcp", ps.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	defer listener.Close()

	sem := make(chan struct{}, ps.maxConnections())

	ps.log().Info("proxy listening",
		"addr", ps.listenAddr,
		"tls", ps.resolvedTLS != nil,
		"max_connections", cap(sem))

	// Close listener when context is cancelled
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				ps.log().Error("failed to accept connection", "error", err)
				continue
			}
		}

		select {
		case sem <- struct{}{}:
			// Logged only on the accepted path, so counting these lines gives
			// the connection count even when the cap is being hit.
			ps.log().Info("connection accepted", "client", conn.RemoteAddr().String())
			go func() {
				defer func() { <-sem }()
				defer ps.recoverConn(conn.RemoteAddr())
				ps.handleConnection(ctx, conn)
			}()
		default:
			// In a goroutine so a client that stops reading cannot stall the
			// accept loop.
			go func() {
				defer ps.recoverConn(conn.RemoteAddr())
				ps.rejectOverCapacity(conn)
			}()
		}
	}
}

// recoverConn keeps one malformed connection from taking the process down.
// Every per-connection goroutine defers this.
func (ps *ProxyServer) recoverConn(addr net.Addr) {
	if r := recover(); r != nil {
		ps.log().Error("panic handling connection",
			"client", addrString(addr),
			"panic", fmt.Sprint(r),
			"stack", string(debug.Stack()))
	}
}

func addrString(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}

// resolveServerTLS turns a TLSConfig into the *tls.Config used for every client
// handshake, failing if it cannot produce a usable one.
func resolveServerTLS(cfg *TLSConfig) (*tls.Config, error) {
	if cfg.Config != nil {
		return cfg.Config, nil
	}
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, errors.New("TLS enabled but no certificates provided")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// sanitizeAuthError strips a backend ErrorResponse down to the fields a client
// legitimately needs during authentication: PostgreSQL populates Detail, Hint,
// Where, File, Line and Routine with server internals. Applies pre-auth only;
// once a session is established, query errors are relayed intact.
func sanitizeAuthError(msg *pgproto3.ErrorResponse) *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{
		Severity: msg.Severity,
		Code:     msg.Code,
		Message:  msg.Message,
	}
}

// sendFatal writes a FATAL ErrorResponse to the client. The message must be a
// fixed string, never a wrapped error: see msgBackendUnavailable.
func (ps *ProxyServer) sendFatal(conn net.Conn, code, message string) {
	errorMsg := &pgproto3.ErrorResponse{
		Severity: "FATAL",
		Code:     code,
		Message:  message,
	}
	buf, _ := errorMsg.Encode(nil)
	conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = conn.Write(buf)
	conn.SetWriteDeadline(time.Time{})
}

func (ps *ProxyServer) rejectOverCapacity(conn net.Conn) {
	defer conn.Close()
	ps.log().Warn("rejecting connection: at max connections", "client", addrString(conn.RemoteAddr()))
	errorMsg := &pgproto3.ErrorResponse{
		Severity: "FATAL",
		Code:     "53300",
		Message:  "too many connections for proxy",
	}
	buf, _ := errorMsg.Encode(nil)
	conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = conn.Write(buf)
}

func (ps *ProxyServer) handleConnection(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	backend := pgproto3.NewBackend(ps.newClientChunkReader(clientConn), clientConn)

	clientConn.SetReadDeadline(time.Now().Add(startupTimeout))
	startupMsg, err := backend.ReceiveStartupMessage()
	if err != nil {
		ps.log().Debug("failed to receive startup message", "client", addrString(clientConn.RemoteAddr()), "error", err)
		return
	}

	ps.log().Debug("startup message type", "client", addrString(clientConn.RemoteAddr()), "type", fmt.Sprintf("%T", startupMsg))

	switch msg := startupMsg.(type) {
	case *pgproto3.StartupMessage:
		clientConn.SetReadDeadline(time.Time{})
		ps.log().Debug("protocol version", "major", msg.ProtocolVersion>>16, "minor", msg.ProtocolVersion&0xFFFF)
		ps.handleStartupMessage(ctx, backend, msg, clientConn)
	case *pgproto3.SSLRequest:
		ps.handleSSLRequest(ctx, backend, clientConn)
	case *pgproto3.GSSEncRequest:
		// libpq sends GSSENCRequest before SSLRequest when gssencmode is prefer
		// (the default) and the client holds Kerberos credentials. Decline it
		// and read the client's next packet, same as the SSL path.
		if _, err := clientConn.Write([]byte{'N'}); err != nil {
			ps.log().Debug("failed to decline GSS encryption",
				"client", addrString(clientConn.RemoteAddr()), "error", err)
			return
		}
		clientConn.SetReadDeadline(time.Now().Add(startupTimeout))
		next, err := backend.ReceiveStartupMessage()
		if err != nil {
			ps.log().Debug("failed to receive startup message after GSS declined",
				"client", addrString(clientConn.RemoteAddr()), "error", err)
			return
		}
		clientConn.SetReadDeadline(time.Time{})
		switch nm := next.(type) {
		case *pgproto3.StartupMessage:
			ps.handleStartupMessage(ctx, backend, nm, clientConn)
		case *pgproto3.SSLRequest:
			ps.handleSSLRequest(ctx, backend, clientConn)
		default:
			ps.log().Debug("unexpected startup message after GSS declined",
				"client", addrString(clientConn.RemoteAddr()), "type", fmt.Sprintf("%T", nm))
		}
	default:
		ps.log().Debug("unexpected startup message type", "client", addrString(clientConn.RemoteAddr()), "type", fmt.Sprintf("%T", msg))
	}
}

// handleSSLRequest performs PostgreSQL SSL negotiation and then reads the real
// StartupMessage, over TLS if the upgrade happened.
func (ps *ProxyServer) handleSSLRequest(ctx context.Context, backend *pgproto3.Backend, clientConn net.Conn) {
	client := addrString(clientConn.RemoteAddr())

	if ps.resolvedTLS == nil {
		// TLS not configured: decline and continue in cleartext.
		if _, err := clientConn.Write([]byte{'N'}); err != nil {
			ps.log().Debug("failed to send SSL negotiation response", "client", client, "error", err)
			return
		}
		clientConn.SetReadDeadline(time.Now().Add(startupTimeout))
		startupMsg, err := backend.ReceiveStartupMessage()
		if err != nil {
			ps.log().Debug("failed to receive startup message after SSL negotiation", "client", client, "error", err)
			return
		}
		clientConn.SetReadDeadline(time.Time{})
		if sm, ok := startupMsg.(*pgproto3.StartupMessage); ok {
			ps.handleStartupMessage(ctx, backend, sm, clientConn)
		}
		return
	}

	if _, err := clientConn.Write([]byte{'S'}); err != nil {
		ps.log().Debug("failed to send SSL negotiation response", "client", client, "error", err)
		return
	}

	tlsConn := tls.Server(clientConn, ps.resolvedTLS)
	clientConn.SetDeadline(time.Now().Add(tlsHandshakeTimeout))
	if err := tlsConn.Handshake(); err != nil {
		ps.log().Debug("TLS handshake failed", "client", client, "error", err)
		return
	}
	clientConn.SetDeadline(time.Time{})
	ps.log().Debug("TLS connection established", "client", client)

	tlsBackend := pgproto3.NewBackend(ps.newClientChunkReader(tlsConn), tlsConn)
	tlsConn.SetReadDeadline(time.Now().Add(startupTimeout))
	startupMsg, err := tlsBackend.ReceiveStartupMessage()
	if err != nil {
		ps.log().Debug("failed to receive startup message after TLS", "client", client, "error", err)
		return
	}
	tlsConn.SetReadDeadline(time.Time{})

	if sm, ok := startupMsg.(*pgproto3.StartupMessage); ok {
		ps.handleStartupMessage(ctx, tlsBackend, sm, tlsConn)
	}
}

func (ps *ProxyServer) handleStartupMessage(ctx context.Context, clientBackend *pgproto3.Backend,
	startupMsg *pgproto3.StartupMessage, clientConn net.Conn,
) {
	originalUser := startupMsg.Parameters["user"]
	clientAddr := clientConn.RemoteAddr()

	// Startup parameters are attacker-supplied on a public endpoint; keep them
	// out of the default log stream.
	ps.log().Debug("startup message received", "client", addrString(clientAddr),
		"user", originalUser,
		"parameters", fmt.Sprintf("%+v", startupMsg.Parameters))

	// Route the user to get backend configuration
	backendConfig, err := ps.router.Route(ctx, originalUser)
	if err != nil {
		// Do not echo the username or routing error back: one confirms which
		// usernames exist, the other describes the topology.
		code, msg := "08001", msgBackendUnavailable
		if errors.Is(err, ErrUserNotFound) {
			code, msg = "28P01", msgNotAuthorized
		}
		ps.log().Warn("routing failed",
			"client", addrString(clientAddr), "user", originalUser, "error", err)
		ps.sendFatal(clientConn, code, msg)
		return
	}

	// Create new connection for authentication (with retries for port changes)
	addr := net.JoinHostPort(backendConfig.Host, strconv.Itoa(backendConfig.Port))
	ps.log().Debug("connecting to backend", "backend", addr, "backend_user", backendConfig.User)

	var backendConn net.Conn
	maxRetries := 3
	for attempt := range maxRetries {
		if attempt > 0 {
			// progressive wait
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
			backendConfig, err = ps.router.Route(ctx, originalUser)
			if err != nil {
				break
			}
			addr = net.JoinHostPort(backendConfig.Host, strconv.Itoa(backendConfig.Port))
			ps.log().Debug("retrying backend connection", "attempt", attempt+1, "backend", addr)
		}
		dialer := net.Dialer{Timeout: 5 * time.Second}
		backendConn, err = dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			break
		}
		ps.log().Warn("backend dial attempt failed", "attempt", attempt+1, "max_attempts", maxRetries, "error", err)
	}
	if err != nil {
		// The dial error names the backend host and port: log it, do not send it.
		ps.log().Error("backend dial failed",
			"client", addrString(clientAddr), "user", originalUser, "error", err)
		ps.sendFatal(clientConn, "08001", msgBackendUnavailable)
		return
	}
	defer backendConn.Close()

	if backendConfig.TLS != nil {
		upgraded, err := upgradeBackendToTLS(ctx, backendConn, backendConfig.TLS)
		if err != nil {
			ps.log().Error("backend TLS negotiation failed",
				"client", addrString(clientAddr), "error", err)
			ps.sendFatal(clientConn, "08001", msgBackendUnavailable)
			return
		}
		backendConn = upgraded
	}

	// Rewrite the identity parameters, keep all others.
	startupMsg.Parameters["user"] = backendConfig.User
	if backendConfig.Database != "" {
		startupMsg.Parameters["database"] = backendConfig.Database
	}

	if ps.startupRewrite != nil {
		ps.startupRewrite(clientAddr, startupMsg.Parameters)
	}

	serverFrontend := pgproto3.NewFrontend(pgproto3.NewChunkReader(backendConn), backendConn)

	buf, _ := startupMsg.Encode(nil)
	ps.log().Debug("forwarding startup message to backend",
		"client", addrString(clientAddr),
		"parameters", fmt.Sprintf("%+v", startupMsg.Parameters))
	_, err = backendConn.Write(buf)
	if err != nil {
		ps.log().Error("failed to send startup message to backend",
			"client", addrString(clientAddr), "error", err)
		ps.sendFatal(clientConn, "08001", msgBackendUnavailable)
		return
	}

	if err := ps.handleAuthentication(clientBackend, serverFrontend, clientConn, backendConn); err != nil {
		ps.log().Warn("authentication failed", "client", addrString(clientAddr), "user", originalUser, "error", err)
		return
	}

	// Debug, not Info: "connection accepted" is the single per-connection
	// record at default level.
	ps.log().Debug("authentication successful",
		"client", addrString(clientAddr), "user", originalUser, "backend_user", backendConfig.User)
	ps.proxyMessages(ctx, clientBackend, serverFrontend, clientConn, backendConn)
}

func (ps *ProxyServer) handleAuthentication(clientBackend *pgproto3.Backend, serverFrontend *pgproto3.Frontend,
	clientConn, serverConn net.Conn,
) error {
	// Set a reasonable timeout for authentication
	serverConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	clientConn.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer func() {
		serverConn.SetReadDeadline(time.Time{})
		clientConn.SetReadDeadline(time.Time{})
	}()

	for {
		msg, err := serverFrontend.Receive()
		if err != nil {
			return fmt.Errorf("failed to receive from backend: %w", err)
		}

		ps.log().Debug("auth message from backend", "type", fmt.Sprintf("%T", msg))

		var buf []byte
		switch msg := msg.(type) {
		case *pgproto3.AuthenticationOk:
			ps.log().Debug("authentication OK received")
			buf, _ = msg.Encode(nil)
		case *pgproto3.AuthenticationCleartextPassword:
			buf, _ = msg.Encode(nil)
			_, err = clientConn.Write(buf)
			if err != nil {
				return fmt.Errorf("failed to send auth request to client: %w", err)
			}

			passMsg, err := clientBackend.Receive()
			if err != nil {
				return fmt.Errorf("failed to receive password: %w", err)
			}

			if pm, ok := passMsg.(*pgproto3.PasswordMessage); ok {
				buf, _ = pm.Encode(nil)
				_, err = serverConn.Write(buf)
				if err != nil {
					return fmt.Errorf("failed to send password to server: %w", err)
				}
			}
			continue
		case *pgproto3.AuthenticationMD5Password:
			buf, _ = msg.Encode(nil)
			_, err = clientConn.Write(buf)
			if err != nil {
				return fmt.Errorf("failed to send auth request to client: %w", err)
			}

			passMsg, err := clientBackend.Receive()
			if err != nil {
				return fmt.Errorf("failed to receive password: %w", err)
			}

			if pm, ok := passMsg.(*pgproto3.PasswordMessage); ok {
				buf, _ = pm.Encode(nil)
				_, err = serverConn.Write(buf)
				if err != nil {
					return fmt.Errorf("failed to send password to server: %w", err)
				}
			}
			continue
		case *pgproto3.AuthenticationSASL:
			// SASL authentication - forward server's mechanism list to client.
			buf, _ := msg.Encode(nil)
			if _, err := clientConn.Write(buf); err != nil {
				return fmt.Errorf("failed to send SASL auth to client: %w", err)
			}

			// Receive the client's SASLInitialResponse via the framed Backend
			// rather than a raw Read. A single Read assumes one TCP segment
			// equals one protocol message; that's not guaranteed and the
			// resulting mis-framing would corrupt the proof bytes.
			if err := clientBackend.SetAuthType(pgproto3.AuthTypeSASL); err != nil {
				return fmt.Errorf("failed to set SASL auth type: %w", err)
			}
			clientMsg, err := clientBackend.Receive()
			if err != nil {
				return fmt.Errorf("failed to receive SASL initial response from client: %w", err)
			}
			initResp, ok := clientMsg.(*pgproto3.SASLInitialResponse)
			if !ok {
				return fmt.Errorf("unexpected client message during SASL init: %T", clientMsg)
			}
			buf, _ = initResp.Encode(nil)
			if _, err := serverConn.Write(buf); err != nil {
				return fmt.Errorf("failed to forward client SASL response: %w", err)
			}

			// Handle the rest of the SASL handshake.
			for {
				serverMsg, err := serverFrontend.Receive()
				if err != nil {
					return fmt.Errorf("failed to receive from server during SASL: %w", err)
				}

				var buf []byte
				switch msg := serverMsg.(type) {
				case *pgproto3.AuthenticationSASLContinue:
					buf, _ = msg.Encode(nil)
				case *pgproto3.AuthenticationSASLFinal:
					buf, _ = msg.Encode(nil)
				case *pgproto3.AuthenticationOk:
					buf, _ = msg.Encode(nil)
					clientConn.Write(buf)
					return nil
				case *pgproto3.ErrorResponse:
					ps.log().Warn("backend rejected authentication",
						"code", msg.Code, "message", msg.Message,
						"detail", msg.Detail, "where", msg.Where)
					buf, _ = sanitizeAuthError(msg).Encode(nil)
					clientConn.Write(buf)
					return fmt.Errorf("server auth error: %s", msg.Message)
				default:
					if encoder, ok := msg.(interface{ Encode([]byte) ([]byte, error) }); ok {
						buf, _ = encoder.Encode(nil)
					}
				}

				if buf != nil {
					if _, err := clientConn.Write(buf); err != nil {
						return fmt.Errorf("failed to forward server message to client: %w", err)
					}
				}

				if _, ok := serverMsg.(*pgproto3.AuthenticationSASLContinue); ok {
					if err := clientBackend.SetAuthType(pgproto3.AuthTypeSASLContinue); err != nil {
						return fmt.Errorf("failed to set SASL continue auth type: %w", err)
					}
					contMsg, err := clientBackend.Receive()
					if err != nil {
						return fmt.Errorf("failed to read SASL response from client: %w", err)
					}
					saslResp, ok := contMsg.(*pgproto3.SASLResponse)
					if !ok {
						return fmt.Errorf("unexpected client message during SASL continue: %T", contMsg)
					}
					buf, _ := saslResp.Encode(nil)
					if _, err := serverConn.Write(buf); err != nil {
						return fmt.Errorf("failed to forward client SASL response: %w", err)
					}
				}
			}
		case *pgproto3.ParameterStatus:
			buf, _ = msg.Encode(nil)
		case *pgproto3.BackendKeyData:
			buf, _ = msg.Encode(nil)
		case *pgproto3.ReadyForQuery:
			buf, _ = msg.Encode(nil)
			_, err = clientConn.Write(buf)
			if err != nil {
				return fmt.Errorf("failed to send ready to client: %w", err)
			}
			return nil
		case *pgproto3.ErrorResponse:
			ps.log().Warn("backend rejected authentication",
				"code", msg.Code, "message", msg.Message,
				"detail", msg.Detail, "where", msg.Where)
			buf, _ = sanitizeAuthError(msg).Encode(nil)
			_, err = clientConn.Write(buf)
			if err != nil {
				return fmt.Errorf("failed to send error to client: %w", err)
			}
			return fmt.Errorf("authentication error: %s", msg.Message)
		default:
			ps.log().Debug("unexpected auth message type", "type", fmt.Sprintf("%T", msg))
			continue
		}

		if buf != nil {
			_, err = clientConn.Write(buf)
			if err != nil {
				return fmt.Errorf("failed to forward auth message: %w", err)
			}
		}
	}
}

func (ps *ProxyServer) proxyMessages(ctx context.Context, clientBackend *pgproto3.Backend,
	serverFrontend *pgproto3.Frontend, clientConn, serverConn net.Conn,
) {
	errChan := make(chan error, 2)

	idle := ps.clientIdleTimeout()

	// Client to server
	go func() {
		// Always signal on exit: returning silently would leave the opposite
		// goroutine parked in Receive and the select below blocked forever,
		// leaking the connection slot.
		var err error
		defer func() { errChan <- err }()
		defer ps.recoverConn(clientConn.RemoteAddr())
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if idle > 0 {
					clientConn.SetReadDeadline(time.Now().Add(idle))
				}
				var msg pgproto3.FrontendMessage
				msg, err = clientBackend.Receive()
				if err != nil {
					err = fmt.Errorf("client receive: %w", err)
					return
				}

				var buf []byte
				switch m := msg.(type) {
				case *pgproto3.Query:
					buf, _ = m.Encode(nil)
				case *pgproto3.Parse:
					buf, _ = m.Encode(nil)
				case *pgproto3.Bind:
					buf, _ = m.Encode(nil)
				case *pgproto3.Execute:
					buf, _ = m.Encode(nil)
				case *pgproto3.Describe:
					buf, _ = m.Encode(nil)
				case *pgproto3.Sync:
					buf, _ = m.Encode(nil)
				case *pgproto3.Close:
					buf, _ = m.Encode(nil)
				case *pgproto3.Terminate:
					buf, _ = m.Encode(nil)
				case *pgproto3.CopyData:
					buf, _ = m.Encode(nil)
				case *pgproto3.CopyDone:
					buf, _ = m.Encode(nil)
				case *pgproto3.CopyFail:
					buf, _ = m.Encode(nil)
				case *pgproto3.Flush:
					buf, _ = m.Encode(nil)
				default:
					ps.log().Debug("unknown client message type", "type", fmt.Sprintf("%T", m))
					continue
				}

				if buf != nil {
					if _, err = serverConn.Write(buf); err != nil {
						err = fmt.Errorf("server send: %w", err)
						return
					}
				}
			}
		}
	}()

	// Server to client
	go func() {
		var err error
		defer func() { errChan <- err }()
		defer ps.recoverConn(serverConn.RemoteAddr())
		for {
			select {
			case <-ctx.Done():
				return
			default:
				var msg pgproto3.BackendMessage
				msg, err = serverFrontend.Receive()
				if err != nil {
					err = fmt.Errorf("server receive: %w", err)
					return
				}

				var buf []byte
				switch m := msg.(type) {
				case *pgproto3.RowDescription:
					buf, _ = m.Encode(nil)
				case *pgproto3.DataRow:
					buf, _ = m.Encode(nil)
				case *pgproto3.CommandComplete:
					buf, _ = m.Encode(nil)
				case *pgproto3.ReadyForQuery:
					buf, _ = m.Encode(nil)
				case *pgproto3.ErrorResponse:
					buf, _ = m.Encode(nil)
				case *pgproto3.NoticeResponse:
					buf, _ = m.Encode(nil)
				case *pgproto3.ParameterStatus:
					buf, _ = m.Encode(nil)
				case *pgproto3.BackendKeyData:
					buf, _ = m.Encode(nil)
				case *pgproto3.ParseComplete:
					buf, _ = m.Encode(nil)
				case *pgproto3.BindComplete:
					buf, _ = m.Encode(nil)
				case *pgproto3.NoData:
					buf, _ = m.Encode(nil)
				case *pgproto3.EmptyQueryResponse:
					buf, _ = m.Encode(nil)
				case *pgproto3.ParameterDescription:
					buf, _ = m.Encode(nil)
				case *pgproto3.CloseComplete:
					buf, _ = m.Encode(nil)
				case *pgproto3.NotificationResponse:
					buf, _ = m.Encode(nil)
				case *pgproto3.CopyInResponse:
					buf, _ = m.Encode(nil)
				case *pgproto3.CopyOutResponse:
					buf, _ = m.Encode(nil)
				case *pgproto3.CopyBothResponse:
					buf, _ = m.Encode(nil)
				case *pgproto3.CopyData:
					buf, _ = m.Encode(nil)
				case *pgproto3.CopyDone:
					buf, _ = m.Encode(nil)
				case *pgproto3.PortalSuspended:
					buf, _ = m.Encode(nil)
				default:
					ps.log().Debug("unknown server message type", "type", fmt.Sprintf("%T", m))
					continue
				}

				if buf != nil {
					if _, err = clientConn.Write(buf); err != nil {
						err = fmt.Errorf("client send: %w", err)
						return
					}
				}
			}
		}
	}()

	// Returning runs the caller's deferred Close on both connections, which
	// unblocks whichever direction is still in Receive.
	select {
	case err := <-errChan:
		switch {
		case err == nil:
			ps.log().Debug("proxy session ended")
		case isConnectionClosed(err) || errors.Is(err, io.ErrUnexpectedEOF):
			// Ordinary hang-ups: EOF, reset, or an idle-timeout deadline.
			ps.log().Debug("proxy session closed", "reason", err)
		default:
			ps.log().Warn("proxy session ended with error", "error", err)
		}
	case <-ctx.Done():
		ps.log().Debug("context cancelled, closing proxy connection")
	}
}

// upgradeBackendToTLS performs the PostgreSQL SSLRequest handshake on an
// existing backend connection and wraps it in a TLS client. Refuses to
// proceed if the backend declines SSL ('N') or returns an error ('E') —
// silent downgrade would defeat the point of using TLS here.
func upgradeBackendToTLS(ctx context.Context, conn net.Conn, cfg *tls.Config) (net.Conn, error) {
	// SSLRequest: int32 length (8) + int32 magic (80877103 = 0x04D2162F).
	sslRequest := []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer conn.SetDeadline(time.Time{})

	if _, err := conn.Write(sslRequest); err != nil {
		return nil, fmt.Errorf("send SSLRequest: %w", err)
	}

	resp := make([]byte, 1)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, fmt.Errorf("read SSLRequest response: %w", err)
	}
	switch resp[0] {
	case 'S':
		// Backend agrees to TLS; proceed.
	case 'N':
		return nil, fmt.Errorf("backend refused SSL (responded 'N'); refusing to send credentials over plaintext")
	case 'E':
		return nil, fmt.Errorf("backend returned error in response to SSLRequest")
	default:
		return nil, fmt.Errorf("unexpected SSLRequest response byte: %q", resp[0])
	}

	tlsConn := tls.Client(conn, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("backend TLS handshake: %w", err)
	}
	return tlsConn, nil
}

func isConnectionClosed(err error) bool {
	if netErr, ok := err.(*net.OpError); ok {
		return netErr.Op == "read" || netErr.Op == "write"
	}
	// Errors reach us wrapped by fmt.Errorf, and tls.Conn wraps read failures
	// in its own permanentError, so the bare type assertion above is not enough.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "read" || opErr.Op == "write"
	}
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF)
}
