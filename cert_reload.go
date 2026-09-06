package pgmux

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certReloader serves the keypair from disk, reloading it when either file
// changes. Certificates rotate on their own schedule — a 60-day Let's Encrypt
// renewal should not need a restart to take effect.
type certReloader struct {
	certFile string
	keyFile  string
	logger   *slog.Logger

	// loadMu serializes reloads so a burst of handshakes arriving on one
	// renewal performs a single read and logs a single line.
	loadMu sync.Mutex

	mu      sync.RWMutex
	cert    *tls.Certificate
	certSig fileSig
	keySig  fileSig
	// failedCert/failedKey record the version that last failed to load, so a
	// file that stays broken is reported once rather than on every handshake.
	failed     bool
	failedCert fileSig
	failedKey  fileSig
}

// fileSig is the cheap change signal: mtime plus size, two stats per handshake.
type fileSig struct {
	mod  time.Time
	size int64
}

func statSig(path string) (fileSig, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileSig{}, err
	}
	return fileSig{mod: fi.ModTime(), size: fi.Size()}, nil
}

// newCertReloader loads the keypair once, so a bad path fails at startup.
func newCertReloader(certFile, keyFile string, logger *slog.Logger) (*certReloader, error) {
	// Stat before loading: a signature taken afterwards could record a file
	// that changed mid-load and skip the next reload. Stat errors need no
	// handling — the load reports them, and a zero signature only costs one
	// redundant reload.
	certSig, _ := statSig(certFile)
	keySig, _ := statSig(keyFile)

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}
	return &certReloader{
		certFile: certFile,
		keyFile:  keyFile,
		logger:   logger,
		cert:     &cert,
		certSig:  certSig,
		keySig:   keySig,
	}, nil
}

// current reports the loaded certificate and whether the files on disk are a
// version this reloader has not already handled.
func (r *certReloader) current() (*tls.Certificate, bool) {
	certSig, certErr := statSig(r.certFile)
	keySig, keyErr := statSig(r.keyFile)

	r.mu.RLock()
	defer r.mu.RUnlock()

	switch {
	case certErr != nil || keyErr != nil:
		// Unreadable now: keep serving what we have rather than failing
		// handshakes while a renewal tool swaps files around.
		return r.cert, false
	case certSig == r.certSig && keySig == r.keySig:
		return r.cert, false
	case r.failed && certSig == r.failedCert && keySig == r.failedKey:
		// The same broken version we already reported.
		return r.cert, false
	}
	return r.cert, true
}

// GetCertificate reloads on change and serves the previous certificate if the
// new one will not load — a half-written file must not take the endpoint down.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert, changed := r.current()
	if !changed {
		return cert, nil
	}
	return r.reload(), nil
}

// reload loads and publishes the keypair, logging what it did, and returns the
// certificate to serve. Errors are absorbed: the previous certificate is kept.
func (r *certReloader) reload() *tls.Certificate {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()

	certSig, _ := statSig(r.certFile)
	keySig, _ := statSig(r.keyFile)

	r.mu.RLock()
	cert := r.cert
	handled := (certSig == r.certSig && keySig == r.keySig) ||
		(r.failed && certSig == r.failedCert && keySig == r.failedKey)
	r.mu.RUnlock()

	// Another handshake reloaded, or already reported, this same version.
	if handled {
		return cert
	}

	loaded, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		r.mu.Lock()
		r.failed, r.failedCert, r.failedKey = true, certSig, keySig
		r.mu.Unlock()

		// Logged once per distinct broken version: an unauthenticated client
		// must not be able to drive log volume by reconnecting.
		r.log().Error("TLS certificate reload failed, serving the previous one",
			"cert", r.certFile, "error", err)
		return cert
	}

	r.mu.Lock()
	r.cert, r.certSig, r.keySig, r.failed = &loaded, certSig, keySig, false
	r.mu.Unlock()

	r.log().Info("TLS certificate reloaded", "cert", r.certFile)
	return &loaded
}

func (r *certReloader) log() *slog.Logger {
	if r.logger != nil {
		return r.logger
	}
	return slog.Default()
}
