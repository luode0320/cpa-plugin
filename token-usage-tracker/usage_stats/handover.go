package usagestats

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	// Probe briefly first so normal startup and a clean shutdown do not create an
	// unnecessary handover request. A longer retry follows only when bbolt reports
	// that another plugin instance still owns its exclusive file lock.
	storeOpenProbeTimeout    = 100 * time.Millisecond
	storeOpenHandoverTimeout = 5 * time.Second
	storeLeasePollInterval   = 50 * time.Millisecond
)

type storeLease struct {
	path  string
	token string
	pid   int
}

func openStoreDatabase(path string) (*bolt.DB, *storeLease, error) {
	lease, err := newStoreLease(path)
	if err != nil {
		return nil, nil, err
	}

	// Most opens happen at process startup or after an orderly shutdown and can
	// acquire the database immediately. Do not disturb a live owner unless the
	// bbolt lock probe actually times out.
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: storeOpenProbeTimeout})
	if err == nil {
		if err = lease.claim(); err != nil {
			_ = db.Close()
			return nil, nil, err
		}
		return db, lease, nil
	}
	if !errors.Is(err, bolt.ErrTimeout) {
		return nil, nil, err
	}

	// CLIProxyAPI loads and registers a versioned replacement while the previous
	// shared library remains active. The old plugin therefore still owns bbolt's exclusive
	// lock while the new plugin is registering. Publishing a new lease token asks
	// a handover-aware old instance to flush, close, and release that lock.
	if err = lease.claim(); err != nil {
		return nil, nil, err
	}
	db, err = bolt.Open(path, 0o600, &bolt.Options{Timeout: storeOpenHandoverTimeout})
	if err != nil {
		lease.release()
		return nil, nil, fmt.Errorf("hot-reload handover timed out; restart CLIProxyAPI or reinstall once when upgrading from a legacy plugin: %w", err)
	}
	if !lease.current() {
		_ = db.Close()
		return nil, nil, fmt.Errorf("database handover was superseded by another plugin instance")
	}
	return db, lease, nil
}

func newStoreLease(databasePath string) (*storeLease, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, fmt.Errorf("create database handover token: %w", err)
	}
	pid := os.Getpid()
	return &storeLease{
		path:  databasePath + ".handover",
		token: fmt.Sprintf("%d-%s", pid, hex.EncodeToString(random[:])),
		pid:   pid,
	}, nil
}

func (l *storeLease) claim() error {
	if l == nil {
		return errors.New("database handover lease is nil")
	}
	if err := os.WriteFile(l.path, []byte(l.token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write database handover marker: %w", err)
	}
	return nil
}

func (l *storeLease) current() bool {
	if l == nil {
		return false
	}
	token, err := readStoreLeaseToken(l.path)
	return err == nil && token == l.token
}

func (l *storeLease) release() {
	if l == nil || !l.current() {
		return
	}
	_ = os.Remove(l.path)
}

func readStoreLeaseToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func storeLeaseTokenPID(token string) (int, bool) {
	rawPID, _, ok := strings.Cut(token, "-")
	if !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(rawPID)
	return pid, err == nil && pid > 0
}

func (l *storeLease) monitor(store *Store) {
	if l == nil || store == nil {
		return
	}
	ticker := time.NewTicker(storeLeasePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-store.done:
			return
		case <-ticker.C:
			token, err := readStoreLeaseToken(l.path)
			if err != nil || token == "" || token == l.token {
				continue
			}
			claimPID, ok := storeLeaseTokenPID(token)
			if !ok || claimPID != l.pid {
				// Only instances loaded by this CLIProxyAPI process participate in
				// hot-reload handover. A different process must still respect
				// bbolt's normal exclusive-lock timeout.
				continue
			}
			// A newer plugin instance is waiting for the database. Close through
			// the normal actor path so pending records are flushed before bbolt's
			// file lock is released.
			_ = store.Close()
			return
		}
	}
}
