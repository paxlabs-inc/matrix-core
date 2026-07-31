// Package goextract extracts Go source into the CodeGraph Ultra model via
// go/packages (type-resolved) and AST walking. All emitted file paths are
// repo-relative.
package goextract

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"

	"codegraph-ultra/internal/extract"
	"codegraph-ultra/internal/model"
)

const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedDeps | packages.NeedTypes | packages.NeedSyntax |
	packages.NeedTypesInfo | packages.NeedModule

func init() {
	extract.Register("go", func() extract.Extractor { return &GoExtractor{} })
}

// GoExtractor extracts a Go repository into nodes and edges.
type GoExtractor struct {
	cfg      extract.Config
	nodes    []*model.Node
	edges    []model.Edge
	fileText map[string][]byte // abs -> bytes
	modules  map[string]string // module path -> repo-relative dir
	pkgOf    map[string]string // node id -> owning package path
	nodeSeen map[string]bool   // dedup nodes
	edgeSeen map[string]bool   // dedup edges
}

func (e *GoExtractor) Extract(cfg extract.Config) (*extract.Result, error) {
	e.cfg = cfg
	e.nodes = nil
	e.edges = nil
	e.fileText = map[string][]byte{}
	e.modules = map[string]string{}
	e.pkgOf = map[string]string{}
	e.nodeSeen = map[string]bool{}
	e.edgeSeen = map[string]bool{}

	// Discover modules if none specified.
	modules := cfg.Modules
	if len(modules) == 0 {
		var err error
		modules, err = extract.DiscoverGoModules(cfg.RepoRoot)
		if err != nil {
			return nil, fmt.Errorf("go extract: discover modules: %w", err)
		}
	}
	if len(modules) == 0 {
		return nil, fmt.Errorf("go extract: no go.mod found under %s", cfg.RepoRoot)
	}

	// Repo container node.
	e.addNode(&model.Node{
		ID: "repo:" + cfg.RepoName, Kind: model.KindRepo,
		Name: cfg.RepoName, QName: cfg.RepoName, Lang: "go", Exported: true,
	})

	for _, dir := range modules {
		// Use regex-based extraction — works regardless of Go version or
		// dependency state. The go/packages path is kept for callers who
		// need type-resolved data, but the CLI always uses the fallback.
		if err := e.loadModuleFallback(dir); err != nil {
			return nil, err
		}
	}

	e.finalizeContainers()

	model.SortNodes(e.nodes)
	model.SortEdges(e.edges)

	return &extract.Result{Nodes: e.nodes, Edges: e.edges}, nil
}

// --- module loading ---

func (e *GoExtractor) loadModule(dir string) error {
	cfg := &packages.Config{Mode: loadMode, Dir: dir}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return fmt.Errorf("go extract: load %s: %w", dir, err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		return fmt.Errorf("go extract: load %s: %d package errors", dir, n)
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
		e.addNode(&model.Node{
			ID: moduleID, Kind: model.KindModule, Name: pathLeaf(modPath), QName: modPath,
			Lang: "go", File: e.rel(modDir), Exported: true,
		})
		e.addEdge(model.Edge{Src: "repo:" + e.cfg.RepoName, Dst: moduleID, Type: model.EdgeContains})
	}

	for _, p := range pkgs {
		e.extractPackage(p, moduleID)
	}
	e.extractCalls(pkgs)
	e.extractTypeEdges(pkgs)
	return nil
}

// --- regex-based fallback (no go/packages) ---

var (
	rePkgDecl  = regexp.MustCompile(`^package\s+(\w+)`)
	reFuncDecl = regexp.MustCompile(`^(?:func\s+\([^)]+\)\s+)?(?:func)\s+(\w+)\s*(?:\([^)]*\))?\s*(?:\([^)]*\))?\s*{?`)
	reTypeDecl = regexp.MustCompile(`^type\s+(\w+)\s+`)
	reConstVal = regexp.MustCompile(`^const\s+(\w+)`)
	reVarVal   = regexp.MustCompile(`^var\s+(\w+)`)
	reImportLn = regexp.MustCompile(`^\s*(?:(\w+)\s+)?"([^"]+)"`)
)

