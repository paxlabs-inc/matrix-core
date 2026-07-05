// Command codegraph builds the CodeGraph graph store for a repository and runs
// the retrieval tools against it.
//
//	codegraph build     [-root DIR] [-name NAME] [-out DIR] [-modules a,b,...]
//	codegraph lookup    [build flags] -name-of SYMBOL [-kind KIND]
//	codegraph neighbors [build flags] -id NODEID [-edge TYPE] [-dir out|in|both] [-depth N]
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"matrix/codegraph/extract"
	codegraphmcp "matrix/codegraph/mcp"
	"matrix/codegraph/model"
	"matrix/codegraph/retrieve"
	"matrix/codegraph/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "build":
		err = cmdBuild(os.Args[2:])
	case "lookup":
		err = cmdLookup(os.Args[2:])
	case "neighbors":
		err = cmdNeighbors(os.Args[2:])
	case "impact":
		err = cmdImpact(os.Args[2:])
	case "diff":
		err = cmdDiff(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "mcp":
		err = cmdMCP(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "codegraph:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: codegraph <build|lookup|neighbors|impact|diff|check|mcp> [flags]")
}

type common struct {
	root    string
	name    string
	out     string
	modules string
}

func (c *common) bind(fs *flag.FlagSet) {
	fs.StringVar(&c.root, "root", ".", "repo root")
	fs.StringVar(&c.name, "name", "", "repo name (default: base of root)")
	fs.StringVar(&c.out, "out", "", "graph output dir (default: <root>/graph)")
	fs.StringVar(&c.modules, "modules", "", "comma-separated module dirs (default: auto-discover)")
}

func (c *common) config() (extract.Config, error) {
	root, err := filepath.Abs(c.root)
	if err != nil {
		return extract.Config{}, err
	}
	name := c.name
	if name == "" {
		name = filepath.Base(root)
	}
	var mods []string
	if c.modules != "" {
		for _, m := range strings.Split(c.modules, ",") {
			mods = append(mods, filepath.Join(root, strings.TrimSpace(m)))
		}
	} else {
		mods, err = discoverModules(root)
		if err != nil {
			return extract.Config{}, err
		}
	}
	return extract.Config{RepoRoot: root, RepoName: name, Modules: mods}, nil
}

func (c *common) graphDir(cfg extract.Config) string {
	if c.out != "" {
		return c.out
	}
	return filepath.Join(cfg.RepoRoot, "graph")
}

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	var c common
	c.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := c.config()
	if err != nil {
		return err
	}
	e, merkle, err := extract.Build(cfg)
	if err != nil {
		return err
	}
	out := c.graphDir(cfg)
	if err := e.WriteStore(out, merkle); err != nil {
		return err
	}
	fmt.Printf("built %d nodes across %d modules -> %s (merkle=%s)\n",
		e.Index().Len(), len(cfg.Modules), out, merkle)
	return nil
}

func cmdLookup(args []string) error {
	fs := flag.NewFlagSet("lookup", flag.ExitOnError)
	var c common
	c.bind(fs)
	sym := fs.String("name-of", "", "symbol name to look up")
	kind := fs.String("kind", "", "restrict to a node kind")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ix, err := buildIndex(&c)
	if err != nil {
		return err
	}
	fmt.Print(retrieve.New(ix).SymbolLookup(*sym, model.Kind(*kind)))
	return nil
}

func cmdNeighbors(args []string) error {
	fs := flag.NewFlagSet("neighbors", flag.ExitOnError)
	var c common
	c.bind(fs)
	id := fs.String("id", "", "node id")
	edge := fs.String("edge", "", "edge type (default: all)")
	dir := fs.String("dir", "both", "out|in|both")
	depth := fs.Int("depth", 1, "traversal depth (<=3)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ix, err := buildIndex(&c)
	if err != nil {
		return err
	}
	frag, err := retrieve.New(ix).Neighbors(*id, model.EdgeType(*edge), parseDir(*dir), *depth)
	if err != nil {
		return err
	}
	fmt.Print(frag)
	return nil
}

func cmdImpact(args []string) error {
	fs := flag.NewFlagSet("impact", flag.ExitOnError)
	var c common
	c.bind(fs)
	id := fs.String("id", "", "node id whose change impact to assess")
	depth := fs.Int("max-depth", 0, "closure depth bound (0 = full closure)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ix, err := buildIndex(&c)
	if err != nil {
		return err
	}
	fmt.Print(retrieve.New(ix).Impact(*id, *depth))
	return nil
}

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	a := fs.String("a", "", "base graph store dir (rev_a)")
	b := fs.String("b", "", "new graph store dir (rev_b)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *a == "" || *b == "" {
		return fmt.Errorf("diff: both -a and -b store dirs are required")
	}
	ixA, _, err := store.Load(*a)
	if err != nil {
		return fmt.Errorf("load a: %w", err)
	}
	ixB, _, err := store.Load(*b)
	if err != nil {
		return fmt.Errorf("load b: %w", err)
	}
	fmt.Print(retrieve.Diff(ixA, ixB))
	return nil
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	var c common
	c.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := c.config()
	if err != nil {
		return err
	}
	graphDir := c.graphDir(cfg)
	stale, changes, err := extract.CheckStale(cfg, graphDir)
	if err != nil {
		return err
	}
	if stale {
		for _, p := range changes.Added {
			fmt.Fprintf(os.Stderr, "  + %s\n", p)
		}
		for _, p := range changes.Changed {
			fmt.Fprintf(os.Stderr, "  ~ %s\n", p)
		}
		for _, p := range changes.Removed {
			fmt.Fprintf(os.Stderr, "  - %s\n", p)
		}
		return fmt.Errorf("graph store in %s is STALE relative to source; run: codegraph build", graphDir)
	}
	fmt.Println("codegraph: graph store is up to date")
	return nil
}

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	var c common
	c.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ix, err := buildIndex(&c)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "codegraph mcp: serving %d nodes over stdio\n", ix.Len())
	return codegraphmcp.New(ix).Serve(os.Stdin, os.Stdout)
}

func buildIndex(c *common) (*model.Index, error) {
	cfg, err := c.config()
	if err != nil {
		return nil, err
	}
	e, _, err := extract.Build(cfg)
	if err != nil {
		return nil, err
	}
	return e.Index(), nil
}

func parseDir(s string) model.Direction {
	switch s {
	case "out":
		return model.Out
	case "in":
		return model.In
	default:
		return model.Both
	}
}

func discoverModules(root string) ([]string, error) {
	var mods []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" {
			mods = append(mods, filepath.Dir(path))
		}
		return nil
	})
	sort.Strings(mods)
	return mods, err
}
