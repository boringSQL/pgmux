package pgmux

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
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

	// ProxyServer is a PostgreSQL proxy server that routes connections based on username
	ProxyServer struct {
		listenAddr string
		router     Router
		pools      map[string]*ConnectionPool
		mu         sync.RWMutex
	}
)

// NewProxyServer creates a new ProxyServer with the given listen address and router
func NewProxyServer(listenAddr string, router Router) *ProxyServer {
	return &ProxyServer{
		listenAddr: listenAddr,
		router:     router,
		pools:      make(map[string]*ConnectionPool),
	}
}

// Start starts the proxy server and listens for connections
func (ps *ProxyServer) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", ps.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	defer listener.Close()

	log.Printf("PostgreSQL proxy listening on %s", ps.listenAddr)

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
				log.Printf("Failed to accept connection: %v", err)
				continue
			}
		}

		go ps.handleConnection(ctx, conn)
	}
}

func (ps *ProxyServer) handleConnection(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	backend := pgproto3.NewBackend(pgproto3.NewChunkReader(clientConn), clientConn)

	startupMsg, err := backend.ReceiveStartupMessage()
	if err != nil {
		log.Printf("Failed to receive startup message: %v", err)
		return
	}

	log.Printf("Received startup message type: %T", startupMsg)

	switch msg := startupMsg.(type) {
	case *pgproto3.StartupMessage:
		log.Printf("Protocol version: %d.%d", msg.ProtocolVersion>>16, msg.ProtocolVersion&0xFFFF)
		ps.handleStartupMessage(ctx, backend, msg, clientConn)
	case *pgproto3.SSLRequest:
		_, err := clientConn.Write([]byte{'N'})
		if err != nil {
			log.Printf("Failed to send SSL response: %v", err)
			return
		}

		startupMsg, err := backend.ReceiveStartupMessage()
		if err != nil {
			log.Printf("Failed to receive startup message after SSL: %v", err)
			return
		}

		if sm, ok := startupMsg.(*pgproto3.StartupMessage); ok {
			ps.handleStartupMessage(ctx, backend, sm, clientConn)
		}
	default:
		log.Printf("Unexpected startup message type: %T", msg)
	}
}