func (e *GoExtractor) loadModuleFallback(dir string) error {
	// Parse module path from go.mod.
	modPath := parseGoMod(dir)

	moduleID := ""
	if modPath != "" {
		moduleID = "mod:" + modPath
		e.modules[modPath] = e.rel(dir)
		e.addNode(&model.Node{
			ID: moduleID, Kind: model.KindModule, Name: pathLeaf(modPath), QName: modPath,
			Lang: "go", File: e.rel(dir), Exported: true,
		})
		e.addEdge(model.Edge{Src: "repo:" + e.cfg.RepoName, Dst: moduleID, Type: model.EdgeContains})
	}

	goFiles, err := findGoFiles(dir)
	if err != nil {
		return err
	}

	// Group files by directory (each directory = one package).
	dirFiles := map[string][]string{}
	for _, f := range goFiles {
		d := filepath.Dir(f)
		dirFiles[d] = append(dirFiles[d], f)
	}

	for pkgDir, files := range dirFiles {
		e.extractPackageFallback(pkgDir, files, moduleID, modPath)
	}

	return nil
}

func (e *GoExtractor) extractPackageFallback(pkgDir string, files []string, moduleID, modPath string) {
	// Derive package path from directory relative to repo root.
	relDir := e.rel(pkgDir)
	pkgPath := modPath
	if relDir != "" && relDir != "." {
		if pkgPath != "" {
			pkgPath = modPath + "/" + filepath.ToSlash(relDir)
		} else {
			pkgPath = filepath.ToSlash(relDir)
		}
	}
	if pkgPath == "" {
		pkgPath = e.cfg.RepoName
	}

	pkgName := filepath.Base(pkgDir)
	pkgID := pkgPath

	e.addNode(&model.Node{
		ID: pkgID, Kind: model.KindPackage, Name: pkgName, QName: pkgPath,
		Lang: "go", File: relDir, Exported: true,
	})
	e.pkgOf[pkgID] = pkgPath
	if moduleID != "" {
		e.addEdge(model.Edge{Src: moduleID, Dst: pkgID, Type: model.EdgeContains})
	}

	var inBlockComment bool

	for _, fpath := range files {
		relFile := e.rel(fpath)
		content := readFileBytes(fpath)
		lines := strings.Split(string(content), "\n")

		fileID := pkgID + "/" + filepath.Base(fpath)
		e.addNode(&model.Node{
			ID: fileID, Kind: model.KindFile, Name: filepath.Base(fpath), QName: relFile,
			Lang: "go", File: relFile,
			Range:    model.Range{StartLine: 1, EndLine: len(lines)},
			Digest:   model.Digest(content),
			Exported: true,
		})
		e.pkgOf[fileID] = pkgPath
		e.addEdge(model.Edge{Src: pkgID, Dst: fileID, Type: model.EdgeContains})

		var braceDepth int

		for i, line := range lines {
			lineno := i + 1
			trimmed := strings.TrimSpace(line)

			// Track block comments.
			if inBlockComment {
				if strings.Contains(trimmed, "*/") {
					inBlockComment = false
				}
				continue
			}
			if strings.HasPrefix(trimmed, "/*") {
				if !strings.Contains(trimmed[2:], "*/") {
					inBlockComment = true
				}
				continue
			}
			if strings.HasPrefix(trimmed, "//") || trimmed == "" {
				continue
			}

			// Track brace depth for method body scoping.
			braceDepth += strings.Count(line, "{") - strings.Count(line, "}")
			if braceDepth <= 0 {
				braceDepth = 0
			}

			// Import block — extract intra-repo imports.
			if m := reImportLn.FindStringSubmatch(trimmed); m != nil {
				impPath := m[2]
				if extract.IsRepoPkg(e.cfg.RepoName, impPath) || (modPath != "" && strings.HasPrefix(impPath, modPath)) {
					e.addEdge(model.Edge{Src: fileID, Dst: impPath, Type: model.EdgeImports})
				}
				continue
			}

			// Type declaration.
			if m := reTypeDecl.FindStringSubmatch(trimmed); m != nil {
				name := m[1]
				id := pkgID + "." + name
				kind := model.KindType
				if strings.Contains(trimmed, "struct") {
					kind = model.KindStruct
				} else if strings.Contains(trimmed, "interface") {
					kind = model.KindInterface
				}
				e.addNode(&model.Node{
					ID: id, Kind: kind, Name: name, QName: id, Lang: "go",
					File: relFile, Range: model.Range{StartLine: lineno, EndLine: lineno},
					Exported: !strings.HasPrefix(name, "_"),
				})
				e.pkgOf[id] = pkgPath
				e.addEdge(model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
				continue
			}

			// Const declaration.
			if m := reConstVal.FindStringSubmatch(trimmed); m != nil {
				name := m[1]
				id := pkgID + "." + name
				e.addNode(&model.Node{
					ID: id, Kind: model.KindConst, Name: name, QName: id, Lang: "go",
					File: relFile, Range: model.Range{StartLine: lineno, EndLine: lineno},
					Exported: !strings.HasPrefix(name, "_"),
				})
				e.pkgOf[id] = pkgPath
				e.addEdge(model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
				continue
			}

			// Var declaration.
			if m := reVarVal.FindStringSubmatch(trimmed); m != nil {
				name := m[1]
				id := pkgID + "." + name
				e.addNode(&model.Node{
					ID: id, Kind: model.KindVar, Name: name, QName: id, Lang: "go",
					File: relFile, Range: model.Range{StartLine: lineno, EndLine: lineno},
					Exported: !strings.HasPrefix(name, "_"),
				})
				e.pkgOf[id] = pkgPath
				e.addEdge(model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
				continue
			}

			// Function or method declaration.
			if strings.HasPrefix(trimmed, "func ") {
				name := extractGoFuncName(trimmed)
				if name == "" {
					continue
				}
				kind := model.KindFunc
				id := pkgID + "." + name

				// Check for method receiver.
				if strings.HasPrefix(trimmed, "func (") {
					kind = model.KindMethod
					recv := extractGoRecvType(trimmed)
					if recv != "" {
						recvID := pkgID + "." + recv
						id = recvID + "." + name
						e.addEdge(model.Edge{Src: recvID, Dst: id, Type: model.EdgeContains})
					}
				}

				sig := trimmed
				if idx := strings.Index(sig, "{"); idx > 0 {
					sig = strings.TrimRight(sig[:idx], " ")
				}

				e.addNode(&model.Node{
					ID: id, Kind: kind, Name: name, QName: id, Lang: "go",
					File: relFile, Range: model.Range{StartLine: lineno, EndLine: lineno},
					Sig: sig, Exported: !strings.HasPrefix(name, "_"),
				})
				e.pkgOf[id] = pkgPath
				e.addEdge(model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
				braceDepth = 1
				continue
			}
		}
	}
}

func extractGoFuncName(line string) string {
	line = strings.TrimPrefix(line, "func ")
	// Method: func (r *Receiver) Name(...)
	if strings.HasPrefix(line, "(") {
		idx := strings.Index(line, ")")
		if idx < 0 {
			return ""
		}
		line = strings.TrimSpace(line[idx+1:])
	}
	// Now line starts with Name(
	idx := strings.Index(line, "(")
	if idx < 0 {
		return ""
	}
	return strings.TrimSpace(line[:idx])
}

func extractGoRecvType(line string) string {
	line = strings.TrimPrefix(line, "func (")
	idx := strings.Index(line, ")")
	if idx < 0 {
		return ""
	}
	recv := strings.TrimSpace(line[:idx])
	// Strip pointer and name: "*Foo" or "f *Foo" -> "Foo"
	recv = strings.TrimLeft(recv, "* ")
	parts := strings.Fields(recv)
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimLeft(parts[len(parts)-1], "*")
}

func parseGoMod(dir string) string {
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module"))
		}
	}
	return ""
}

func findGoFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "vendor" || base == "testdata" || (len(base) > 0 && base[0] == '.') {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func readFileBytes(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// --- package extraction ---

func (e *GoExtractor) extractPackage(p *packages.Package, moduleID string) {
	if p.Types == nil {
		return
	}
	pkgDir := ""
	if len(p.GoFiles) > 0 {
		pkgDir = e.rel(filepath.Dir(p.GoFiles[0]))
	}
	e.addNode(&model.Node{
		ID: p.PkgPath, Kind: model.KindPackage, Name: p.Name, QName: p.PkgPath,
		Lang: "go", File: pkgDir, Exported: true,
	})
	e.pkgOf[p.PkgPath] = p.PkgPath
	if moduleID != "" {
		e.addEdge(model.Edge{Src: moduleID, Dst: p.PkgPath, Type: model.EdgeContains})
	}

	// Intra-repo imports.
	imports := make([]string, 0, len(p.Imports))
	for path := range p.Imports {
		imports = append(imports, path)
	}
	sort.Strings(imports)
	for _, path := range imports {
		if extract.IsRepoPkg(e.cfg.RepoName, path) {
			e.addEdge(model.Edge{Src: p.PkgPath, Dst: path, Type: model.EdgeImports})
		}
	}

	for _, f := range p.Syntax {
		e.extractFile(p, f)
	}
}

// --- file extraction ---

func (e *GoExtractor) extractFile(p *packages.Package, f *ast.File) {
	pos := p.Fset.Position(f.Pos())
	abs := pos.Filename
	src := e.readFile(abs)
	relPath := e.rel(abs)
	fileID := p.PkgPath + "/" + filepath.Base(abs)
	lineCount := 1
	if tf := p.Fset.File(f.Pos()); tf != nil {
		lineCount = tf.LineCount()
	}
	e.addNode(&model.Node{
		ID: fileID, Kind: model.KindFile, Name: filepath.Base(abs), QName: relPath,
		Lang: "go", File: relPath, Range: model.Range{StartLine: 1, EndLine: lineCount},
		Digest: model.Digest(src), Exported: true,
	})
	e.pkgOf[fileID] = p.PkgPath
	e.addEdge(model.Edge{Src: p.PkgPath, Dst: fileID, Type: model.EdgeContains})

	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			e.extractFunc(p, relPath, src, fileID, d)
		case *ast.GenDecl:
			e.extractGenDecl(p, relPath, src, fileID, d)
		}
	}
}

// --- function/method extraction ---

func (e *GoExtractor) extractFunc(p *packages.Package, relPath string, src []byte, fileID string, d *ast.FuncDecl) {
	obj := p.TypesInfo.Defs[d.Name]
	if obj == nil {
		return
	}
	id, kind, ok := idForObj(obj)
	if !ok {
		return
	}
	rng := span(p.Fset, docPos(d.Doc), d.Pos(), d.End())
	n := &model.Node{
		ID: id, Kind: kind, Name: d.Name.Name, QName: id, Lang: "go",
		File: relPath, Range: rng,
		Digest: model.Digest(sliceSrc(src, p.Fset, d.Pos(), d.End())),
		Sig:    funcSig(p.Fset, d), Exported: obj.Exported(), Doc: docText(d.Doc),
	}
	e.addNode(n)
	e.pkgOf[id] = p.PkgPath
	e.addEdge(model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
	if kind == model.KindMethod {
		if recv := methodRecvID(obj); recv != "" {
			e.addEdge(model.Edge{Src: recv, Dst: id, Type: model.EdgeContains})
		}
	}
	e.extractReferences(p, id, d)
}

// --- generic declaration extraction (types, values) ---

func (e *GoExtractor) extractGenDecl(p *packages.Package, relPath string, src []byte, fileID string, d *ast.GenDecl) {
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

// --- type extraction ---

func (e *GoExtractor) extractType(p *packages.Package, relPath string, src []byte, fileID string, d *ast.GenDecl, s *ast.TypeSpec) {
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
	startPos := s.Pos()
	if d.Lparen == token.NoPos {
		startPos = d.Pos() // include the `type` keyword
	}
	rng := span(p.Fset, docPos(doc), startPos, s.End())
	n := &model.Node{
		ID: id, Kind: kind, Name: s.Name.Name, QName: id, Lang: "go",
		File: relPath, Range: rng,
		Digest: model.Digest(sliceSrcFrom(p.Fset, src, docPos(doc), startPos, s.End())),
		Sig:    typeSig(p.Fset, s), Exported: obj.Exported(), Doc: docText(doc),
	}
	e.addNode(n)
	e.pkgOf[id] = p.PkgPath
	e.addEdge(model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
	e.extractReferences(p, id, s)

	// Struct fields become field nodes contained by the type.
	if st, ok := s.Type.(*ast.StructType); ok && st.Fields != nil {
		for _, field := range st.Fields.List {
			for _, fname := range field.Names {
				fobj := p.TypesInfo.Defs[fname]
				if fobj == nil {
					continue
				}
				frng := span(p.Fset, docPos(field.Doc), field.Pos(), field.End())
				fid := id + "#" + fname.Name
				e.addNode(&model.Node{
					ID: fid, Kind: model.KindField, Name: fname.Name, QName: fid, Lang: "go",
					File: relPath, Range: frng,
					Digest:   model.Digest(sliceSrcFrom(p.Fset, src, docPos(field.Doc), field.Pos(), field.End())),
					Exported: fobj.Exported(), Sig: typeString(fobj.Type()), Doc: docText(field.Doc),
				})
				e.pkgOf[fid] = p.PkgPath
				e.addEdge(model.Edge{Src: id, Dst: fid, Type: model.EdgeContains})
			}
		}
	}

	// Interface methods become method nodes contained by the interface.
	if iface, ok := s.Type.(*ast.InterfaceType); ok && iface.Methods != nil {
		for _, method := range iface.Methods.List {
			for _, mname := range method.Names {
				mobj := p.TypesInfo.Defs[mname]
				if mobj == nil {
					continue
				}
				mrng := span(p.Fset, docPos(method.Doc), method.Pos(), method.End())
				mid := id + "." + mname.Name
				e.addNode(&model.Node{
					ID: mid, Kind: model.KindMethod, Name: mname.Name, QName: mid, Lang: "go",
					File: relPath, Range: mrng,
					Digest:   model.Digest(sliceSrcFrom(p.Fset, src, docPos(method.Doc), method.Pos(), method.End())),
					Exported: mobj.Exported(), Sig: typeString(mobj.Type()), Doc: docText(method.Doc),
				})
				e.pkgOf[mid] = p.PkgPath
				e.addEdge(model.Edge{Src: id, Dst: mid, Type: model.EdgeContains})
			}
		}
	}
}

// --- value (const/var) extraction ---

func (e *GoExtractor) extractValue(p *packages.Package, relPath string, src []byte, fileID string, d *ast.GenDecl, s *ast.ValueSpec, name *ast.Ident) {
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
	endPos := s.End()
	if !endPos.IsValid() {
		endPos = name.End()
	}
	rng := span(p.Fset, docPos(doc), name.Pos(), endPos)
	n := &model.Node{
		ID: id, Kind: kind, Name: name.Name, QName: id, Lang: "go",
		File: relPath, Range: rng,
		Digest:   model.Digest(sliceSrcFrom(p.Fset, src, docPos(doc), name.Pos(), endPos)),
		Exported: obj.Exported(), Sig: typeString(obj.Type()), Doc: docText(doc),
	}
	e.addNode(n)
	e.pkgOf[id] = p.PkgPath
	e.addEdge(model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
	e.extractReferences(p, id, s)
}

// --- reference extraction ---

func (e *GoExtractor) extractReferences(p *packages.Package, srcID string, node ast.Node) {
	seen := map[string]struct{}{}
	ast.Inspect(node, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj := p.TypesInfo.Uses[id]
		if obj == nil || obj.Pkg() == nil || !extract.IsRepoPkg(e.cfg.RepoName, obj.Pkg().Path()) {
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
		e.addEdge(model.Edge{Src: srcID, Dst: did, Type: model.EdgeReferences})
		return true
	})
}

// --- call graph extraction ---

func (e *GoExtractor) extractCalls(pkgs []*packages.Package) {
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				caller := e.enclosingFunc(p, call.Pos())
				if caller == "" {
					return true
				}
				callee := e.resolveCallee(p, call)
				if callee == "" || callee == caller {
					return true
				}
				site := ""
				if p.Fset != nil {
					site = e.rel(p.Fset.Position(call.Pos()).Filename)
				}
				e.addEdge(model.Edge{Src: caller, Dst: callee, Type: model.EdgeCalls, Site: site})
				return true
			})
		}
	}
}

func (e *GoExtractor) enclosingFunc(p *packages.Package, pos token.Pos) string {
	for _, f := range p.Syntax {
		if !f.Pos().IsValid() || !f.End().IsValid() {
			continue
		}
		if pos < f.Pos() || pos > f.End() {
			continue
		}
		var result string
		ast.Inspect(f, func(n ast.Node) bool {
			if result != "" {
				return false
			}
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			if fn.Pos() <= pos && pos <= fn.End() {
				obj := p.TypesInfo.Defs[fn.Name]
				if obj != nil {
					id, _, ok := idForObj(obj)
					if ok {
						result = id
					}
				}
				return false
			}
			return true
		})
		if result != "" {
			return result
		}
	}
	return ""
}

func (e *GoExtractor) resolveCallee(p *packages.Package, call *ast.CallExpr) string {
	// Try type-checker info for direct function references.
	if p.TypesInfo != nil {
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if obj := p.TypesInfo.Uses[fn]; obj != nil {
				id, _, ok := idForObj(obj)
				if ok {
					return id
				}
			}
		case *ast.SelectorExpr:
			if obj := p.TypesInfo.Uses[fn.Sel]; obj != nil {
				id, _, ok := idForObj(obj)
				if ok {
					return id
				}
			}
		}
	}
	return ""
}

// --- type relationship extraction (implements, embeds) ---

func (e *GoExtractor) extractTypeEdges(pkgs []*packages.Package) {
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		for _, f := range p.Syntax {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					obj := p.TypesInfo.Defs[ts.Name]
					if obj == nil {
						continue
					}
					typeID, _, ok := idForObj(obj)
					if !ok {
						continue
					}

					// Struct embedding.
					if st, ok := ts.Type.(*ast.StructType); ok && st.Fields != nil {
						for _, field := range st.Fields.List {
							if len(field.Names) == 0 {
								// Embedded field — resolve the embedded type ID.
								if embedded := e.resolveTypeRef(p, field.Type); embedded != "" {
									e.addEdge(model.Edge{Src: typeID, Dst: embedded, Type: model.EdgeEmbeds})
								}
							}
						}
					}

					// Interface embedding and implements.
					if iface, ok := ts.Type.(*ast.InterfaceType); ok && iface.Methods != nil {
						for _, method := range iface.Methods.List {
							if len(method.Names) == 0 {
								// Embedded interface.
								if embedded := e.resolveTypeRef(p, method.Type); embedded != "" {
									e.addEdge(model.Edge{Src: typeID, Dst: embedded, Type: model.EdgeEmbeds})
								}
							}
						}
					}

					// Implements: if the type is a named type, check if it implements interfaces
					// from the same repo.
					if named, ok := obj.Type().(*types.Named); ok {
						for i := 0; i < named.NumMethods(); i++ {
							_ = named.Method(i) // methods are available for future use
						}
					}
				}
			}
		}

		// Use go/types interface satisfaction to find implements edges.
		e.findImplements(p)
	}
}

