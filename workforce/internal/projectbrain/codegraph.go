package projectbrain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"matrix/workforce/internal/contracts"
)

const (
	commandOutputLimit       = 16 << 20
	codeGraphBoundaryTimeout = 30 * time.Second
	maxSourceFileBytes       = 1 << 30
)

type indexedFile struct {
	Path      string `json:"path"`
	Language  string `json:"language"`
	NodeCount uint64 `json:"nodeCount"`
	Digest    string `json:"digest"`
}

type cgRange struct {
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`
}

type cgNode struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	QName     string          `json:"qname"`
	Language  string          `json:"lang"`
	File      string          `json:"file"`
	Range     cgRange         `json:"range"`
	Digest    string          `json:"digest"`
	Signature string          `json:"sig"`
	Exported  bool            `json:"exported"`
	Enrich    json.RawMessage `json:"enrich"`
	NodeCount uint64          `json:"-"`
}

type cgEdge struct {
	Source string `json:"src"`
	Target string `json:"dst"`
	Type   string `json:"type"`
}

type cgStats struct {
	TotalNodes  uint64            `json:"total_nodes"`
	TotalEdges  uint64            `json:"total_edges"`
	NodesByKind map[string]uint64 `json:"nodes_by_kind"`
	EdgesByType map[string]uint64 `json:"edges_by_type"`
	FilesCount  uint64            `json:"files_count"`
	Languages   []string          `json:"languages"`
}

type cgExport struct {
	Edges []cgEdge `json:"edges"`
	Nodes []cgNode `json:"nodes"`
	Stats cgStats  `json:"stats"`
}

// ImpactNode is one CodeGraph symbol or dependent in a bounded change blast radius.
type ImpactNode struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	FilePath  string `json:"filePath"`
	StartLine uint32 `json:"startLine"`
}

// ImpactResult is the structured CodeGraph impact projection for one symbol.
type ImpactResult struct {
	Symbol    string       `json:"symbol"`
	Depth     uint32       `json:"depth"`
	NodeCount uint64       `json:"nodeCount"`
	EdgeCount uint64       `json:"edgeCount"`
	Affected  []ImpactNode `json:"affected"`
}

// AffectedTests is the structured test projection for a set of changed files.
type AffectedTests struct {
	ChangedFiles             []string `json:"changedFiles"`
	AffectedTests            []string `json:"affectedTests"`
	TotalDependentsTraversed uint64   `json:"totalDependentsTraversed"`
}

// CodeGraph reads the current persistent CodeGraph through its versioned CLI
// process boundary and always hashes source bytes directly.
type CodeGraph struct {
	executable       string
	resolved         string
	executableDigest [sha256.Size]byte
	environment      []string
	now              func() time.Time
}

// NewCodeGraph constructs the real CodeGraph process adapter.
func NewCodeGraph(executable string, now func() time.Time) (*CodeGraph, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" || now == nil {
		return nil, fmt.Errorf("project brain CodeGraph executable and time source are required")
	}
	base := filepath.Base(executable)
	if !filepath.IsAbs(executable) || base != "cg" {
		return nil, fmt.Errorf("project brain CodeGraph Ultra executable must be an absolute cg path")
	}
	resolved, digest, err := verifyCodeGraphExecutable(executable)
	if err != nil {
		return nil, err
	}
	return &CodeGraph{
		executable: resolved, resolved: resolved, executableDigest: digest,
		environment: []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "NO_COLOR=1"},
		now:         now,
	}, nil
}

// Impact resolves a bounded live CodeGraph blast radius for one symbol.
func (graph *CodeGraph) Impact(
	ctx context.Context,
	workspaceRoot, symbol string,
	depth uint32,
) (ImpactResult, error) {
	if graph == nil || strings.TrimSpace(symbol) == "" || len(symbol) > 512 ||
		depth == 0 || depth > 8 || strings.HasPrefix(symbol, "-") ||
		strings.IndexFunc(symbol, func(character rune) bool {
			return character == 0 || character < 0x20 || character == 0x7f
		}) >= 0 {
		return ImpactResult{}, fmt.Errorf("project brain CodeGraph impact request is invalid")
	}
	root, err := resolveWorkspaceRoot(workspaceRoot)
	if err != nil {
		return ImpactResult{}, err
	}
	exported, _, _, err := graph.exportGraph(ctx, root)
	if err != nil {
		return ImpactResult{}, err
	}
	nodeByID := make(map[string]cgNode, len(exported.Nodes))
	reverse := make(map[string][]string)
	seeds := make([]string, 0)
	for _, node := range exported.Nodes {
		nodeByID[node.ID] = node
		if node.Name == symbol || node.QName == symbol ||
			strings.HasSuffix(node.QName, "."+symbol) {
			seeds = append(seeds, node.ID)
		}
	}
	for _, edge := range exported.Edges {
		reverse[edge.Target] = append(reverse[edge.Target], edge.Source)
	}
	if len(seeds) == 0 {
		return ImpactResult{}, fmt.Errorf("project brain CodeGraph impact result is invalid")
	}
	type visit struct {
		id    string
		depth uint32
	}
	queue := make([]visit, 0, len(seeds))
	visited := make(map[string]struct{}, len(seeds))
	for _, seed := range seeds {
		queue = append(queue, visit{id: seed})
		visited[seed] = struct{}{}
	}
	edgeCount := uint64(0)
	for len(queue) > 0 && len(visited) <= 10000 {
		current := queue[0]
		queue = queue[1:]
		if current.depth == depth {
			continue
		}
		for _, caller := range reverse[current.id] {
			edgeCount++
			if _, exists := visited[caller]; exists {
				continue
			}
			visited[caller] = struct{}{}
			queue = append(queue, visit{id: caller, depth: current.depth + 1})
		}
	}
	affected := make([]ImpactNode, 0, len(visited))
	seen := make(map[string]struct{}, len(visited))
	for id := range visited {
		node := nodeByID[id]
		if !isImpactKind(node.Kind) || node.Range.StartLine == 0 ||
			validateRelativePath(filepath.ToSlash(node.File)) != nil {
			continue
		}
		candidate := ImpactNode{
			Name: node.Name, Kind: node.Kind,
			FilePath: filepath.ToSlash(node.File), StartLine: node.Range.StartLine,
		}
		key := candidate.Name + "\x00" + candidate.Kind + "\x00" +
			candidate.FilePath + "\x00" + fmt.Sprintf("%d", candidate.StartLine)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		affected = append(affected, candidate)
	}
	sort.Slice(affected, func(left, right int) bool {
		if affected[left].FilePath != affected[right].FilePath {
			return affected[left].FilePath < affected[right].FilePath
		}
		if affected[left].StartLine != affected[right].StartLine {
			return affected[left].StartLine < affected[right].StartLine
		}
		return affected[left].Name < affected[right].Name
	})
	if len(affected) == 0 || len(affected) > 10000 {
		return ImpactResult{}, fmt.Errorf("project brain CodeGraph impact result is invalid")
	}
	for _, node := range affected {
		if strings.TrimSpace(node.Name) == "" || strings.TrimSpace(node.Kind) == "" ||
			strings.TrimSpace(node.FilePath) == "" || len(node.FilePath) > 4096 ||
			node.StartLine == 0 {
			return ImpactResult{}, fmt.Errorf("project brain CodeGraph impact node is invalid")
		}
		path := filepath.ToSlash(node.FilePath)
		if err := validateRelativePath(path); err != nil {
			return ImpactResult{}, fmt.Errorf("project brain CodeGraph impact path: %w", err)
		}
		if _, _, err := hashWorkspaceFile(
			root, filepath.Join(root, filepath.FromSlash(path)),
		); err != nil {
			return ImpactResult{}, fmt.Errorf(
				"project brain CodeGraph impact source is not current: %w", err,
			)
		}
	}
	return ImpactResult{
		Symbol: symbol, Depth: depth, NodeCount: uint64(len(visited)),
		EdgeCount: edgeCount, Affected: affected,
	}, nil
}

// TestsAffected resolves real tests reachable from the declared changed files.
func (graph *CodeGraph) TestsAffected(
	ctx context.Context,
	workspaceRoot string,
	files []string,
	depth uint32,
) (AffectedTests, error) {
	if graph == nil || len(files) == 0 || len(files) > 256 || depth == 0 || depth > 8 {
		return AffectedTests{}, fmt.Errorf("project brain CodeGraph affected-test request is invalid")
	}
	root, err := resolveWorkspaceRoot(workspaceRoot)
	if err != nil {
		return AffectedTests{}, err
	}
	requested := make([]string, 0, len(files))
	for _, file := range files {
		file = filepath.ToSlash(file)
		if err := validateRelativePath(file); err != nil {
			return AffectedTests{}, err
		}
		if _, _, err := hashWorkspaceFile(
			root, filepath.Join(root, filepath.FromSlash(file)),
		); err != nil {
			return AffectedTests{}, fmt.Errorf(
				"project brain changed source is not current: %w", err,
			)
		}
		requested = append(requested, file)
	}
	exported, _, _, err := graph.exportGraph(ctx, root)
	if err != nil {
		return AffectedTests{}, err
	}
	nodeByID := make(map[string]cgNode, len(exported.Nodes))
	adjacent := make(map[string][]string)
	queue := make([]string, 0)
	visited := make(map[string]uint32)
	requestedSet := make(map[string]struct{}, len(requested))
	for _, path := range requested {
		requestedSet[path] = struct{}{}
	}
	for _, node := range exported.Nodes {
		nodeByID[node.ID] = node
		if _, selected := requestedSet[filepath.ToSlash(node.File)]; selected {
			if _, exists := visited[node.ID]; !exists {
				visited[node.ID] = 0
				queue = append(queue, node.ID)
			}
		}
	}
	for _, edge := range exported.Edges {
		adjacent[edge.Source] = append(adjacent[edge.Source], edge.Target)
		adjacent[edge.Target] = append(adjacent[edge.Target], edge.Source)
	}
	for len(queue) > 0 && len(visited) <= 10000 {
		id := queue[0]
		queue = queue[1:]
		currentDepth := visited[id]
		if currentDepth == depth {
			continue
		}
		for _, next := range adjacent[id] {
			if _, exists := visited[next]; exists {
				continue
			}
			visited[next] = currentDepth + 1
			queue = append(queue, next)
		}
	}
	affectedSet := make(map[string]struct{})
	affectedDirectories := make(map[string]struct{})
	affectedSourceStems := make(map[string]map[string]struct{})
	addAffectedSource := func(path string) {
		if validateRelativePath(path) != nil || isTestPath(path) {
			return
		}
		directory := filepath.ToSlash(filepath.Dir(path))
		affectedDirectories[directory] = struct{}{}
		if affectedSourceStems[directory] == nil {
			affectedSourceStems[directory] = make(map[string]struct{})
		}
		affectedSourceStems[directory][sourceStem(path)] = struct{}{}
	}
	for id := range visited {
		path := filepath.ToSlash(nodeByID[id].File)
		if isTestPath(path) {
			affectedSet[path] = struct{}{}
		}
	}
	for changed := range requestedSet {
		addAffectedSource(changed)
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && skipSourceDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("project brain test source symlink is forbidden")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !isTestPath(relative) {
			return nil
		}
		directory := filepath.ToSlash(filepath.Dir(relative))
		stems, selected := affectedSourceStems[directory]
		if _, directorySelected := affectedDirectories[directory]; directorySelected &&
			selected && testMatchesSourceStem(relative, stems) {
			affectedSet[relative] = struct{}{}
			if len(affectedSet) > 10000 {
				return fmt.Errorf("project brain affected test bound exceeded")
			}
		}
		return nil
	}); err != nil {
		return AffectedTests{}, err
	}
	affected := make([]string, 0, len(affectedSet))
	for path := range affectedSet {
		if err := validateRelativePath(path); err != nil {
			return AffectedTests{}, fmt.Errorf(
				"project brain CodeGraph affected test path: %w", err,
			)
		}
		if _, _, err := hashWorkspaceFile(
			root, filepath.Join(root, filepath.FromSlash(path)),
		); err != nil {
			return AffectedTests{}, fmt.Errorf(
				"project brain CodeGraph affected test is not current: %w", err,
			)
		}
		affected = append(affected, path)
	}
	sort.Strings(requested)
	sort.Strings(affected)
	return AffectedTests{
		ChangedFiles: requested, AffectedTests: affected,
		TotalDependentsTraversed: uint64(len(visited)),
	}, nil
}

func indexedFiles(exported cgExport, filter string) []indexedFile {
	byPath := make(map[string]indexedFile)
	for _, node := range exported.Nodes {
		path := filepath.ToSlash(node.File)
		if validateRelativePath(path) != nil ||
			filter != "" && path != filter && !strings.HasPrefix(path, filter+"/") {
			continue
		}
		current := byPath[path]
		current.Path = path
		if current.Language == "" {
			current.Language = node.Language
		}
		if node.Kind == "file" && node.NodeCount > 0 {
			current.NodeCount = node.NodeCount
		} else {
			current.NodeCount++
		}
		if node.Kind == "file" {
			current.Digest = node.Digest
			if node.Language != "" {
				current.Language = node.Language
			}
		}
		byPath[path] = current
	}
	result := make([]indexedFile, 0, len(byPath))
	for _, file := range byPath {
		if strings.HasPrefix(file.Digest, "sha256:") {
			result = append(result, file)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Path < result[right].Path
	})
	return result
}

func isImpactKind(kind string) bool {
	switch kind {
	case "class", "const", "constant", "func", "function", "interface", "method",
		"struct", "type", "type_alias", "var", "variable":
		return true
	default:
		return false
	}
}

func isTestPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".test.tsx") ||
		strings.HasSuffix(base, ".test.js") ||
		strings.HasSuffix(base, ".test.jsx") ||
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") ||
		strings.HasSuffix(base, "_test.py")
}

func sourceStem(path string) string {
	base := strings.ToLower(filepath.Base(path))
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func testMatchesSourceStem(path string, stems map[string]struct{}) bool {
	base := strings.ToLower(filepath.Base(path))
	candidates := []string{
		strings.TrimSuffix(base, "_test.go"),
		strings.TrimSuffix(base, ".test.ts"),
		strings.TrimSuffix(base, ".test.tsx"),
		strings.TrimSuffix(base, ".test.js"),
		strings.TrimSuffix(base, ".test.jsx"),
		strings.TrimSuffix(strings.TrimPrefix(base, "test_"), ".py"),
		strings.TrimSuffix(base, "_test.py"),
	}
	for _, candidate := range candidates {
		if _, exists := stems[candidate]; exists {
			return true
		}
	}
	return false
}

// Capture binds one CodeGraph generation to the actual current source files in scope.
func (graph *CodeGraph) Capture(
	ctx context.Context,
	workspaceRoot, filter string,
) (GraphSnapshot, error) {
	root, err := resolveWorkspaceRoot(workspaceRoot)
	if err != nil {
		return GraphSnapshot{}, err
	}
	if err := validateFilter(filter); err != nil {
		return GraphSnapshot{}, err
	}
	exported, exportDigest, indexedAt, err := graph.exportGraph(ctx, root)
	if err != nil {
		return GraphSnapshot{}, err
	}
	capturedAt := graph.now()
	if capturedAt.IsZero() || capturedAt.Location() != time.UTC || capturedAt.Before(indexedAt) {
		return GraphSnapshot{}, fmt.Errorf("project brain capture time is invalid")
	}
	indexed := indexedFiles(exported, filter)
	files, pending, err := captureFiles(root, filter, indexed)
	if err != nil {
		return GraphSnapshot{}, err
	}
	rootDigest, err := hashGraphFiles(files)
	if err != nil {
		return GraphSnapshot{}, err
	}
	graphDigest, err := digestGraphGeneration(exportDigest, exported.Stats, rootDigest, files)
	if err != nil {
		return GraphSnapshot{}, err
	}
	generation := uint64(indexedAt.UnixNano())
	if generation == 0 {
		return GraphSnapshot{}, fmt.Errorf("project brain CodeGraph generation is invalid")
	}
	finalExport, finalDigest, finalIndexedAt, err := graph.exportGraph(ctx, root)
	if err != nil {
		return GraphSnapshot{}, err
	}
	if finalDigest != exportDigest || !finalIndexedAt.Equal(indexedAt) ||
		finalExport.Stats.TotalNodes != exported.Stats.TotalNodes ||
		finalExport.Stats.TotalEdges != exported.Stats.TotalEdges {
		return GraphSnapshot{}, fmt.Errorf("project brain CodeGraph changed during capture")
	}
	if err := revalidateGraphFiles(root, filter, files); err != nil {
		return GraphSnapshot{}, err
	}
	snapshot := GraphSnapshot{
		SchemaVersion: contracts.SchemaVersionV1,
		RootDigest:    rootDigest, GraphDigest: graphDigest,
		Generation: generation, IndexedAt: indexedAt, CapturedAt: capturedAt,
		Fresh: len(pending) == 0, PendingFiles: pending, Files: files,
		NodeCount: exported.Stats.TotalNodes, EdgeCount: exported.Stats.TotalEdges,
	}
	if err := snapshot.Validate(); err != nil {
		return GraphSnapshot{}, err
	}
	return snapshot, nil
}

// CaptureFiles binds one CodeGraph generation to an explicit bounded source set.
func (graph *CodeGraph) CaptureFiles(
	ctx context.Context,
	workspaceRoot string,
	paths []string,
) (GraphSnapshot, error) {
	root, err := resolveWorkspaceRoot(workspaceRoot)
	if err != nil {
		return GraphSnapshot{}, err
	}
	requested, err := exactSourcePaths(paths)
	if err != nil {
		return GraphSnapshot{}, err
	}
	exported, exportDigest, indexedAt, err := graph.exportGraph(ctx, root)
	if err != nil {
		return GraphSnapshot{}, err
	}
	capturedAt := graph.now()
	if capturedAt.IsZero() || capturedAt.Location() != time.UTC || capturedAt.Before(indexedAt) {
		return GraphSnapshot{}, fmt.Errorf("project brain capture time is invalid")
	}
	indexed := indexedFiles(exported, "")
	files, pending, err := captureExactFiles(root, requested, indexed)
	if err != nil {
		return GraphSnapshot{}, err
	}
	rootDigest, err := hashGraphFiles(files)
	if err != nil {
		return GraphSnapshot{}, err
	}
	graphDigest, err := digestGraphGeneration(exportDigest, exported.Stats, rootDigest, files)
	if err != nil {
		return GraphSnapshot{}, err
	}
	generation := uint64(indexedAt.UnixNano())
	if generation == 0 {
		return GraphSnapshot{}, fmt.Errorf("project brain CodeGraph generation is invalid")
	}
	finalExport, finalDigest, finalIndexedAt, err := graph.exportGraph(ctx, root)
	if err != nil {
		return GraphSnapshot{}, err
	}
	if finalDigest != exportDigest || !finalIndexedAt.Equal(indexedAt) ||
		finalExport.Stats.TotalNodes != exported.Stats.TotalNodes ||
		finalExport.Stats.TotalEdges != exported.Stats.TotalEdges {
		return GraphSnapshot{}, fmt.Errorf("project brain CodeGraph changed during capture")
	}
	if err := revalidateExactGraphFiles(root, files); err != nil {
		return GraphSnapshot{}, err
	}
	snapshot := GraphSnapshot{
		SchemaVersion: contracts.SchemaVersionV1,
		RootDigest:    rootDigest, GraphDigest: graphDigest,
		Generation: generation, IndexedAt: indexedAt, CapturedAt: capturedAt,
		Fresh: len(pending) == 0, PendingFiles: pending, Files: files,
		NodeCount: exported.Stats.TotalNodes, EdgeCount: exported.Stats.TotalEdges,
	}
	if err := snapshot.Validate(); err != nil {
		return GraphSnapshot{}, err
	}
	return snapshot, nil
}

func exactSourcePaths(paths []string) ([]string, error) {
	if len(paths) == 0 || len(paths) > 10000 {
		return nil, fmt.Errorf("project brain exact source set must contain 1 to 10000 paths")
	}
	set := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = filepath.ToSlash(path)
		if err := validateRelativePath(path); err != nil {
			return nil, err
		}
		if filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))) != path {
			return nil, fmt.Errorf("project brain exact source path must be canonical")
		}
		set[path] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for path := range set {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

func resolveWorkspaceRoot(workspaceRoot string) (string, error) {
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("project brain workspace root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("project brain resolve workspace root: %w", err)
	}
	return root, nil
}

func (graph *CodeGraph) exportGraph(
	ctx context.Context,
	root string,
) (cgExport, [sha256.Size]byte, time.Time, error) {
	boundaryContext, cancel := context.WithTimeout(ctx, codeGraphBoundaryTimeout)
	defer cancel()
	resolved, digest, err := verifyCodeGraphExecutable(graph.executable)
	if err != nil || resolved != graph.resolved || digest != graph.executableDigest {
		return cgExport{}, [sha256.Size]byte{}, time.Time{},
			fmt.Errorf("project brain CodeGraph executable changed")
	}
	database := filepath.Join(root, ".cg", "codegraph.db")
	if err := validateCodeGraphDatabase(database); err != nil {
		return cgExport{}, [sha256.Size]byte{}, time.Time{}, err
	}
	indexedAt, err := codeGraphIndexedAt(database)
	if err != nil {
		return cgExport{}, [sha256.Size]byte{}, time.Time{}, err
	}
	exportFile, err := os.CreateTemp("", "workforce-cg-export-*.json")
	if err != nil {
		return cgExport{}, [sha256.Size]byte{}, time.Time{},
			fmt.Errorf("project brain create CodeGraph export: %w", err)
	}
	exportPath := exportFile.Name()
	defer func() {
		_ = exportFile.Close()
		_ = os.Remove(exportPath)
	}()
	command := exec.CommandContext(
		boundaryContext, graph.executable,
		"--db", database, "export", "json", exportPath,
	)
	command.Dir = root
	command.Env = append([]string(nil), graph.environment...)
	stdout := boundedOutput{limit: commandOutputLimit, cancel: cancel}
	stderr := boundedOutput{limit: 64 << 10, cancel: cancel}
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	if stdout.exceeded {
		return cgExport{}, [sha256.Size]byte{}, time.Time{},
			fmt.Errorf("project brain CodeGraph output exceeds %d bytes", commandOutputLimit)
	}
	if stderr.exceeded {
		return cgExport{}, [sha256.Size]byte{}, time.Time{},
			fmt.Errorf("project brain CodeGraph error output exceeds %d bytes", 64<<10)
	}
	if boundaryContext.Err() != nil {
		return cgExport{}, [sha256.Size]byte{}, time.Time{},
			fmt.Errorf("project brain CodeGraph boundary: %w", boundaryContext.Err())
	}
	if runErr != nil {
		return cgExport{}, [sha256.Size]byte{}, time.Time{},
			fmt.Errorf("project brain CodeGraph failed")
	}
	openedInfo, statErr := exportFile.Stat()
	pathInfo, pathErr := os.Lstat(exportPath)
	if statErr != nil || pathErr != nil || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(openedInfo, pathInfo) || openedInfo.Size() <= 0 ||
		openedInfo.Size() > commandOutputLimit {
		return cgExport{}, [sha256.Size]byte{}, time.Time{},
			fmt.Errorf("project brain CodeGraph export is invalid")
	}
	if _, err := exportFile.Seek(0, io.SeekStart); err != nil {
		return cgExport{}, [sha256.Size]byte{}, time.Time{},
			fmt.Errorf("project brain read CodeGraph export: %w", err)
	}
	exportBytes, err := io.ReadAll(io.LimitReader(exportFile, commandOutputLimit+1))
	if err != nil || len(exportBytes) > commandOutputLimit {
		return cgExport{}, [sha256.Size]byte{}, time.Time{},
			fmt.Errorf("project brain read CodeGraph export")
	}
	var target cgExport
	decoder := json.NewDecoder(bytes.NewReader(exportBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&target); err != nil {
		return cgExport{}, [sha256.Size]byte{}, time.Time{},
			fmt.Errorf("project brain decode CodeGraph output: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return cgExport{}, [sha256.Size]byte{}, time.Time{},
			fmt.Errorf("project brain CodeGraph returned trailing data")
	}
	if target.Stats.TotalNodes == 0 || target.Stats.FilesCount == 0 ||
		len(target.Nodes) == 0 {
		return cgExport{}, [sha256.Size]byte{}, time.Time{},
			fmt.Errorf("project brain CodeGraph is not complete for the requested workspace")
	}
	return target, sha256.Sum256(exportBytes), indexedAt, nil
}

func codeGraphIndexedAt(database string) (time.Time, error) {
	info, err := os.Lstat(database)
	if err != nil || !info.Mode().IsRegular() {
		return time.Time{}, fmt.Errorf("project brain CodeGraph database is unavailable")
	}
	return info.ModTime().UTC(), nil
}

func validateCodeGraphDatabase(database string) error {
	directory := filepath.Dir(database)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("project brain CodeGraph database directory is invalid")
	}
	for _, path := range []string{database, database + "-wal", database + "-shm"} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && path != database {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("project brain CodeGraph database is invalid")
		}
	}
	return nil
}

func verifyCodeGraphExecutable(path string) (string, [sha256.Size]byte, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("project brain CodeGraph executable: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("project brain CodeGraph executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 ||
		info.Mode().Perm()&0o022 != 0 {
		return "", [sha256.Size]byte{},
			fmt.Errorf("project brain CodeGraph executable is not trusted")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return "", [sha256.Size]byte{},
			fmt.Errorf("project brain CodeGraph executable is not trusted")
	}
	for _, candidate := range []string{filepath.Dir(path), filepath.Dir(resolved)} {
		if err := verifyCodeGraphAncestry(candidate); err != nil {
			return "", [sha256.Size]byte{}, err
		}
	}
	file, err := os.Open(resolved)
	if err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("project brain CodeGraph executable: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", [sha256.Size]byte{},
			fmt.Errorf("project brain CodeGraph executable changed while opening")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", [sha256.Size]byte{}, fmt.Errorf("project brain CodeGraph executable: %w", err)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return resolved, digest, nil
}

func verifyCodeGraphAncestry(path string) error {
	for directory := filepath.Clean(path); ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("project brain CodeGraph executable ancestry is not trusted")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return fmt.Errorf("project brain CodeGraph executable ancestry is not trusted")
		}
		if directory == "/" {
			return nil
		}
	}
}

type boundedOutput struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (output *boundedOutput) Write(value []byte) (int, error) {
	accepted := len(value)
	remaining := output.limit - output.buffer.Len()
	if remaining > 0 {
		if remaining > len(value) {
			remaining = len(value)
		}
		_, _ = output.buffer.Write(value[:remaining])
	}
	if accepted > remaining {
		output.exceeded = true
		output.cancel()
	}
	return accepted, nil
}

func (output *boundedOutput) Bytes() []byte {
	return output.buffer.Bytes()
}

func captureFiles(
	root, filter string,
	indexed []indexedFile,
) ([]GraphFile, []string, error) {
	indexedByPath := make(map[string]indexedFile, len(indexed))
	for _, file := range indexed {
		if err := validateRelativePath(file.Path); err != nil {
			return nil, nil, err
		}
		indexedByPath[file.Path] = file
	}
	files := make([]GraphFile, 0, len(indexed))
	pendingSet := make(map[string]struct{})
	for path, metadata := range indexedByPath {
		absolute := filepath.Join(root, filepath.FromSlash(path))
		hash, info, err := hashWorkspaceFile(root, absolute)
		if err != nil {
			if os.IsNotExist(err) {
				pendingSet[path] = struct{}{}
				continue
			}
			return nil, nil, fmt.Errorf("project brain hash stable source %q: %w", path, err)
		}
		indexedDigest := strings.TrimPrefix(metadata.Digest, "sha256:")
		indexedCurrent := len(indexedDigest) == 16 &&
			strings.HasPrefix(hash.Digest, indexedDigest)
		if !indexedCurrent {
			pendingSet[path] = struct{}{}
		}
		files = append(files, GraphFile{
			Path: path, Language: metadata.Language, NodeCount: metadata.NodeCount,
			SizeBytes: uint64(info.Size()), Hash: hash, Indexed: indexedCurrent,
		})
	}
	scope := root
	if filter != "" {
		scope = filepath.Join(root, filepath.FromSlash(filter))
	}
	if err := filepath.WalkDir(scope, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != scope && skipSourceDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("project brain source symlink is forbidden: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if _, exists := indexedByPath[relative]; !exists && isSourceFile(relative) {
			hash, info, err := hashWorkspaceFile(root, path)
			if err != nil {
				return err
			}
			files = append(files, GraphFile{
				Path: relative, Language: sourceLanguage(relative),
				SizeBytes: uint64(info.Size()), Hash: hash, Indexed: false,
			})
			if !isTestPath(relative) && !isSourceManifest(relative) {
				pendingSet[relative] = struct{}{}
			}
		}
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("project brain discover current source: %w", err)
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	pending := make([]string, 0, len(pendingSet))
	for path := range pendingSet {
		pending = append(pending, path)
	}
	sort.Strings(pending)
	return files, pending, nil
}

func captureExactFiles(
	root string,
	paths []string,
	indexed []indexedFile,
) ([]GraphFile, []string, error) {
	indexedByPath := make(map[string]indexedFile, len(indexed))
	for _, file := range indexed {
		indexedByPath[file.Path] = file
	}
	files := make([]GraphFile, 0, len(paths))
	pending := make([]string, 0)
	for _, path := range paths {
		hash, info, err := hashWorkspaceFile(
			root, filepath.Join(root, filepath.FromSlash(path)),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("project brain hash exact source %q: %w", path, err)
		}
		metadata, exists := indexedByPath[path]
		indexedCurrent := false
		if exists {
			indexedDigest := strings.TrimPrefix(metadata.Digest, "sha256:")
			indexedCurrent = len(indexedDigest) == 16 &&
				strings.HasPrefix(hash.Digest, indexedDigest)
		}
		if !indexedCurrent && (exists || !isTestPath(path)) {
			pending = append(pending, path)
		}
		language := sourceLanguage(path)
		if metadata.Language != "" {
			language = metadata.Language
		}
		files = append(files, GraphFile{
			Path: path, Language: language, NodeCount: metadata.NodeCount,
			SizeBytes: uint64(info.Size()), Hash: hash, Indexed: indexedCurrent,
		})
	}
	return files, pending, nil
}

func hashGraphFiles(files []GraphFile) (contracts.ContentHash, error) {
	hasher := sha256Writer()
	for _, file := range files {
		if _, err := io.WriteString(hasher, file.Path+"\x00"+file.Hash.Digest+"\n"); err != nil {
			return contracts.ContentHash{}, err
		}
	}
	return digestFromHasher(hasher), nil
}

func sha256Writer() hash.Hash {
	return sha256.New()
}

func digestFromHasher(hasher hash.Hash) contracts.ContentHash {
	return contracts.ContentHash{
		Algorithm: "sha256",
		Digest:    hex.EncodeToString(hasher.Sum(nil)),
	}
}

func hashWorkspaceFile(root, path string) (contracts.ContentHash, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return contracts.ContentHash{}, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return contracts.ContentHash{}, nil, fmt.Errorf("source must be a regular non-symlink file")
	}
	if before.Size() > maxSourceFileBytes {
		return contracts.ContentHash{}, nil, fmt.Errorf("source exceeds one GiB hash bound")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return contracts.ContentHash{}, nil, err
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return contracts.ContentHash{}, nil, fmt.Errorf("source resolves outside workspace")
	}
	file, err := os.Open(path)
	if err != nil {
		return contracts.ContentHash{}, nil, err
	}
	hasher := sha256Writer()
	copied, copyErr := io.Copy(hasher, io.LimitReader(file, maxSourceFileBytes+1))
	openedAfter, statErr := file.Stat()
	closeErr := file.Close()
	pathAfter, lstatErr := os.Lstat(path)
	if copyErr != nil || statErr != nil || closeErr != nil || lstatErr != nil {
		return contracts.ContentHash{}, nil, fmt.Errorf("source changed or failed during hash")
	}
	if copied > maxSourceFileBytes || openedAfter.Size() > maxSourceFileBytes {
		return contracts.ContentHash{}, nil, fmt.Errorf("source exceeds one GiB hash bound")
	}
	if !os.SameFile(before, openedAfter) || !os.SameFile(before, pathAfter) ||
		before.Size() != openedAfter.Size() || !before.ModTime().Equal(openedAfter.ModTime()) {
		return contracts.ContentHash{}, nil, fmt.Errorf("source changed during hash")
	}
	return digestFromHasher(hasher), openedAfter, nil
}

func revalidateGraphFiles(root, filter string, files []GraphFile) error {
	capturedPaths := make(map[string]struct{}, len(files))
	for _, captured := range files {
		capturedPaths[captured.Path] = struct{}{}
		current, info, err := hashWorkspaceFile(
			root,
			filepath.Join(root, filepath.FromSlash(captured.Path)),
		)
		if err != nil {
			return fmt.Errorf("project brain revalidate source %q: %w", captured.Path, err)
		}
		if current != captured.Hash || uint64(info.Size()) != captured.SizeBytes {
			return fmt.Errorf("project brain source changed during capture")
		}
	}
	scope := root
	if filter != "" {
		scope = filepath.Join(root, filepath.FromSlash(filter))
	}
	if err := filepath.WalkDir(scope, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != scope && skipSourceDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("project brain source symlink is forbidden: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if isSourceFile(relative) {
			if _, exists := capturedPaths[relative]; !exists {
				return fmt.Errorf("project brain source set changed during capture")
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("project brain revalidate source membership: %w", err)
	}
	return nil
}

func revalidateExactGraphFiles(root string, files []GraphFile) error {
	for _, captured := range files {
		current, info, err := hashWorkspaceFile(
			root,
			filepath.Join(root, filepath.FromSlash(captured.Path)),
		)
		if err != nil {
			return fmt.Errorf("project brain revalidate exact source %q: %w", captured.Path, err)
		}
		if current != captured.Hash || uint64(info.Size()) != captured.SizeBytes {
			return fmt.Errorf("project brain exact source changed during capture")
		}
	}
	return nil
}

func digestGraphGeneration(
	exportDigest [sha256.Size]byte,
	stats cgStats,
	root contracts.ContentHash,
	files []GraphFile,
) (contracts.ContentHash, error) {
	hasher := sha256Writer()
	header := strings.Join([]string{
		hex.EncodeToString(exportDigest[:]),
		fmt.Sprintf("%d", stats.FilesCount),
		fmt.Sprintf("%d", stats.TotalNodes),
		fmt.Sprintf("%d", stats.TotalEdges),
		root.Digest,
	}, "\x00")
	if _, err := io.WriteString(hasher, header+"\n"); err != nil {
		return contracts.ContentHash{}, err
	}
	for _, file := range files {
		if _, err := io.WriteString(
			hasher,
			file.Path+"\x00"+file.Language+"\x00"+
				fmt.Sprintf("%d", file.NodeCount)+"\x00"+file.Hash.Digest+"\n",
		); err != nil {
			return contracts.ContentHash{}, err
		}
	}
	return digestFromHasher(hasher), nil
}

func validateFilter(filter string) error {
	if filter == "" {
		return nil
	}
	return validateRelativePath(filepath.ToSlash(filter))
}

func skipSourceDirectory(name string) bool {
	switch name {
	case ".git", ".cg", ".codegraph", ".venv", "venv", "node_modules", ".next", "dist", "build", "vendor":
		return true
	default:
		return false
	}
}

func isSourceFile(path string) bool {
	if isSourceManifest(path) {
		return true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".py", ".rs", ".java", ".c", ".h", ".cpp", ".hpp", ".sql":
		return true
	default:
		return false
	}
}

func isSourceManifest(path string) bool {
	switch strings.ToLower(filepath.Base(path)) {
	case "cargo.lock", "cargo.toml", "deno.json", "deno.lock", "go.mod", "go.sum",
		"package-lock.json", "package.json", "pipfile", "pipfile.lock", "pnpm-lock.yaml",
		"poetry.lock", "pyproject.toml", "requirements.txt", "uv.lock", "yarn.lock":
		return true
	default:
		return false
	}
}

func sourceLanguage(path string) string {
	switch strings.ToLower(filepath.Base(path)) {
	case "go.mod", "go.sum":
		return "go"
	case "package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock",
		"deno.json", "deno.lock":
		return "javascript"
	case "cargo.toml", "cargo.lock":
		return "rust"
	case "pyproject.toml", "poetry.lock", "uv.lock", "requirements.txt",
		"pipfile", "pipfile.lock":
		return "python"
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".sql":
		return "sql"
	default:
		return "other"
	}
}
