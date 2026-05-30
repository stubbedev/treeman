package mongo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/dumpload"
)

// Restore loads a `mongodump --archive` archive into `targetDB`. It
// picks the fastest available path:
//
//  1. `<container-engine> exec -i CID mongorestore …` when the
//     connection's ContainerRef resolves to a running container.
//  2. Host `mongorestore` CLI on PATH, piping the archive over stdin.
//  3. Pure-Go wire-protocol fallback that parses the BSON archive
//     stream itself and inserts every doc via the official mongo Go
//     driver. Always available — selected when neither CLI path is.
//
// `sourceDB` names the DB the archive was made from (`mongodump
// --db=`). All three strategies remap it onto `targetDB`. Pass
// sourceDB="" to skip the rename (the archive must already use the
// target name).
//
// Returns the strategy that actually ran so callers can log it. Fast-
// path FAILURES (CLI ran but exited non-zero) are downgraded to a
// warn log + fall-through, so a dev box missing mongorestore can
// never block a cold build.
func Restore(ctx context.Context, conn *config.MongoConn, targetDB, sourceDB, dumpPath string) (dumpload.LoadStrategy, error) {
	if ok, err := tryDockerExecMongoRestore(ctx, conn, targetDB, sourceDB, dumpPath); ok {
		return dumpload.StrategyDockerExec, nil
	} else if err != nil {
		slog.Warn("mongo restore fast path (docker exec) failed; falling through", "error", err)
	}
	if ok, err := tryNativeCLIMongoRestore(ctx, conn, targetDB, sourceDB, dumpPath); ok {
		return dumpload.StrategyNativeCLI, nil
	} else if err != nil {
		slog.Warn("mongo restore fast path (native CLI) failed; falling through", "error", err)
	}
	return dumpload.StrategyWire, restoreViaDriver(ctx, conn, targetDB, sourceDB, dumpPath)
}

// tryDockerExecMongoRestore runs mongorestore INSIDE the engine
// container via `<engine> exec -i`. Returns (true, nil) on success,
// (false, nil) when the container ref isn't set / not resolvable, and
// (false, err) when exec ran but failed.
func tryDockerExecMongoRestore(ctx context.Context, conn *config.MongoConn, targetDB, sourceDB, dumpPath string) (bool, error) {
	if conn == nil || (conn.Container == "" && conn.ComposeService == "") {
		return false, nil
	}
	opts := containerip.Opts{
		Container:      conn.Container,
		ComposeService: conn.ComposeService,
		ComposeProject: conn.ComposeProject,
		Engine:         conn.ContainerEngine,
		Network:        conn.Network,
	}
	cid, cerr := containerip.ContainerID(opts)
	if cerr != nil {
		return false, nil
	}
	engineBin := opts.Engine
	if engineBin == "" {
		engineBin = "docker"
	}
	if _, err := exec.LookPath(engineBin); err != nil {
		return false, nil
	}
	args := []string{
		"exec", "-i", cid, "mongorestore",
		"--archive", "--drop", "--quiet", "--uri=" + conn.URI,
	}
	if sourceDB != "" && sourceDB != targetDB {
		args = append(args, "--nsFrom="+sourceDB+".*", "--nsTo="+targetDB+".*")
	}
	rc, format, oerr := dumpload.OpenDump(dumpPath)
	if oerr != nil {
		return false, oerr
	}
	defer func() { _ = rc.Close() }()
	if format == dumpload.FormatGzip {
		args = append(args, "--gzip")
	}
	cmd := exec.CommandContext(ctx, engineBin, args...)
	cmd.Stdin = rc
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("%s exec mongorestore: %w (stderr: %s)", engineBin, err, stderr.String())
	}
	return true, nil
}

