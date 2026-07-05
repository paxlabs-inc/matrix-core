package extract

import (
	"go/types"
	"sort"

	"golang.org/x/tools/go/packages"

	"matrix/codegraph/model"
)

type namedType struct {
	id    string
	named *types.Named
}

// extractTypeEdges emits implements edges from the type checker (not name
// matching) and embeds edges for struct/interface embedding. Endpoints are
// restricted to intra-repo named types.
func (e *Extractor) extractTypeEdges(pkgs []*packages.Package) {
	var concretes, ifaces []namedType
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			tn, ok := scope.Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok {
				continue
			}
			id, _, ok := idForObj(tn)
			if !ok {
				continue
			}
			if types.IsInterface(tn.Type()) {
				ifaces = append(ifaces, namedType{id, named})
			} else {
				concretes = append(concretes, namedType{id, named})
			}
			e.embeds(id, named)
		}
	}
	sort.Slice(concretes, func(i, j int) bool { return concretes[i].id < concretes[j].id })
	sort.Slice(ifaces, func(i, j int) bool { return ifaces[i].id < ifaces[j].id })

	for _, c := range concretes {
		for _, i := range ifaces {
			if c.id == i.id {
				continue
			}
			iface, ok := i.named.Underlying().(*types.Interface)
			if !ok || iface.Empty() {
				continue
			}
			if types.Implements(c.named, iface) || types.Implements(types.NewPointer(c.named), iface) {
				e.ix.AddEdge(model.Edge{Src: c.id, Dst: i.id, Type: model.EdgeImplements})
			}
		}
	}
}

func (e *Extractor) embeds(id string, named *types.Named) {
	switch u := named.Underlying().(type) {
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			f := u.Field(i)
			if !f.Embedded() {
				continue
			}
			if did := e.namedID(f.Type()); did != "" && did != id {
				e.ix.AddEdge(model.Edge{Src: id, Dst: did, Type: model.EdgeEmbeds})
			}
		}
	case *types.Interface:
		for i := 0; i < u.NumEmbeddeds(); i++ {
			if did := e.namedID(u.EmbeddedType(i)); did != "" && did != id {
				e.ix.AddEdge(model.Edge{Src: id, Dst: did, Type: model.EdgeEmbeds})
			}
		}
	}
}

func (e *Extractor) namedID(t types.Type) string {
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		return ""
	}
	obj := named.Obj()
	if obj.Pkg() == nil || !e.isRepoPkg(obj.Pkg().Path()) {
		return ""
	}
	id, _, ok := idForObj(obj)
	if !ok {
		return ""
	}
	return id
}
