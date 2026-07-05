// Package extract turns Go source into the canonical CodeGraph model via
// go/packages + go/types (type-resolved) and go/callgraph (resolved calls). It
// is the L1 extractor's Go tier: a pure function of source with no LLM or
// embedding dependency. All file paths it emits are repo-relative.
package extract

import (
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"

	"matrix/codegraph/merkle"
	"matrix/codegraph/model"
)

const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedDeps | packages.NeedTypes | packages.NeedSyntax |
	packages.NeedTypesInfo | packages.NeedModule

// Config describes an extraction run.
type Config struct {
	RepoRoot string   // absolute repo root; emitted file paths are relative to it
	RepoName string   // e.g. "matrix"
	Modules  []string // absolute module directories (each holds a go.mod)
}

// Extractor accumulates a single graph across one or more module loads.
type Extractor struct {
	cfg      Config
	ix       *model.Index
	fileText map[string][]byte // abs file path -> bytes (for digests)
	modules  map[string]string // module path -> repo-relative dir
	pkgOf    map[string]string // node id -> owning package path (shard routing)
}

// New returns an Extractor writing into a fresh Index.
func New(cfg Config) *Extractor {
	return &Extractor{
		cfg:      cfg,
		ix:       model.NewIndex(),
		fileText: map[string][]byte{},
		modules:  map[string]string{},
		pkgOf:    map[string]string{},
	}
}

// PackageOf reports the owning package path of a node id, or "" for the repo and
// module container nodes (which belong in the manifest).
func (e *Extractor) PackageOf(id string) string { return e.pkgOf[id] }

// Modules returns the extracted module path -> repo-relative dir map.
func (e *Extractor) Modules() map[string]string { return e.modules }

// Index returns the accumulated graph.
func (e *Extractor) Index() *model.Index { return e.ix }

// Run loads every configured module and extracts nodes and all edge tiers into
// the shared Index. Container digests and the repo node are finalized last.
func (e *Extractor) Run() error {
	e.ix.AddNode(&model.Node{
		Id: "repo:" + e.cfg.RepoName, Kind: model.KindRepo,
		Name: e.cfg.RepoName, QName: e.cfg.RepoName, Lang: "go", Exported: true,
	})
	for _, dir := range e.cfg.Modules {
		if err := e.loadModule(dir); err != nil {
			return err
		}
	}
	e.finalizeContainers()
	return nil
}

func (e *Extractor) loadModule(dir string) error {
	cfg := &packages.Config{Mode: loadMode, Dir: dir}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return fmt.Errorf("load %s: %w", dir, err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		return fmt.Errorf("load %s: %d package errors", dir, n)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].PkgPath < pkgs[j].PkgPath })

	var modPath, modDir string
	for _, p := range pkgs {
		if p.Module != nil && modPath == "" {
			modPath = p.Module.Path
			modDir = p.Module.Dir
		}
	}
	moduleID := ""
	if modPath != "" {
		moduleID = "mod:" + modPath
		e.modules[modPath] = e.rel(modDir)
		e.ix.AddNode(&model.Node{
			Id: moduleID, Kind: model.KindModule, Name: pathLeaf(modPath), QName: modPath,
			Lang: "go", File: e.rel(modDir), Exported: true,
		})
		e.ix.AddEdge(model.Edge{Src: "repo:" + e.cfg.RepoName, Dst: moduleID, Type: model.EdgeContains})
	}

	for _, p := range pkgs {
		e.extractPackage(p, moduleID)
	}
	// Second and third tiers reuse the loaded, type-checked packages.
	e.extractCalls(pkgs)
	e.extractTypeEdges(pkgs)
	return nil
}

func (e *Extractor) extractPackage(p *packages.Package, moduleID string) {
	if p.Types == nil {
		return
	}
	pkgDir := ""
	if len(p.GoFiles) > 0 {
		pkgDir = e.rel(filepath.Dir(p.GoFiles[0]))
	}
	e.ix.AddNode(&model.Node{
		Id: p.PkgPath, Kind: model.KindPackage, Name: p.Name, QName: p.PkgPath,
		Lang: "go", File: pkgDir, Exported: true,
	})
	e.pkgOf[p.PkgPath] = p.PkgPath
	if moduleID != "" {
		e.ix.AddEdge(model.Edge{Src: moduleID, Dst: p.PkgPath, Type: model.EdgeContains})
	}

	// intra-repo imports
	imports := make([]string, 0, len(p.Imports))
	for path := range p.Imports {
		imports = append(imports, path)
	}
	sort.Strings(imports)
	for _, path := range imports {
		if e.isRepoPkg(path) {
			e.ix.AddEdge(model.Edge{Src: p.PkgPath, Dst: path, Type: model.EdgeImports})
		}
	}

	for _, f := range p.Syntax {
		e.extractFile(p, f)
	}
}