// tryNativeCLIMongoRestore runs the host's `mongorestore` CLI when
// it's on PATH.
func tryNativeCLIMongoRestore(ctx context.Context, conn *config.MongoConn, targetDB, sourceDB, dumpPath string) (bool, error) {
	if conn == nil {
		return false, nil
	}
	if _, err := exec.LookPath("mongorestore"); err != nil {
		return false, nil
	}
	args := []string{"--uri=" + conn.URI, "--drop", "--quiet"}
	if sourceDB != "" && sourceDB != targetDB {
		args = append(args, "--nsFrom="+sourceDB+".*", "--nsTo="+targetDB+".*")
	}
	rc, format, err := dumpload.OpenDump(dumpPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = rc.Close() }()
	var cmd *exec.Cmd
	switch format {
	case dumpload.FormatNone:
		cmd = exec.CommandContext(ctx, "mongorestore", append(args, "--archive="+dumpPath)...)
	case dumpload.FormatGzip:
		// mongorestore handles gzip natively — let it do the work.
		cmd = exec.CommandContext(ctx, "mongorestore", append(args, "--archive="+dumpPath, "--gzip")...)
	default:
		// zstd/bzip2/xz: decompress in Go, pipe via stdin.
		cmd = exec.CommandContext(ctx, "mongorestore", append(args, "--archive")...)
		cmd.Stdin = rc
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if rerr := cmd.Run(); rerr != nil {
		return false, fmt.Errorf("mongorestore (%s): %w: %s", format, rerr, stderr.String())
	}
	return true, nil
}

// restoreViaDriver is the pure-Go wire-protocol fallback. It parses
// the `mongodump --archive` stream itself and inserts every doc via
// the official mongo Go driver, which speaks the same BSON wire
// protocol the CLI does. Compression is auto-detected via
// dumpload.OpenDump.
//
// Limitations of this fallback (documented + tolerated, NOT silent):
//   - Indexes encoded in the prelude's collection metadata are NOT
//     replayed. Most dev workflows rebuild them via the migrate step
//     after the dump applies, so this rarely surfaces. We log a
//     `wire fallback used: indexes not replayed` warning so the
//     operator can decide whether to install mongorestore.
//   - Per-doc inserts (not bulk-write batching). Adequate for the
//     small seed dumps treeman typically handles; could be batched
//     later if measurement shows it's worth the complexity.
func restoreViaDriver(ctx context.Context, conn *config.MongoConn, targetDB, sourceDB, dumpPath string) error {
	if conn == nil || conn.URI == "" {
		return errors.New("mongo wire restore: missing connection URI")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(conn.URI))
	if err != nil {
		return fmt.Errorf("mongo connect for wire restore: %w", err)
	}
	defer func() { _ = client.Disconnect(ctx) }()

	rc, _, err := dumpload.OpenDump(dumpPath)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	r := &archiveReader{r: rc}
	if err := r.readHeader(); err != nil {
		return fmt.Errorf("read archive header: %w", err)
	}

	dropped := map[string]bool{}
	for {
		ns, doc, terminate, rerr := r.next()
		if rerr != nil {
			return rerr
		}
		if terminate {
			break
		}
		dbName, coll := splitNamespace(ns)
		if dbName == "" || coll == "" {
			continue
		}
		if sourceDB != "" && dbName == sourceDB {
			dbName = targetDB
		}
		// Drop-then-restore mirrors `mongorestore --drop`. We drop the
		// destination collection on first encounter so a partially-
		// loaded run is replaced wholesale, never doubled.
		key := dbName + "." + coll
		if !dropped[key] {
			_ = client.Database(dbName).Collection(coll).Drop(ctx)
			dropped[key] = true
		}
		if _, err := client.Database(dbName).Collection(coll).InsertOne(ctx, doc); err != nil {
			return fmt.Errorf("insert into %s.%s: %w", dbName, coll, err)
		}
	}

	slog.Warn("mongo restore: wire-protocol fallback used; indexes not replayed",
		"path", dumpPath, "target_db", targetDB,
		"hint", "install mongorestore on PATH or set ContainerRef on connections.mongodb for the fast path")
	return nil
}

// archiveMagic is the first four bytes every `mongodump --archive`
// stream begins with.
const archiveMagic uint32 = 0x8199e26d

// archiveTerminator marks the end of a namespace block or the end of
// the archive (two terminators in a row).
const archiveTerminator uint32 = 0xFFFFFFFF

// archiveReader walks a mongodump archive stream:
//
//	[4-byte magic][prelude BSON][ block ... ]*[terminator][terminator]
//
// where each block is:
//
//	[namespace header BSON][document BSON]*[terminator]
//
// The header BSON carries `ns` (and metadata we don't currently
// replay); subsequent BSON docs (until the terminator) belong to that
// namespace.
type archiveReader struct {
	r       io.Reader
	curNs   string
	atEnd   bool
	inBlock bool
}

func (a *archiveReader) readHeader() error {
	var magic uint32
	if err := binary.Read(a.r, binary.LittleEndian, &magic); err != nil {
		return err
	}
	if magic != archiveMagic {
		return fmt.Errorf("not a mongodump archive (magic %#x; want %#x)", magic, archiveMagic)
	}
	// Prelude wire format:
	//
	//   [Header BSON: {concurrent_collections, server_version, ...}]
	//   [CollectionMetadata BSON for ns1]
	//   [CollectionMetadata BSON for ns2]
	//   ...
	//   [TERMINATOR (0xFFFFFFFF)]
	//
	// We don't currently parse the metadata (index replay is a TODO);
	// just consume every doc up to the terminator so the body-stream
	// parser sees the first NamespaceHeader exactly where it should.
	// Reading one BSON doc and stopping (the old bug) misclassified
	// subsequent CollectionMetadata BSONs as namespace headers and
	// body documents, doubling rows for archives with >1 collection.
	if _, err := readBSONDoc(a.r); err != nil {
		return fmt.Errorf("prelude header doc: %w", err)
	}
	for {
		var head [4]byte
		if _, err := io.ReadFull(a.r, head[:]); err != nil {
			return fmt.Errorf("prelude metadata: %w", err)
		}
		sz := binary.LittleEndian.Uint32(head[:])
		if sz == archiveTerminator {
			return nil
		}
		if sz < 5 {
			return fmt.Errorf("invalid prelude BSON length %d", sz)
		}
		body := make([]byte, sz-4)
		if _, err := io.ReadFull(a.r, body); err != nil {
			return fmt.Errorf("prelude metadata body: %w", err)
		}
		// metadata is discarded; future work: parse for index defs.
	}
}

// next returns the next document in the archive along with its
// fully-qualified namespace, or terminate=true when the archive ends.
func (a *archiveReader) next() (ns string, doc bson.Raw, terminate bool, err error) {
	if a.atEnd {
		return "", nil, true, nil
	}
	for {
		// Peek the next 4 bytes: either a terminator (0xFFFFFFFF) or
		// the length prefix of a BSON document.
		var head [4]byte
		if _, err := io.ReadFull(a.r, head[:]); err != nil {
			if errors.Is(err, io.EOF) {
				a.atEnd = true
				return "", nil, true, nil
			}
			return "", nil, false, err
		}
		sz := binary.LittleEndian.Uint32(head[:])
		if sz == archiveTerminator {
			// End of a namespace block; the next iteration peeks
			// again to see whether the archive ended (another
			// terminator → EOF) or a new block starts.
			a.inBlock = false
			a.curNs = ""
			continue
		}
		if sz < 5 {
			return "", nil, false, fmt.Errorf("invalid BSON length %d", sz)
		}
		body := make([]byte, sz-4)
		if _, err := io.ReadFull(a.r, body); err != nil {
			return "", nil, false, err
		}
		raw := make([]byte, sz)
		copy(raw[:4], head[:])
		copy(raw[4:], body)

		if !a.inBlock {
			// Namespace header: extract db + collection and start the
			// block. mongo-tools encodes namespace headers as separate
			// `db` and `collection` fields (NOT a combined `ns`); an
			// optional EOF flag closes a namespace's stream without
			// any body documents to follow.
			var meta struct {
				DB         string `bson:"db"`
				Collection string `bson:"collection"`
				EOF        bool   `bson:"EOF"`
			}
			if uerr := bson.Unmarshal(raw, &meta); uerr != nil {
				return "", nil, false, fmt.Errorf("namespace header bson: %w", uerr)
			}
			if meta.EOF {
				// EOF marker for an empty namespace block; reset and
				// peek the next item without entering the body state.
				a.curNs = ""
				continue
			}
			if meta.DB == "" || meta.Collection == "" {
				return "", nil, false, fmt.Errorf("namespace header missing db/collection: %s.%s", meta.DB, meta.Collection)
			}
			a.curNs = meta.DB + "." + meta.Collection
			a.inBlock = true
			continue
		}
		return a.curNs, raw, false, nil
	}
}

// readBSONDoc reads one length-prefixed BSON document from r and
// returns its raw bytes (including the length prefix). Used for the
// archive prelude which we skip after parsing the magic.
func readBSONDoc(r io.Reader) (bson.Raw, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	sz := binary.LittleEndian.Uint32(head[:])
	if sz < 5 {
		return nil, fmt.Errorf("invalid BSON length %d", sz)
	}
	body := make([]byte, sz-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	out := make([]byte, sz)
	copy(out[:4], head[:])
	copy(out[4:], body)
	return out, nil
}

// splitNamespace splits a `db.collection` namespace into its two
// pieces. Returns ("", "") for malformed inputs so the caller can
// skip them without erroring.
func splitNamespace(ns string) (string, string) {
	for i := range len(ns) {
		if ns[i] == '.' {
			return ns[:i], ns[i+1:]
		}
	}
	return "", ""
}
