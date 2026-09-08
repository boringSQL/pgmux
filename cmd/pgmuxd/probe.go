package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgproto3/v2"
)

// backendSettings are the connection ceilings the backend is configured with.
type backendSettings struct {
	MaxConnections     int
	SuperuserReserved  int
	ReservedConnection int
}

// Available reports how many connections are left for ordinary clients.
func (s backendSettings) Available() int {
	return s.MaxConnections - s.SuperuserReserved - s.ReservedConnection
}

const settingsQuery = `select current_setting('max_connections'), ` +
	`current_setting('superuser_reserved_connections'), ` +
	`coalesce(current_setting('reserved_connections', true), '0')`

// probe opens a connection to the backend exactly as the proxy would and, when
// asked, reads the connection settings.
//
// This speaks the protocol directly rather than using a SQL driver: it is the
// same startup message a visitor's session sends, so it exercises pg_hba for
// the guest role — the thing that actually breaks — and it keeps the module's
// single dependency single.
func probe(ctx context.Context, cfg *Config, withSettings bool) (backendSettings, error) {
	addr := net.JoinHostPort(cfg.BackendHost, strconv.Itoa(cfg.BackendPort))
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return backendSettings{}, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	defer terminate(conn)

	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	startup := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters: map[string]string{
			"user":             cfg.BackendUser,
			"database":         cfg.BackendDatabase,
			"application_name": "pgmuxd_healthcheck",
		},
	}
	buf, _ := startup.Encode(nil)
	if _, err := conn.Write(buf); err != nil {
		return backendSettings{}, fmt.Errorf("send startup: %w", err)
	}

	fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
	if err := awaitReady(fe); err != nil {
		return backendSettings{}, err
	}
	if !withSettings {
		return backendSettings{}, nil
	}

	q, _ := (&pgproto3.Query{String: settingsQuery}).Encode(nil)
	if _, err := conn.Write(q); err != nil {
		return backendSettings{}, fmt.Errorf("send settings query: %w", err)
	}
	return readSettings(fe)
}

// awaitReady consumes the startup exchange up to ReadyForQuery. The backend is
// expected to be trust-authenticated; anything else is reported rather than
// answered, since the proxy could not complete it either.
func awaitReady(fe *pgproto3.Frontend) error {
	for {
		msg, err := fe.Receive()
		if err != nil {
			return fmt.Errorf("startup: %w", err)
		}
		switch m := msg.(type) {
		case *pgproto3.ReadyForQuery:
			return nil
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("backend refused the health connection: %s (%s)", m.Message, m.Code)
		case *pgproto3.AuthenticationOk, *pgproto3.ParameterStatus, *pgproto3.BackendKeyData,
			*pgproto3.NoticeResponse:
			// Expected on the way to ReadyForQuery.
		default:
			// Includes AuthenticationSASL and friends: the probe is not going
			// to authenticate, and neither could the proxy for this role.
			return fmt.Errorf("unexpected message during startup (%T)", m)
		}
	}
}

func readSettings(fe *pgproto3.Frontend) (backendSettings, error) {
	var settings backendSettings
	var got bool
	for {
		msg, err := fe.Receive()
		if err != nil {
			return settings, fmt.Errorf("settings query: %w", err)
		}
		switch m := msg.(type) {
		case *pgproto3.DataRow:
			if len(m.Values) < 3 {
				return settings, errors.New("settings query returned too few columns")
			}
			settings.MaxConnections = atoiOrZero(m.Values[0])
			settings.SuperuserReserved = atoiOrZero(m.Values[1])
			settings.ReservedConnection = atoiOrZero(m.Values[2])
			got = true
		case *pgproto3.ReadyForQuery:
			if !got {
				return settings, errors.New("settings query returned no rows")
			}
			return settings, nil
		case *pgproto3.ErrorResponse:
			return settings, fmt.Errorf("settings query: %s (%s)", m.Message, m.Code)
		}
	}
}

func atoiOrZero(v []byte) int {
	n, err := strconv.Atoi(string(v))
	if err != nil {
		return 0
	}
	return n
}

func terminate(conn net.Conn) {
	buf, _ := (&pgproto3.Terminate{}).Encode(nil)
	conn.SetWriteDeadline(time.Now().Add(time.Second))
	conn.Write(buf)
}