func (e *Extractor) extractFile(p *packages.Package, f *ast.File) {
	pos := p.Fset.Position(f.Pos())
	abs := pos.Filename
	src := e.readFile(abs)
	relPath := e.rel(abs)
	fileID := p.PkgPath + "/" + filepath.Base(abs)
	lineCount := 1
	if tf := p.Fset.File(f.Pos()); tf != nil {
		lineCount = tf.LineCount()
	}
	e.ix.AddNode(&model.Node{
		Id: fileID, Kind: model.KindFile, Name: filepath.Base(abs), QName: relPath,
		Lang: "go", File: relPath, Range: model.Range{StartLine: 1, EndLine: lineCount},
		Digest: model.Digest(src), Exported: true,
	})
	e.pkgOf[fileID] = p.PkgPath
	e.ix.AddEdge(model.Edge{Src: p.PkgPath, Dst: fileID, Type: model.EdgeContains})

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			e.extractFunc(p, relPath, src, fileID, d)
		case *ast.GenDecl:
			e.extractGenDecl(p, relPath, src, fileID, d)
		}
	}
}

func (e *Extractor) extractFunc(p *packages.Package, relPath string, src []byte, fileID string, d *ast.FuncDecl) {
	obj := p.TypesInfo.Defs[d.Name]
	if obj == nil {
		return
	}
	id, kind, ok := idForObj(obj)
	if !ok {
		return
	}
	rng, br := span(p.Fset, docPos(d.Doc), d.Pos(), d.End())
	n := &model.Node{
		Id: id, Kind: kind, Name: d.Name.Name, QName: id, Lang: "go",
		File: relPath, Range: rng, ByteRange: br,
		Digest: model.Digest(slice(src, br)), Sig: funcSig(p.Fset, d),
		Exported: obj.Exported(), Doc: docText(d.Doc),
	}
	e.ix.AddNode(n)
	e.pkgOf[id] = p.PkgPath
	e.ix.AddEdge(model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
	if kind == model.KindMethod {
		if recv := methodRecvID(obj); recv != "" {
			e.ix.AddEdge(model.Edge{Src: recv, Dst: id, Type: model.EdgeContains})
		}
	}
	e.references(p, id, d)
}

func (e *Extractor) extractGenDecl(p *packages.Package, relPath string, src []byte, fileID string, d *ast.GenDecl) {
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			e.extractType(p, relPath, src, fileID, d, s)
		case *ast.ValueSpec:
			for _, name := range s.Names {
				e.extractValue(p, relPath, src, fileID, d, s, name)
			}
		}
	}
}

