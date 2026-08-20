// Package typescript provides regex-based extraction for TypeScript/JavaScript source files.
// It extracts classes, functions, interfaces, types, imports, exports, and call references.
package typescript

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"centra/protocol/codegraph-ultra/internal/extract"
	"centra/protocol/codegraph-ultra/internal/model"
)

func init() {
	extract.Register("typescript", func() extract.Extractor { return &tsExtractor{} })
}

// tsExtractor adapts the package-level Extract function to the Extractor interface.
type tsExtractor struct{}

func (t *tsExtractor) Extract(cfg extract.Config) (*extract.Result, error) {
	r, err := Extract(Config{
		RepoRoot: cfg.RepoRoot,
		RepoName: cfg.RepoName,
		Paths:    cfg.Modules,
	})
	if err != nil {
		return nil, err
	}
	return &extract.Result{Nodes: r.Nodes, Edges: r.Edges}, nil
}

var (
	reClass     = regexp.MustCompile(`^(export\s+)?(?:abstract\s+)?class\s+(\w+)(?:<[^>]*>)?(?:\s+extends\s+(\w+))?(?:\s+implements\s+([^{]+))?`)
	reInterface = regexp.MustCompile(`^(export\s+)?interface\s+(\w+)(?:<[^>]*>)?(?:\s+extends\s+([^{]+))?`)
	reType      = regexp.MustCompile(`^(export\s+)?type\s+(\w+)`)
	reFunc      = regexp.MustCompile(`^(export\s+)?(?:async\s+)?function\s+(\w+)\s*(?:<[^>]*>)?\s*\(([^)]*)\)`)
	reMethod    = regexp.MustCompile(`^\s+(?:static\s+)?(?:async\s+)?(?:get\s+|set\s+)?(\w+)\s*(?:<[^>]*>)?\s*\(([^)]*)\)`)
	reArrow     = regexp.MustCompile(`^(export\s+)?(?:const|let|var)\s+(\w+)\s*(?::\s*[^=]+)?\s*=\s*(?:async\s+)?\(`)
	reImport    = regexp.MustCompile(`import\s+(?:(\w+)\s*(?:,\s*\{([^}]+)\})?|(?:\{([^}]+)\})|(?:\*\s+as\s+(\w+)))\s+from\s+['"]([^'"]+)['"]`)
	reCallExpr  = regexp.MustCompile(`(\w+(?:\.\w+)*)\s*\(`)
	reEnum      = regexp.MustCompile(`^(export\s+)?(?:const\s+)?enum\s+(\w+)`)
)

// Config for TypeScript extraction.
type Config struct {
	RepoRoot string
	RepoName string
	Paths    []string
}

// Result holds extracted graph elements.
type Result struct {
	Nodes []*model.Node
	Edges []model.Edge
}

// Extract parses TypeScript/JavaScript files and returns graph nodes and edges.
func Extract(cfg Config) (*Result, error) {
	r := &Result{}
	if len(cfg.Paths) == 0 {
		cfg.Paths = []string{cfg.RepoRoot}
	}

	files, err := findTSFiles(cfg.Paths)
	if err != nil {
		return nil, err
	}

	repoID := "repo:" + cfg.RepoName
	r.Nodes = append(r.Nodes, &model.Node{
		ID:       repoID,
		Kind:     model.KindRepo,
		Name:     cfg.RepoName,
		QName:    cfg.RepoName,
		Lang:     "typescript",
		Exported: true,
	})

	packages := map[string]bool{}

	for _, f := range files {
		rel, _ := filepath.Rel(cfg.RepoRoot, f)
		rel = filepath.ToSlash(rel)
		content, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := strings.Split(string(content), "\n")

		// Determine package from directory
		dir := filepath.Dir(rel)
		pkgID := strings.ReplaceAll(dir, "/", ".")
		if pkgID == "." {
			pkgID = cfg.RepoName
		} else {
			pkgID = cfg.RepoName + "." + pkgID
		}

		if !packages[pkgID] {
			packages[pkgID] = true
			r.Nodes = append(r.Nodes, &model.Node{
				ID:       pkgID,
				Kind:     model.KindPackage,
				Name:     filepath.Base(dir),
				QName:    pkgID,
				Lang:     "typescript",
				Exported: true,
			})
			r.Edges = append(r.Edges, model.Edge{
				Src:  repoID,
				Dst:  pkgID,
				Type: model.EdgeContains,
			})
		}

		lang := "typescript"
		if strings.HasSuffix(f, ".js") || strings.HasSuffix(f, ".jsx") {
			lang = "javascript"
		}

		fileID := pkgID + "/" + filepath.Base(rel)
		r.Nodes = append(r.Nodes, &model.Node{
			ID:       fileID,
			Kind:     model.KindFile,
			Name:     filepath.Base(rel),
			QName:    rel,
			Lang:     lang,
			File:     rel,
			Range:    model.Range{StartLine: 1, EndLine: len(lines)},
			Digest:   model.Digest(content),
			Exported: true,
		})
		r.Edges = append(r.Edges, model.Edge{
			Src:  pkgID,
			Dst:  fileID,
			Type: model.EdgeContains,
		})

		parseTSFile(r, lines, fileID, pkgID, rel, lang)
	}

	return r, nil
}

