// Package python provides regex-based extraction for Python source files.
// It extracts classes, functions, methods, imports, and call references.
package python

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"centra/protocol/codegraph-ultra/internal/extract"
	"centra/protocol/codegraph-ultra/internal/model"
)

func init() {
	extract.Register("python", func() extract.Extractor { return &pythonExtractor{} })
}

// pythonExtractor adapts the package-level Extract function to the Extractor interface.
type pythonExtractor struct{}

func (p *pythonExtractor) Extract(cfg extract.Config) (*extract.Result, error) {
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
	reClass    = regexp.MustCompile(`^class\s+(\w+)(?:\(([^)]*)\))?:`)
	reFunc     = regexp.MustCompile(`^(?:async\s+)?def\s+(\w+)\s*\(([^)]*)\)`)
	reImport   = regexp.MustCompile(`^import\s+(.+)`)
	reFrom     = regexp.MustCompile(`^from\s+([\w.]+)\s+import\s+(.+)`)
	reDecorator = regexp.MustCompile(`^@(\w+(?:\.\w+)*)`)
	reCall     = regexp.MustCompile(`(\w+(?:\.\w+)*)\s*\(`)
)

// Config for Python extraction.
type Config struct {
	RepoRoot string
	RepoName string
	Paths    []string // directories to scan
}

// Result holds extracted graph elements.
type Result struct {
	Nodes []*model.Node
	Edges []model.Edge
}

