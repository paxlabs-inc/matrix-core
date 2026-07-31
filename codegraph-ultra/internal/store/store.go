// Package store provides a SQLite-backed persistent graph store with full
// bidirectional edge indexing, full-text search, and incremental updates.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"codegraph-ultra/internal/model"
)

// DB wraps the SQLite database with the graph schema.
type DB struct {
	conn *sql.DB
	path string
}

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
	id       TEXT PRIMARY KEY,
	kind     TEXT NOT NULL,
	name     TEXT NOT NULL,
	qname    TEXT NOT NULL DEFAULT '',
	lang     TEXT NOT NULL DEFAULT '',
	file     TEXT NOT NULL DEFAULT '',
	line_start INTEGER NOT NULL DEFAULT 0,
	line_end   INTEGER NOT NULL DEFAULT 0,
	digest   TEXT NOT NULL DEFAULT '',
	sig      TEXT NOT NULL DEFAULT '',
	exported INTEGER NOT NULL DEFAULT 0,
	doc      TEXT NOT NULL DEFAULT '',
	summary  TEXT NOT NULL DEFAULT '',
	salience REAL NOT NULL DEFAULT 0,
	embed_ref TEXT NOT NULL DEFAULT '',
	extra    TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS edges (
	src  TEXT NOT NULL,
	dst  TEXT NOT NULL,
	type TEXT NOT NULL,
	site TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (src, dst, type),
	FOREIGN KEY (src) REFERENCES nodes(id) ON DELETE CASCADE,
	FOREIGN KEY (dst) REFERENCES nodes(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_edges_src ON edges(src, type);
CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges(dst, type);
CREATE INDEX IF NOT EXISTS idx_nodes_kind ON nodes(kind);
CREATE INDEX IF NOT EXISTS idx_nodes_name ON nodes(name);
CREATE INDEX IF NOT EXISTS idx_nodes_file ON nodes(file);
CREATE INDEX IF NOT EXISTS idx_nodes_lang ON nodes(lang);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

const ftsSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS nodes_fts USING fts5(
	id, name, qname, doc, sig, summary,
	content='nodes',
	content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS nodes_ai AFTER INSERT ON nodes BEGIN
	INSERT INTO nodes_fts(rowid, id, name, qname, doc, sig, summary)
	VALUES (new.rowid, new.id, new.name, new.qname, new.doc, new.sig, new.summary);
END;

CREATE TRIGGER IF NOT EXISTS nodes_ad AFTER DELETE ON nodes BEGIN
	INSERT INTO nodes_fts(nodes_fts, rowid, id, name, qname, doc, sig, summary)
	VALUES ('delete', old.rowid, old.id, old.name, old.qname, old.doc, old.sig, old.summary);
END;

CREATE TRIGGER IF NOT EXISTS nodes_au AFTER UPDATE ON nodes BEGIN
	INSERT INTO nodes_fts(nodes_fts, rowid, id, name, qname, doc, sig, summary)
	VALUES ('delete', old.rowid, old.id, old.name, old.qname, old.doc, old.sig, old.summary);
	INSERT INTO nodes_fts(rowid, id, name, qname, doc, sig, summary)
	VALUES (new.rowid, new.id, new.name, new.qname, new.doc, new.sig, new.summary);
END;
`

// Open creates or opens a SQLite graph store at the given path.
func Open(dbPath string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	conn, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_foreign_keys=ON&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// Try to create FTS5 virtual table — this may fail if SQLite was compiled
	// without FTS5 support (common in some distro packages). We degrade gracefully:
	// search will fall back to LIKE queries.
	if _, err := conn.Exec(ftsSchema); err != nil {
		// FTS5 not available — continue without it
	}
	return &DB{conn: conn, path: dbPath}, nil
}

// Close closes the database connection.
func (db *DB) Close() error { return db.conn.Close() }

// Path returns the database file path.
func (db *DB) Path() string { return db.path }

// SetMeta stores a key-value pair in the meta table.
func (db *DB) SetMeta(key, value string) error {
	_, err := db.conn.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?)`, key, value)
	return err
}

// GetMeta retrieves a value from the meta table.
func (db *DB) GetMeta(key string) (string, error) {
	var val string
	err := db.conn.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

// UpsertNode inserts or updates a node.
func (db *DB) UpsertNode(n *model.Node) error {
	_, err := db.conn.Exec(`
		INSERT INTO nodes (id, kind, name, qname, lang, file, line_start, line_end, digest, sig, exported, doc, summary, salience, embed_ref)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			kind=excluded.kind, name=excluded.name, qname=excluded.qname,
			lang=excluded.lang, file=excluded.file,
			line_start=excluded.line_start, line_end=excluded.line_end,
			digest=excluded.digest, sig=excluded.sig, exported=excluded.exported,
			doc=excluded.doc, summary=excluded.summary, salience=excluded.salience,
			embed_ref=excluded.embed_ref
	`, n.ID, n.Kind, n.Name, n.QName, n.Lang, n.File,
		n.Range.StartLine, n.Range.EndLine, n.Digest, n.Sig,
		boolToInt(n.Exported), n.Doc, n.Enrich.Summary, n.Enrich.Salience, n.Enrich.EmbedRef)
	return err
}

// UpsertEdge inserts or updates an edge.
func (db *DB) UpsertEdge(e model.Edge) error {
	_, err := db.conn.Exec(`
		INSERT INTO edges (src, dst, type, site) VALUES (?, ?, ?, ?)
		ON CONFLICT(src, dst, type) DO UPDATE SET site=excluded.site
	`, e.Src, e.Dst, e.Type, e.Site)
	return err
}

// GetNode returns a node by ID, or nil if not found.
func (db *DB) GetNode(id string) *model.Node {
	var n model.Node
	var exported int
	err := db.conn.QueryRow(`
		SELECT id, kind, name, qname, lang, file, line_start, line_end, digest, sig, exported, doc, summary, salience, embed_ref
		FROM nodes WHERE id = ?
	`, id).Scan(&n.ID, &n.Kind, &n.Name, &n.QName, &n.Lang, &n.File,
		&n.Range.StartLine, &n.Range.EndLine, &n.Digest, &n.Sig,
		&exported, &n.Doc, &n.Enrich.Summary, &n.Enrich.Salience, &n.Enrich.EmbedRef)
	if err != nil {
		return nil
	}
	n.Exported = exported != 0
	return &n
}

// Forward returns destination node IDs for a source node and edge type.
func (db *DB) Forward(src string, typ model.EdgeType) []string {
	return db.edgeQuery(`SELECT dst FROM edges WHERE src = ? AND type = ? ORDER BY dst`, src, typ)
}

// Reverse returns source node IDs for a destination node and edge type.
func (db *DB) Reverse(dst string, typ model.EdgeType) []string {
	return db.edgeQuery(`SELECT src FROM edges WHERE dst = ? AND type = ? ORDER BY src`, dst, typ)
}

// ForwardAll returns all destination IDs for a source node (any edge type).
func (db *DB) ForwardAll(src string) []string {
	return db.edgeQuery(`SELECT dst FROM edges WHERE src = ? ORDER BY dst`, src, "")
}

// ReverseAll returns all source IDs for a destination node (any edge type).
func (db *DB) ReverseAll(dst string) []string {
	return db.edgeQuery(`SELECT src FROM edges WHERE dst = ? ORDER BY dst`, dst, "")
}

func (db *DB) edgeQuery(query, id string, typ model.EdgeType) []string {
	var rows *sql.Rows
	var err error
	if typ != "" {
		rows, err = db.conn.Query(query, id, string(typ))
	} else {
		rows, err = db.conn.Query(strings.Replace(query, " AND type = ?", "", 1), id)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// Nodes returns all nodes, optionally filtered by kind.
func (db *DB) Nodes(kind model.Kind) []*model.Node {
	var rows *sql.Rows
	var err error
	if kind != "" {
		rows, err = db.conn.Query(`
			SELECT id, kind, name, qname, lang, file, line_start, line_end, digest, sig, exported, doc, summary, salience, embed_ref
			FROM nodes WHERE kind = ? ORDER BY id
		`, string(kind))
	} else {
		rows, err = db.conn.Query(`
			SELECT id, kind, name, qname, lang, file, line_start, line_end, digest, sig, exported, doc, summary, salience, embed_ref
			FROM nodes ORDER BY id
		`)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanNodes(rows)
}

// Edges returns all edges, optionally filtered by type.
func (db *DB) Edges(typ model.EdgeType) []model.Edge {
	var rows *sql.Rows
	var err error
	if typ != "" {
		rows, err = db.conn.Query(`SELECT src, dst, type, site FROM edges WHERE type = ? ORDER BY src, dst`, string(typ))
	} else {
		rows, err = db.conn.Query(`SELECT src, dst, type, site FROM edges ORDER BY src, type, dst`)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	var edges []model.Edge
	for rows.Next() {
		var e model.Edge
		if err := rows.Scan(&e.Src, &e.Dst, &e.Type, &e.Site); err == nil {
			edges = append(edges, e)
		}
	}
	return edges
}

// Search performs full-text search across node IDs, names, docs, sigs, and summaries.
// Falls back to LIKE queries if FTS5 is not available.
func (db *DB) Search(query string, limit int) []*model.Node {
	if limit <= 0 {
		limit = 20
	}

	// Try FTS5 first
	terms := strings.Fields(query)
	for i, t := range terms {
		terms[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"`
	}
	ftsQuery := strings.Join(terms, " AND ")
	rows, err := db.conn.Query(`
		SELECT n.id, n.kind, n.name, n.qname, n.lang, n.file, n.line_start, n.line_end, n.digest, n.sig, n.exported, n.doc, n.summary, n.salience, n.embed_ref
		FROM nodes_fts f
		JOIN nodes n ON n.rowid = f.rowid
		WHERE nodes_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, ftsQuery, limit)
	if err == nil {
		defer rows.Close()
		return scanNodes(rows)
	}

	// Fallback to LIKE queries
	likeQuery := "%" + strings.ReplaceAll(query, "%", "\\%") + "%"
	rows, err = db.conn.Query(`
		SELECT id, kind, name, qname, lang, file, line_start, line_end, digest, sig, exported, doc, summary, salience, embed_ref
		FROM nodes
		WHERE name LIKE ? OR qname LIKE ? OR doc LIKE ? OR sig LIKE ?
		ORDER BY name
		LIMIT ?
	`, likeQuery, likeQuery, likeQuery, likeQuery, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanNodes(rows)
}

// Stats returns aggregate graph statistics.
func (db *DB) Stats() model.GraphStats {
	var s model.GraphStats
	db.conn.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&s.TotalNodes)
	db.conn.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&s.TotalEdges)
	db.conn.QueryRow(`SELECT COUNT(DISTINCT file) FROM nodes WHERE file != ''`).Scan(&s.FilesCount)

	s.NodesByKind = make(map[model.Kind]int)
	rows, _ := db.conn.Query(`SELECT kind, COUNT(*) FROM nodes GROUP BY kind`)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var k string
			var c int
			rows.Scan(&k, &c)
			s.NodesByKind[model.Kind(k)] = c
		}
	}

	s.EdgesByType = make(map[model.EdgeType]int)
	rows2, _ := db.conn.Query(`SELECT type, COUNT(*) FROM edges GROUP BY type`)
	if rows2 != nil {
		defer rows2.Close()
		for rows2.Next() {
			var t string
			var c int
			rows2.Scan(&t, &c)
			s.EdgesByType[model.EdgeType(t)] = c
		}
	}

	rows3, _ := db.conn.Query(`SELECT DISTINCT lang FROM nodes WHERE lang != '' ORDER BY lang`)
	if rows3 != nil {
		defer rows3.Close()
		for rows3.Next() {
			var l string
			rows3.Scan(&l)
			s.Languages = append(s.Languages, l)
		}
	}
	return s
}

// Clear removes all nodes and edges.
func (db *DB) Clear() error {
	_, err := db.conn.Exec(`DELETE FROM edges; DELETE FROM nodes; DELETE FROM meta;`)
	return err
}

// Transaction runs fn in a transaction.
func (db *DB) Transaction(fn func(tx *sql.Tx) error) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// LoadIndex reads the full graph into an in-memory model.Index for fast traversal.
func (db *DB) LoadIndex() *model.Index {
	ix := model.NewIndex()
	for _, n := range db.Nodes("") {
		ix.AddNode(n)
	}
	for _, e := range db.Edges("") {
		ix.AddEdge(e)
	}
	return ix
}

// ExportJSON exports the full graph as JSON.
func (db *DB) ExportJSON() ([]byte, error) {
	data := map[string]any{
		"nodes":  db.Nodes(""),
		"edges":  db.Edges(""),
		"stats":  db.Stats(),
	}
	return json.MarshalIndent(data, "", "  ")
}

func scanNodes(rows *sql.Rows) []*model.Node {
	var nodes []*model.Node
	for rows.Next() {
		var n model.Node
		var exported int
		if err := rows.Scan(&n.ID, &n.Kind, &n.Name, &n.QName, &n.Lang, &n.File,
			&n.Range.StartLine, &n.Range.EndLine, &n.Digest, &n.Sig,
			&exported, &n.Doc, &n.Enrich.Summary, &n.Enrich.Salience, &n.Enrich.EmbedRef); err == nil {
			n.Exported = exported != 0
			nodes = append(nodes, &n)
		}
	}
	return nodes
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Neighbors returns the bounded subgraph around a node ID.
func (db *DB) Neighbors(id string, typ model.EdgeType, depth int) []*model.Node {
	if depth <= 0 {
		depth = 1
	}
	visited := map[string]bool{id: true}
	frontier := []string{id}
	for d := 0; d < depth; d++ {
		var next []string
		for _, cur := range frontier {
			var neighbors []string
			if typ != "" {
				neighbors = append(db.Forward(cur, typ), db.Reverse(cur, typ)...)
			} else {
				neighbors = append(db.ForwardAll(cur), db.ReverseAll(cur)...)
			}
			for _, nb := range neighbors {
				if !visited[nb] {
					visited[nb] = true
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}
	ids := make([]string, 0, len(visited))
	for id := range visited {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var nodes []*model.Node
	for _, id := range ids {
		if n := db.GetNode(id); n != nil {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// Impact returns the reverse transitive closure (callers, references, implementors).
func (db *DB) Impact(id string, maxDepth int) []*model.Node {
	if maxDepth <= 0 {
		maxDepth = 64
	}
	impactEdges := []model.EdgeType{model.EdgeCalls, model.EdgeReferences, model.EdgeImplements}
	seen := map[string]bool{id: true}
	affected := map[string]int{}
	frontier := []string{id}
	for d := 1; d <= maxDepth && len(frontier) > 0; d++ {
		var next []string
		for _, cur := range frontier {
			for _, t := range impactEdges {
				for _, src := range db.Reverse(cur, t) {
					if !seen[src] {
						seen[src] = true
						affected[src] = d
						next = append(next, src)
					}
				}
			}
		}
		frontier = next
	}
	ids := make([]string, 0, len(affected))
	for id := range affected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var nodes []*model.Node
	for _, id := range ids {
		if n := db.GetNode(id); n != nil {
			nodes = append(nodes, n)
		}
	}
	return nodes
}