func parseTSFile(r *Result, lines []string, fileID, pkgID, rel, lang string) {
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineno := i + 1

		// Skip comments
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || trimmed == "" {
			continue
		}

		// Imports
		if m := reImport.FindStringSubmatch(trimmed); m != nil {
			modPath := m[5]
			if isLocalImport(modPath) {
				r.Edges = append(r.Edges, model.Edge{
					Src:  fileID,
					Dst:  resolveLocalImport(modPath, pkgID),
					Type: model.EdgeImports,
				})
			}
			continue
		}

		// Enum
		if m := reEnum.FindStringSubmatch(trimmed); m != nil {
			name := m[2]
			id := pkgID + "." + name
			r.Nodes = append(r.Nodes, &model.Node{
				ID:       id,
				Kind:     model.KindEnum,
				Name:     name,
				QName:    id,
				Lang:     lang,
				File:     rel,
				Range:    model.Range{StartLine: lineno, EndLine: lineno},
				Exported: !strings.HasPrefix(name, "_"),
			})
			r.Edges = append(r.Edges, model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
			continue
		}

		// Interface
		if m := reInterface.FindStringSubmatch(trimmed); m != nil {
			name := m[2]
			id := pkgID + "." + name
			r.Nodes = append(r.Nodes, &model.Node{
				ID:       id,
				Kind:     model.KindInterface,
				Name:     name,
				QName:    id,
				Lang:     lang,
				File:     rel,
				Range:    model.Range{StartLine: lineno, EndLine: lineno},
				Exported: !strings.HasPrefix(name, "_"),
			})
			r.Edges = append(r.Edges, model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
			if m[3] != "" {
				for _, ext := range strings.Split(m[3], ",") {
					ext = strings.TrimSpace(ext)
					if ext != "" {
						r.Edges = append(r.Edges, model.Edge{Src: id, Dst: ext, Type: model.EdgeInherits})
					}
				}
			}
			continue
		}

		// Type alias
		if m := reType.FindStringSubmatch(trimmed); m != nil {
			name := m[2]
			id := pkgID + "." + name
			r.Nodes = append(r.Nodes, &model.Node{
				ID:       id,
				Kind:     model.KindType,
				Name:     name,
				QName:    id,
				Lang:     lang,
				File:     rel,
				Range:    model.Range{StartLine: lineno, EndLine: lineno},
				Exported: !strings.HasPrefix(name, "_"),
			})
			r.Edges = append(r.Edges, model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
			continue
		}

		// Class
		if m := reClass.FindStringSubmatch(trimmed); m != nil {
			name := m[2]
			id := pkgID + "." + name
			r.Nodes = append(r.Nodes, &model.Node{
				ID:       id,
				Kind:     model.KindClass,
				Name:     name,
				QName:    id,
				Lang:     lang,
				File:     rel,
				Range:    model.Range{StartLine: lineno, EndLine: lineno},
				Exported: !strings.HasPrefix(name, "_"),
			})
			r.Edges = append(r.Edges, model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
			if m[3] != "" {
				r.Edges = append(r.Edges, model.Edge{Src: id, Dst: strings.TrimSpace(m[3]), Type: model.EdgeInherits})
			}
			if m[4] != "" {
				for _, impl := range strings.Split(m[4], ",") {
					impl = strings.TrimSpace(impl)
					if impl != "" {
						r.Edges = append(r.Edges, model.Edge{Src: id, Dst: impl, Type: model.EdgeImplements})
					}
				}
			}
			continue
		}

		// Exported function
		if m := reFunc.FindStringSubmatch(trimmed); m != nil {
			name := m[2]
			id := pkgID + "." + name
			sig := "function " + name + "(" + m[3] + ")"
			r.Nodes = append(r.Nodes, &model.Node{
				ID:       id,
				Kind:     model.KindFunc,
				Name:     name,
				QName:    id,
				Lang:     lang,
				File:     rel,
				Range:    model.Range{StartLine: lineno, EndLine: lineno},
				Sig:      sig,
				Exported: !strings.HasPrefix(name, "_"),
			})
			r.Edges = append(r.Edges, model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
			continue
		}

		// Arrow function export
		if m := reArrow.FindStringSubmatch(trimmed); m != nil {
			name := m[2]
			id := pkgID + "." + name
			r.Nodes = append(r.Nodes, &model.Node{
				ID:       id,
				Kind:     model.KindFunc,
				Name:     name,
				QName:    id,
				Lang:     lang,
				File:     rel,
				Range:    model.Range{StartLine: lineno, EndLine: lineno},
				Exported: !strings.HasPrefix(name, "_"),
			})
			r.Edges = append(r.Edges, model.Edge{Src: fileID, Dst: id, Type: model.EdgeDefines})
			continue
		}
	}
}

func findTSFiles(dirs []string) ([]string, error) {
	var files []string
	exts := map[string]bool{".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true}
	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				base := info.Name()
				if base == ".git" || base == "node_modules" || base == "dist" || base == "build" || base == ".next" {
					return filepath.SkipDir
				}
				return nil
			}
			if exts[filepath.Ext(path)] {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

func isLocalImport(path string) bool {
	return strings.HasPrefix(path, ".") || strings.HasPrefix(path, "@/")
}

func resolveLocalImport(path, pkgID string) string {
	// Remove relative prefix and normalize
	path = strings.TrimLeft(path, "./")
	path = strings.TrimPrefix(path, "@/")
	parts := strings.Split(pkgID, ".")
	if len(parts) > 1 {
		return strings.Join(parts[:len(parts)-1], ".") + "." + strings.ReplaceAll(path, "/", ".")
	}
	return path
}