func (ps *ProxyServer) handleStartupMessage(ctx context.Context, clientBackend *pgproto3.Backend,
	startupMsg *pgproto3.StartupMessage, clientConn net.Conn,
) {
	originalUser := startupMsg.Parameters["user"]
	log.Printf("New connection for user: %s", originalUser)
	log.Printf("Startup parameters: %+v", startupMsg.Parameters)

	// Route the user to get backend configuration
	backendConfig, err := ps.router.Route(ctx, originalUser)
	if err != nil {
		var errorMsg *pgproto3.ErrorResponse
		if err == ErrUserNotFound {
			errorMsg = &pgproto3.ErrorResponse{
				Severity: "FATAL",
				Code:     "28P01",
				Message:  fmt.Sprintf("User mapping not found for: %s", originalUser),
			}
		} else {
			errorMsg = &pgproto3.ErrorResponse{
				Severity: "FATAL",
				Code:     "08001",
				Message:  fmt.Sprintf("Routing error: %v", err),
			}
		}
		buf, _ := errorMsg.Encode(nil)
		clientConn.Write(buf)
		return
	}

	// Create new connection for authentication
	addr := net.JoinHostPort(backendConfig.Host, strconv.Itoa(backendConfig.Port))
	log.Printf("Connecting to backend %s as user %s", addr, backendConfig.User)

	backendConn, err := net.Dial("tcp", addr)
	if err != nil {
		errorMsg := &pgproto3.ErrorResponse{
			Severity: "FATAL",
			Code:     "08001",
			Message:  fmt.Sprintf("Could not connect to backend: %v", err),
		}
		buf, _ := errorMsg.Encode(nil)
		clientConn.Write(buf)
		return
	}
	defer backendConn.Close()

	// Modify only the user parameter, keep all others
	startupMsg.Parameters["user"] = backendConfig.User

	serverFrontend := pgproto3.NewFrontend(pgproto3.NewChunkReader(backendConn), backendConn)

	buf, _ := startupMsg.Encode(nil)
	log.Printf("Sending startup message to backend with parameters: %+v", startupMsg.Parameters)
	_, err = backendConn.Write(buf)
	if err != nil {
		log.Printf("Failed to send startup message to backend: %v", err)
		return
	}

	if err := ps.handleAuthentication(clientBackend, serverFrontend, clientConn, backendConn); err != nil {
		log.Printf("Authentication failed: %v", err)
		return
	}

	log.Printf("Authentication successful for user %s", originalUser)
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

		log.Printf("Received auth message from backend: %T", msg)

		var buf []byte
		switch msg := msg.(type) {
		case *pgproto3.AuthenticationOk:
			log.Printf("Authentication OK received")
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
			// SASL authentication - forward to client
			log.Printf("SASL authentication requested, mechanisms: %v", msg.AuthMechanisms)

			buf, _ := msg.Encode(nil)
			log.Printf("Sending SASL auth to client, message length: %d bytes", len(buf))

			n, err := clientConn.Write(buf)
			if err != nil {
				return fmt.Errorf("failed to send SASL auth to client: %w", err)
			}
			log.Printf("Wrote %d bytes to client", n)

			// Get SASL initial response from client
			log.Printf("Waiting for SASL response from client...")

			clientConn.SetReadDeadline(time.Now().Add(10 * time.Second))
			rawBuf := make([]byte, 1024)
			n, err = clientConn.Read(rawBuf)
			clientConn.SetReadDeadline(time.Time{})

			if err != nil {
				return fmt.Errorf("failed to read from client: %w", err)
			}

			log.Printf("Raw message from client (%d bytes): %x", n, rawBuf[:n])

			// Forward the client's SASL initial response to backend
			log.Printf("Forwarding client SASL response to backend server")

			_, err = serverConn.Write(rawBuf[:n])
			if err != nil {
				return fmt.Errorf("failed to forward client response: %w", err)
			}

			// Handle the rest of the SASL handshake
			for {
				// Read response from server
				serverMsg, err := serverFrontend.Receive()
				if err != nil {
					return fmt.Errorf("failed to receive from server during SASL: %w", err)
				}

				log.Printf("Received from server during SASL: %T", serverMsg)

				// Forward to client
				var buf []byte
				switch msg := serverMsg.(type) {
				case *pgproto3.AuthenticationSASLContinue:
					buf, _ = msg.Encode(nil)
				case *pgproto3.AuthenticationSASLFinal:
					buf, _ = msg.Encode(nil)
				case *pgproto3.AuthenticationOk:
					buf, _ = msg.Encode(nil)
					clientConn.Write(buf)
					log.Printf("SASL authentication completed successfully")
					return nil // Auth complete, exit this function
				case *pgproto3.ErrorResponse:
					buf, _ = msg.Encode(nil)
					clientConn.Write(buf)
					return fmt.Errorf("server auth error: %s", msg.Message)
				default:
					// Forward any other message types
					if encoder, ok := msg.(interface{ Encode([]byte) ([]byte, error) }); ok {
						buf, _ = encoder.Encode(nil)
					}
				}

				if buf != nil {
					_, err = clientConn.Write(buf)
					if err != nil {
						return fmt.Errorf("failed to forward server message to client: %w", err)
					}
				}

				// If it was SASL Continue, read client's response
				if _, ok := serverMsg.(*pgproto3.AuthenticationSASLContinue); ok {
					// Read client's SASL response
					clientBuf := make([]byte, 4096)
					n, err := clientConn.Read(clientBuf)
					if err != nil {
						return fmt.Errorf("failed to read SASL response from client: %w", err)
					}

					log.Printf("Forwarding client SASL continue response (%d bytes) to server", n)

					// Forward to server
					_, err = serverConn.Write(clientBuf[:n])
					if err != nil {
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
			buf, _ = msg.Encode(nil)
			_, err = clientConn.Write(buf)
			if err != nil {
				return fmt.Errorf("failed to send error to client: %w", err)
			}
			return fmt.Errorf("authentication error: %s", msg.Message)
		default:
			log.Printf("Unexpected auth message type: %T", msg)
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

	// Client to server
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				msg, err := clientBackend.Receive()
				if err != nil {
					if err != io.EOF && !isConnectionClosed(err) {
						errChan <- fmt.Errorf("client receive: %w", err)
					}
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
					log.Printf("Unknown client message type: %T", m)
					continue
				}

				if buf != nil {
					_, err = serverConn.Write(buf)
					if err != nil {
						errChan <- fmt.Errorf("server send: %w", err)
						return
					}
				}
			}
		}
	}()

	// Server to client
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				msg, err := serverFrontend.Receive()
				if err != nil {
					if err != io.EOF && !isConnectionClosed(err) {
						errChan <- fmt.Errorf("server receive: %w", err)
					}
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
					log.Printf("Unknown server message type: %T", m)
					continue
				}

				if buf != nil {
					_, err = clientConn.Write(buf)
					if err != nil {
						errChan <- fmt.Errorf("client send: %w", err)
						return
					}
				}
			}
		}
	}()

	select {
	case err := <-errChan:
		if err != nil {
			log.Printf("Proxy error: %v", err)
		}
	case <-ctx.Done():
		log.Println("Context cancelled, closing proxy connection")
	}
}

func isConnectionClosed(err error) bool {
	if netErr, ok := err.(*net.OpError); ok {
		return netErr.Op == "read" || netErr.Op == "write"
	}
	return false
}