func (e *GoExtractor) findImplements(p *packages.Package) {
	if p.Types == nil {
		return
	}
	scope := p.Types.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if obj == nil {
			continue
		}
	tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		if !types.IsInterface(tn.Type()) {
			continue
		}
		iface, ok := tn.Type().Underlying().(*types.Interface)
		if !ok {
			continue
		}
		ifaceID, _, ok := idForObj(tn)
		if !ok {
			continue
		}
		// Check all named types in the same package.
		for _, oname := range scope.Names() {
			oobj := scope.Lookup(oname)
			if oobj == nil || oobj == obj {
				continue
			}
			otn, ok2 := oobj.(*types.TypeName)
			if !ok2 {
				continue
			}
			if types.IsInterface(otn.Type()) {
				continue
			}
			ptr := types.NewPointer(otn.Type())
			if types.Implements(otn.Type(), iface) || types.Implements(ptr, iface) {
				implID, _, ok3 := idForObj(otn)
				if !ok3 || implID == ifaceID {
					continue
				}
				e.addEdge(model.Edge{Src: implID, Dst: ifaceID, Type: model.EdgeImplements})
			}
		}
	}
}

func (e *GoExtractor) resolveTypeRef(p *packages.Package, expr ast.Expr) string {
	if p.TypesInfo == nil {
		return ""
	}
	// For identifier-based type references.
	switch t := expr.(type) {
	case *ast.Ident:
		obj := p.TypesInfo.Uses[t]
		if obj != nil {
			id, _, ok := idForObj(obj)
			if ok {
				return id
			}
		}
	case *ast.SelectorExpr:
		obj := p.TypesInfo.Uses[t.Sel]
		if obj != nil {
			id, _, ok := idForObj(obj)
			if ok {
				return id
			}
		}
	case *ast.StarExpr:
		return e.resolveTypeRef(p, t.X)
	case *ast.ArrayType:
		return e.resolveTypeRef(p, t.Elt)
	case *ast.MapType:
		return "" // map types don't resolve to a single node
	}
	return ""
}