// Extract parses Python files and returns graph nodes and edges.
func Extract(cfg Config) (*Result, error) {
	r := &Result{}
	if len(cfg.Paths) == 0 {
		cfg.Paths = []string{cfg.RepoRoot}
	}

	files, err := findPythonFiles(cfg.Paths)
	if err != nil {
		return nil, err
	}

	// Add repo node
	repoID := "repo:" + cfg.RepoName
	r.Nodes = append(r.Nodes, &model.Node{
		ID:       repoID,
		Kind:     model.KindRepo,
		Name:     cfg.RepoName,
		QName:    cfg.RepoName,
		Lang:     "python",
		Exported: true,
	})

	// Track packages for containment
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

		// Add package node if new
		if !packages[pkgID] {
			packages[pkgID] = true
			r.Nodes = append(r.Nodes, &model.Node{
				ID:       pkgID,
				Kind:     model.KindPackage,
				Name:     filepath.Base(dir),
				QName:    pkgID,
				Lang:     "python",
				Exported: true,
			})
			r.Edges = append(r.Edges, model.Edge{
				Src:  repoID,
				Dst:  pkgID,
				Type: model.EdgeContains,
			})
		}

		// Add file node
		fileID := pkgID + "/" + filepath.Base(rel)
		r.Nodes = append(r.Nodes, &model.Node{
			ID:       fileID,
			Kind:     model.KindFile,
			Name:     filepath.Base(rel),
			QName:    rel,
			Lang:     "python",
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

		// Parse file
		var currentClass string
		var decorators []string

		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			lineno := i + 1

			// Collect decorators
			if m := reDecorator.FindStringSubmatch(trimmed); m != nil {
				decorators = append(decorators, m[1])
				continue
			}

			// Class definition
			if m := reClass.FindStringSubmatch(trimmed); m != nil {
				className := m[1]
				currentClass = className
				classID := pkgID + "." + className
				kind := model.KindClass
				if className == className && strings.Contains(trimmed, "Protocol") {
					kind = model.KindInterface
				}
				r.Nodes = append(r.Nodes, &model.Node{
					ID:       classID,
					Kind:     kind,
					Name:     className,
					QName:    classID,
					Lang:     "python",
					File:     rel,
					Range:    model.Range{StartLine: lineno, EndLine: lineno},
					Exported: !strings.HasPrefix(className, "_"),
				})
				r.Edges = append(r.Edges, model.Edge{
					Src:  fileID,
					Dst:  classID,
					Type: model.EdgeDefines,
				})

				// Handle inheritance
				if m[2] != "" {
					for _, parent := range strings.Split(m[2], ",") {
						parent = strings.TrimSpace(parent)
						if parent != "" && parent != "object" {
							r.Edges = append(r.Edges, model.Edge{
								Src:  classID,
								Dst:  parent,
								Type: model.EdgeInherits,
							})
						}
					}
				}
				decorators = nil
				continue
			}

			// Function/method definition
			if m := reFunc.FindStringSubmatch(trimmed); m != nil {
				funcName := m[1]
				var funcID string
				kind := model.KindFunc
				if currentClass != "" && line != trimmed {
					// Indented = method
					funcID = pkgID + "." + currentClass + "." + funcName
					kind = model.KindMethod
				} else {
					funcID = pkgID + "." + funcName
					if currentClass != "" && !strings.HasPrefix(line, " ") {
						currentClass = ""
					}
				}

				sig := "def " + funcName + "(" + m[2] + ")"
				r.Nodes = append(r.Nodes, &model.Node{
					ID:       funcID,
					Kind:     kind,
					Name:     funcName,
					QName:    funcID,
					Lang:     "python",
					File:     rel,
					Range:    model.Range{StartLine: lineno, EndLine: lineno},
					Sig:      sig,
					Exported: !strings.HasPrefix(funcName, "_"),
				})
				r.Edges = append(r.Edges, model.Edge{
					Src:  fileID,
					Dst:  funcID,
					Type: model.EdgeDefines,
				})
				if kind == model.KindMethod && currentClass != "" {
					r.Edges = append(r.Edges, model.Edge{
						Src:  pkgID + "." + currentClass,
						Dst:  funcID,
						Type: model.EdgeContains,
					})
				}
				decorators = nil
				continue
			}

			// Import statements
			if m := reImport.FindStringSubmatch(trimmed); m != nil {
				for _, imp := range strings.Split(m[1], ",") {
					imp = strings.TrimSpace(imp)
					if parts := strings.Split(imp, " as "); len(parts) > 1 {
						imp = strings.TrimSpace(parts[0])
					}
					if isLocalImport(imp, cfg.RepoName) {
						r.Edges = append(r.Edges, model.Edge{
							Src:  fileID,
							Dst:  resolveLocalImport(imp, cfg.RepoName),
							Type: model.EdgeImports,
						})
					}
				}
				decorators = nil
				continue
			}

			if m := reFrom.FindStringSubmatch(trimmed); m != nil {
				mod := m[1]
				if isLocalImport(mod, cfg.RepoName) {
					r.Edges = append(r.Edges, model.Edge{
						Src:  fileID,
						Dst:  resolveLocalImport(mod, cfg.RepoName),
						Type: model.EdgeImports,
					})
				}
				decorators = nil
				continue
			}

			// Reset current class if we hit a non-indented non-decorator line
			if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && trimmed != "" {
				if !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "@") {
					currentClass = ""
				}
			}

			// Clear decorators on non-decorator, non-definition lines
			if !strings.HasPrefix(trimmed, "@") {
				decorators = nil
			}
		}
	}

	return r, nil
}

func findPythonFiles(dirs []string) ([]string, error) {
	var files []string
	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				base := info.Name()
				if base == ".git" || base == "node_modules" || base == "__pycache__" || base == ".venv" || base == "venv" || base == ".tox" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".py") {
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

func isLocalImport(imp, repoName string) bool {
	return strings.HasPrefix(imp, repoName) || strings.HasPrefix(imp, ".")
}

func resolveLocalImport(imp, repoName string) string {
	imp = strings.TrimLeft(imp, ".")
	if !strings.HasPrefix(imp, repoName) {
		imp = repoName + "." + imp
	}
	return imp
}

// ScanImports extracts import lines from a Python file for quick dependency scanning.
func ScanImports(filePath string) ([]string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var imports []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if m := reFrom.FindStringSubmatch(line); m != nil {
			imports = append(imports, m[1])
		} else if m := reImport.FindStringSubmatch(line); m != nil {
			for _, imp := range strings.Split(m[1], ",") {
				imp = strings.TrimSpace(imp)
				if parts := strings.Split(imp, " as "); len(parts) > 1 {
					imp = strings.TrimSpace(parts[0])
				}
				imports = append(imports, imp)
			}
		}
	}
	return imports, scanner.Err()
}
