// Package devpg starts a throwaway PostgreSQL server for local development,
// tests, the demo and the benchmark, so none of them need Docker or a system
// Postgres install.
//
// It is never imported by the production binary (cmd/omsd), which only
// accepts an explicit OMS_DATABASE_URL.
//
// The server binaries are downloaded once from Maven Central (the
// zonkyio/embedded-postgres-binaries distribution) and cached under
// <module root>/.cache/devpg, or $OMS_DEVPG_DIR when set.
package devpg

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// Version is the PostgreSQL release every local run and test uses. Keep it in
// step with the image tag in docker-compose.yml and the CI service container.
const Version = embeddedpostgres.V17

// Options tunes the throwaway server.
type Options struct {
	// Durable keeps fsync and synchronous_commit at their PostgreSQL defaults.
	// Tests leave it false for speed; the benchmark sets it so that reported
	// latencies include real commit durability.
	Durable bool
	// Log receives the server log. Nil discards it.
	Log io.Writer
}

// Instance is a running throwaway server.
type Instance struct {
	// URL is a connection string for the superuser on the default database.
	URL string

	pg     *embeddedpostgres.EmbeddedPostgres
	runDir string
}

// Start downloads the server binaries if needed, initialises an empty cluster
// in a private directory and starts it on a free loopback port.
func Start(opts Options) (*Instance, error) {
	root, err := cacheRoot()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("devpg: create cache dir: %w", err)
	}

	// Several test binaries may start at once. Downloading and unpacking the
	// shared binaries is not safe to do concurrently, so serialise startup
	// across processes.
	unlock, err := lockFile(filepath.Join(root, "start.lock"))
	if err != nil {
		return nil, fmt.Errorf("devpg: lock: %w", err)
	}
	defer unlock()

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	runDir := filepath.Join(root, "run", fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(suffix)))

	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("devpg: find free port: %w", err)
	}

	params := map[string]string{
		"max_connections": "200",
		// TCP on loopback only. Unix sockets are disabled because the socket
		// path would sit under the run directory and can exceed the OS limit.
		"listen_addresses":        "127.0.0.1",
		"unix_socket_directories": "",
	}
	if !opts.Durable {
		params["fsync"] = "off"
		params["synchronous_commit"] = "off"
		params["full_page_writes"] = "off"
	} else if runtime.GOOS == "darwin" {
		// On macOS a plain fsync does not make the drive flush its cache, so
		// PostgreSQL's default there acknowledges commits that a power loss
		// could still lose. Ask for the real thing (F_FULLFSYNC), or the
		// benchmark would report latencies for durability it does not have.
		params["wal_sync_method"] = "fsync_writethrough"
	}

	logw := opts.Log
	if logw == nil {
		logw = io.Discard
	}

	cfg := embeddedpostgres.DefaultConfig().
		Version(Version).
		Port(uint32(port)).
		Username("oms").
		Password("oms").
		Database("oms").
		CachePath(filepath.Join(root, "archive")).
		BinariesPath(filepath.Join(root, "bin-"+string(Version))).
		RuntimePath(runDir).
		DataPath(filepath.Join(runDir, "data")).
		StartParameters(params).
		StartTimeout(60 * time.Second).
		Logger(logw)

	pg := embeddedpostgres.NewDatabase(cfg)
	if err := pg.Start(); err != nil {
		_ = os.RemoveAll(runDir)
		return nil, fmt.Errorf("devpg: start: %w", err)
	}
	return &Instance{
		URL:    fmt.Sprintf("postgres://oms:oms@127.0.0.1:%d/oms?sslmode=disable", port),
		pg:     pg,
		runDir: runDir,
	}, nil
}

// Durability describes what a commit means on a server started with the
// given options, for reports.
func Durability(opts Options) string {
	switch {
	case !opts.Durable:
		return "OFF (fsync=off, synchronous_commit=off): commits are not durable"
	case runtime.GOOS == "darwin":
		return "ON (fsync=on, synchronous_commit=on, wal_sync_method=fsync_writethrough)"
	}
	return "ON (fsync=on, synchronous_commit=on, default wal_sync_method)"
}

// Stop shuts the server down and deletes its data directory.
func (i *Instance) Stop() error {
	if i == nil || i.pg == nil {
		return nil
	}
	err := i.pg.Stop()
	i.pg = nil
	return errors.Join(err, os.RemoveAll(i.runDir))
}

func cacheRoot() (string, error) {
	if dir := os.Getenv("OMS_DEVPG_DIR"); dir != "" {
		return filepath.Abs(dir)
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, ".cache", "devpg"), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Join(os.TempDir(), "oms-devpg"), nil
		}
		dir = parent
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