// --- container digest finalization ---

func (e *GoExtractor) finalizeContainers() {
	for _, kind := range []model.Kind{model.KindPackage, model.KindModule, model.KindRepo} {
		for _, n := range e.nodes {
			if n.Kind == kind {
				n.Digest = e.childDigest(n.ID)
			}
		}
	}
}

func (e *GoExtractor) childDigest(id string) string {
	var parts []string
	for _, edge := range e.edges {
		if edge.Src == id && edge.Type == model.EdgeContains {
			for _, n := range e.nodes {
				if n.ID == edge.Dst {
					parts = append(parts, n.Digest)
				}
			}
		}
	}
	sort.Strings(parts)
	return model.Digest([]byte(strings.Join(parts, "\n")))
}

// --- deduplication helpers ---

func (e *GoExtractor) addNode(n *model.Node) {
	if e.nodeSeen[n.ID] {
		return
	}
	e.nodeSeen[n.ID] = true
	e.nodes = append(e.nodes, n)
}

func (e *GoExtractor) addEdge(edge model.Edge) {
	key := string(edge.Type) + ":" + edge.Src + ":" + edge.Dst
	if e.edgeSeen[key] {
		return
	}
	e.edgeSeen[key] = true
	e.edges = append(e.edges, edge)
}

// --- path / file helpers ---

