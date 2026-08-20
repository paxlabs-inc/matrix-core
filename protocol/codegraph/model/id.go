package model

import (
	"encoding/hex"
	"strconv"

	"lukechampine.com/blake3"
)

// String renders a range as "start:end" (1-indexed, inclusive).
func (r Range) String() string {
	return strconv.Itoa(r.StartLine) + ":" + strconv.Itoa(r.EndLine)
}

// Id builds the stable logical id for a node. The id is a pure function of the
// symbol's place in the naming hierarchy — never of its body — so it survives
// edits to the body (only the digest moves). The id also never reads enrichment.
//
//	repo                       -> repo:<name>
//	module                     -> <module>                 (the go module path)
//	package                    -> <pkg>                     (the import path)
//	file                       -> <pkg>/<name>             (name = base file name)
//	func                       -> <pkg>.<name>
//	method                     -> <pkg>.<recv>.<name>      (recv = receiver type name)
//	type/interface/const/var   -> <pkg>.<name>
//	field                      -> <recv>#<name>            (recv = owner type's id)
func Id(kind Kind, module, pkg, recv, name, file string, r Range) string {
	switch kind {
	case KindRepo:
		return "repo:" + name
	case KindModule:
		return module
	case KindPackage:
		return pkg
	case KindFile:
		return pkg + "/" + name
	case KindMethod:
		return pkg + "." + recv + "." + name
	case KindField:
		return recv + "#" + name
	case KindFunc, KindType, KindInterface, KindConst, KindVar:
		return pkg + "." + name
	default:
		return pkg + "." + name
	}
}

// Disambiguator is the deterministic suffix applied when two nodes would
// otherwise share an id: the first 8 hex of blake3(file + "|" + range).
func Disambiguator(file string, r Range) string {
	sum := blake3.Sum256([]byte(file + "|" + r.String()))
	return hex.EncodeToString(sum[:])[:8]
}

// Disambiguate appends "~<disambiguator>" to a colliding id.
func Disambiguate(id, file string, r Range) string {
	return id + "~" + Disambiguator(file, r)
}