func (e *Extractor) extractType(p *packages.Package, relPath string, src []byte, fileID string, d *ast.GenDecl, s *ast.TypeSpec) {
	obj := p.TypesInfo.Defs[s.Name]
	if obj == nil {
		return
	}
	id, kind, ok := idForObj(obj)
	if !ok {
		return
	}
	doc := s.Doc
	if doc == nil {
		doc = d.Doc
	}
	rng, br := span(p.Fset, docPos(doc), specStart(d, s), s.End())
	n := &model.Node{
		Id: id, Kind: kind, Name: s.Name.Name, QName: id, Lang: "go",
		File: relPath, Range: rng, ByteRange: br,
		Digest: model.Digest(slice(src, br)), Sig: typeSig(p.Fset, s),
		Exported: obj.Exported(), Doc: docText(doc),
	}
	e.ix.AddNode(n)
	e.pkgOf[id] = p.PkgPath
	e.ix.AddEdge(model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
	e.references(p, id, s)

	// struct fields become field nodes contained by the type
	if st, ok := s.Type.(*ast.StructType); ok && st.Fields != nil {
		for _, field := range st.Fields.List {
			for _, fname := range field.Names {
				fobj := p.TypesInfo.Defs[fname]
				if fobj == nil {
					continue
				}
				frng, fbr := span(p.Fset, docPos(field.Doc), field.Pos(), field.End())
				fid := id + "#" + fname.Name
				e.ix.AddNode(&model.Node{
					Id: fid, Kind: model.KindField, Name: fname.Name, QName: fid, Lang: "go",
					File: relPath, Range: frng, ByteRange: fbr,
					Digest: model.Digest(slice(src, fbr)), Exported: fobj.Exported(),
					Sig: typeString(fobj.Type()), Doc: docText(field.Doc),
				})
				e.pkgOf[fid] = p.PkgPath
				e.ix.AddEdge(model.Edge{Src: id, Dst: fid, Type: model.EdgeContains})
			}
		}
	}
}

func (e *Extractor) extractValue(p *packages.Package, relPath string, src []byte, fileID string, d *ast.GenDecl, s *ast.ValueSpec, name *ast.Ident) {
	obj := p.TypesInfo.Defs[name]
	if obj == nil {
		return
	}
	id, kind, ok := idForObj(obj)
	if !ok {
		return
	}
	doc := s.Doc
	if doc == nil {
		doc = d.Doc
	}
	rng, br := span(p.Fset, docPos(doc), name.Pos(), specValueEnd(s, name))
	n := &model.Node{
		Id: id, Kind: kind, Name: name.Name, QName: id, Lang: "go",
		File: relPath, Range: rng, ByteRange: br,
		Digest: model.Digest(slice(src, br)), Exported: obj.Exported(),
		Sig: typeString(obj.Type()), Doc: docText(doc),
	}
	e.ix.AddNode(n)
	e.pkgOf[id] = p.PkgPath
	e.ix.AddEdge(model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
	e.references(p, id, s)
}

// references walks a declaration subtree and emits a references edge to every
// intra-repo named object it uses (type-resolved via TypesInfo.Uses), excluding
// self-references.
func (e *Extractor) references(p *packages.Package, srcID string, node ast.Node) {
	seen := map[string]struct{}{}
	ast.Inspect(node, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj := p.TypesInfo.Uses[id]
		if obj == nil || obj.Pkg() == nil || !e.isRepoPkg(obj.Pkg().Path()) {
			return true
		}
		did, _, ok := idForObj(obj)
		if !ok || did == srcID {
			return true
		}
		if _, dup := seen[did]; dup {
			return true
		}
		seen[did] = struct{}{}
		e.ix.AddEdge(model.Edge{Src: srcID, Dst: did, Type: model.EdgeReferences})
		return true
	})
}

// finalizeContainers assigns each container (package, module) a digest derived
// from its children's digests, so a container's digest moves iff a descendant
// changed — deterministic and independent of any enrichment.
func (e *Extractor) finalizeContainers() {
	// Roll digests bottom-up: files already have content digests; packages hash
	// their files, then modules hash their packages, then the repo hashes modules.
	for _, kind := range []model.Kind{model.KindPackage, model.KindModule, model.KindRepo} {
		for _, n := range e.ix.Nodes() {
			if n.Kind == kind {
				n.Digest = e.childDigest(n.Id)
			}
		}
	}
}

func (e *Extractor) childDigest(id string) string {
	kids := e.ix.Forward(id, model.EdgeContains)
	parts := make([]string, 0, len(kids))
	for _, k := range kids {
		if child := e.ix.Node(k); child != nil {
			parts = append(parts, child.Digest)
		}
	}
	sort.Strings(parts)
	return model.Digest([]byte(strings.Join(parts, "\n")))
}

// FileMerkle builds the repo file Merkle tree over the canonical source-file set
// (see RepoFiles), so the persisted tree is identical whether produced by a full
// build or recomputed by an incremental update. On walk error it falls back to
// exactly the files the extractor consumed.
func (e *Extractor) FileMerkle() *merkle.Tree {
	if t, err := FileTree(e.cfg); err == nil {
		return t
	}
	leaves := make(map[string]string, len(e.fileText))
	for abs, b := range e.fileText {
		if b == nil {
			continue
		}
		leaves[e.rel(abs)] = merkle.LeafHash(b)
	}
	return merkle.FromLeaves(leaves)
}

// Merkle returns a deterministic repo root over all (id, digest) pairs.
func (e *Extractor) Merkle() string {
	nodes := e.ix.Nodes()
	var b strings.Builder
	for _, n := range nodes {
		b.WriteString(n.Id)
		b.WriteByte(' ')
		b.WriteString(n.Digest)
		b.WriteByte('\n')
	}
	return model.Digest([]byte(b.String()))
}

// --- helpers ---

func (e *Extractor) isRepoPkg(path string) bool {
	return path == e.cfg.RepoName || strings.HasPrefix(path, e.cfg.RepoName+"/")
}

func (e *Extractor) rel(abs string) string {
	r, err := filepath.Rel(e.cfg.RepoRoot, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(r)
}

func (e *Extractor) readFile(abs string) []byte {
	if b, ok := e.fileText[abs]; ok {
		return b
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		b = nil
	}
	e.fileText[abs] = b
	return b
}

func idForObj(obj types.Object) (string, model.Kind, bool) {
	if obj == nil || obj.Pkg() == nil {
		return "", "", false
	}
	p := obj.Pkg().Path()
	switch o := obj.(type) {
	case *types.Func:
		if sig, ok := o.Type().(*types.Signature); ok && sig.Recv() != nil {
			recv := recvTypeName(sig.Recv().Type())
			if recv == "" {
				return "", "", false
			}
			return p + "." + recv + "." + o.Name(), model.KindMethod, true
		}
		return p + "." + o.Name(), model.KindFunc, true
	case *types.TypeName:
		if types.IsInterface(o.Type()) {
			return p + "." + o.Name(), model.KindInterface, true
		}
		return p + "." + o.Name(), model.KindType, true
	case *types.Const:
		return p + "." + o.Name(), model.KindConst, true
	case *types.Var:
		if o.IsField() {
			return "", "", false
		}
		return p + "." + o.Name(), model.KindVar, true
	}
	return "", "", false
}

func methodRecvID(obj types.Object) string {
	fn, ok := obj.(*types.Func)
	if !ok {
		return ""
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return ""
	}
	t := sig.Recv().Type()
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Pkg().Path() + "." + named.Obj().Name()
	}
	return ""
}

func recvTypeName(t types.Type) string {
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		return named.Obj().Name()
	}
	return ""
}