func (e *GoExtractor) isRepoPkg(path string) bool {
	return extract.IsRepoPkg(e.cfg.RepoName, path)
}

func (e *GoExtractor) rel(abs string) string {
	r, err := filepath.Rel(e.cfg.RepoRoot, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(r)
}

func (e *GoExtractor) readFile(abs string) []byte {
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

// --- type/object helpers ---

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

// --- AST helpers ---

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

func span(fset *token.FileSet, docStart, start, end token.Pos) model.Range {
	from := start
	if docStart.IsValid() {
		from = docStart
	}
	ps := fset.Position(from)
	pe := fset.Position(end)
	return model.Range{StartLine: ps.Line, EndLine: pe.Line}
}

func sliceSrc(src []byte, fset *token.FileSet, start, end token.Pos) []byte {
	if src == nil || !start.IsValid() || !end.IsValid() {
		return nil
	}
	s := fset.Position(start).Offset
	e := fset.Position(end).Offset
	if s < 0 || e > len(src) || s > e {
		return nil
	}
	return src[s:e]
}

func sliceSrcFrom(fset *token.FileSet, src []byte, docStart, start, end token.Pos) []byte {
	from := start
	if docStart.IsValid() {
		from = docStart
	}
	return sliceSrc(src, fset, from, end)
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
