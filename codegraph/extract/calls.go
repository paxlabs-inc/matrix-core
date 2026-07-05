package extract

import (
	"sort"
	"strconv"

	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"matrix/codegraph/model"
)

// extractCalls derives resolved call edges. It builds SSA over the loaded
// packages and runs VTA (seeded by a CHA call graph) so each calls edge names
// the actual resolved callee, not a name match. Both endpoints must be
// intra-repo; the retained site is the deterministically smallest call location.
func (e *Extractor) extractCalls(pkgs []*packages.Package) {
	prog, _ := ssautil.Packages(pkgs, ssa.BuilderMode(0))
	if prog == nil {
		return
	}
	prog.Build()

	cg := vta.CallGraph(ssautil.AllFunctions(prog), cha.CallGraph(prog))
	if cg == nil {
		return
	}

	type key struct{ src, dst string }
	sites := map[key][]string{}
	for fn, node := range cg.Nodes {
		srcID, ok := funcID(fn)
		if !ok || !e.isRepoPkg(pkgPathOf(fn)) {
			continue
		}
		for _, edge := range node.Out {
			callee := edge.Callee.Func
			dstID, ok := funcID(callee)
			if !ok || dstID == srcID || !e.isRepoPkg(pkgPathOf(callee)) {
				continue
			}
			k := key{srcID, dstID}
			if edge.Site != nil {
				pos := prog.Fset.Position(edge.Site.Pos())
				sites[k] = append(sites[k], e.rel(pos.Filename)+":"+strconv.Itoa(pos.Line))
			} else if _, ok := sites[k]; !ok {
				sites[k] = nil
			}
		}
	}

	keys := make([]key, 0, len(sites))
	for k := range sites {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].src != keys[j].src {
			return keys[i].src < keys[j].src
		}
		return keys[i].dst < keys[j].dst
	})
	for _, k := range keys {
		ss := sites[k]
		sort.Strings(ss)
		site := ""
		if len(ss) > 0 {
			site = ss[0]
		}
		e.ix.AddEdge(model.Edge{Src: k.src, Dst: k.dst, Type: model.EdgeCalls, Site: site})
	}
}

func funcID(fn *ssa.Function) (string, bool) {
	if fn == nil {
		return "", false
	}
	obj := fn.Object()
	if obj == nil {
		return "", false
	}
	id, _, ok := idForObj(obj)
	return id, ok
}

func pkgPathOf(fn *ssa.Function) string {
	if fn == nil {
		return ""
	}
	if obj := fn.Object(); obj != nil && obj.Pkg() != nil {
		return obj.Pkg().Path()
	}
	return ""
}