func docPos(cg *ast.CommentGroup) token.Pos {
	if cg == nil {
		return token.NoPos
	}
	return cg.Pos()
}

func docText(cg *ast.CommentGroup) string {
	if cg == nil {
		return ""
	}
	return strings.TrimRight(cg.Text(), "\n")
}

func span(fset *token.FileSet, docStart, start, end token.Pos) (model.Range, model.ByteRange) {
	from := start
	if docStart.IsValid() {
		from = docStart
	}
	ps := fset.Position(from)
	pe := fset.Position(end)
	return model.Range{StartLine: ps.Line, EndLine: pe.Line},
		model.ByteRange{StartByte: ps.Offset, EndByte: pe.Offset}
}

func slice(src []byte, br model.ByteRange) []byte {
	if src == nil || br.StartByte < 0 || br.EndByte > len(src) || br.StartByte > br.EndByte {
		return nil
	}
	return src[br.StartByte:br.EndByte]
}

func specStart(d *ast.GenDecl, s *ast.TypeSpec) token.Pos {
	if d.Lparen == token.NoPos {
		return d.Pos() // `type Foo ...` — include the `type` keyword
	}
	return s.Pos()
}

func specValueEnd(s *ast.ValueSpec, name *ast.Ident) token.Pos {
	if s.End().IsValid() {
		return s.End()
	}
	return name.End()
}

func funcSig(fset *token.FileSet, d *ast.FuncDecl) string {
	savedBody, savedDoc := d.Body, d.Doc
	d.Body, d.Doc = nil, nil
	var b strings.Builder
	_ = printer.Fprint(&b, fset, d)
	d.Body, d.Doc = savedBody, savedDoc
	return strings.TrimSpace(b.String())
}

func typeSig(fset *token.FileSet, s *ast.TypeSpec) string {
	var b strings.Builder
	b.WriteString("type ")
	b.WriteString(s.Name.Name)
	b.WriteByte(' ')
	_ = printer.Fprint(&b, fset, s.Type)
	return strings.TrimSpace(b.String())
}

func typeString(t types.Type) string {
	if t == nil {
		return ""
	}
	return types.TypeString(t, func(p *types.Package) string { return p.Name() })
}

func pathLeaf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
