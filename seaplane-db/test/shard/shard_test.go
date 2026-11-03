package shard_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/internal/reader"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-db/test/testinfra"
	"github.com/stretchr/testify/require"
)

var sharedDB *testinfra.TestDB

func TestMain(m *testing.M) {
	sharedDB = testinfra.StartPostgresForTestMain()
	code := m.Run()
	sharedDB.Stop()
	os.Exit(code)
}

// connectManual creates a connection NOT managed by t.Cleanup — the caller
// must close it manually after the watcher goroutine has exited.
func connectManual(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), sharedDB.ConnStr)
	require.NoError(t, err)
	return conn
}

// freshConn creates a connection managed by t.Cleanup.
func freshConn(t *testing.T) *pgx.Conn {
	t.Helper()
	return sharedDB.Connect(t)
}

func truncateAll(t *testing.T) {
	t.Helper()
	conn := freshConn(t)
	sharedDB.TruncateAll(t, conn)
}

// runWatcher starts the watcher in a goroutine and returns a done channel.
func runWatcher(w *reader.Watcher, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()
	return done
}

// hashtextResidue returns the shard residue for a namespace using the same
// formula as the SQL shard clause: abs(hashtext(ns)::bigint) % mod.
func hashtextResidue(t *testing.T, conn *pgx.Conn, ns string, mod int) int {
	t.Helper()
	var residue int
	err := conn.QueryRow(context.Background(),
		"SELECT abs(hashtext($1)::bigint) % $2", ns, mod).Scan(&residue)
	require.NoError(t, err)
	return residue
}

// findNamespacesForShards returns a map from each residue in [0, mod) to a
// namespace string that hashes to that residue. Generates random UUID-like
// strings until all residues are covered.
func findNamespacesForShards(t *testing.T, conn *pgx.Conn, mod int) map[int]string {
	t.Helper()
	result := make(map[int]string, mod)
	// Use a deterministic naming scheme and check residues until all shards covered.
	for i := 0; len(result) < mod; i++ {
		ns := fmt.Sprintf("shard-probe-%d", i)
		r := hashtextResidue(t, conn, ns, mod)
		if _, exists := result[r]; !exists {
			result[r] = ns
		}
		if i > 10000 {
			t.Fatalf("could not find namespaces for all %d shards after %d tries", mod, i)
		}
	}
	return result
}
