// Package prog is a self-contained fixture that exercises every structural edge
// kind for the extractor's correctness tests.
package prog

// Greeter greets by name.
type Greeter interface {
	Hello(name string) string
}

// base carries a shared greeting prefix.
type base struct {
	Prefix string
}

// EnglishGreeter greets in English. It embeds base and implements Greeter.
type EnglishGreeter struct {
	base
}

// Hello implements Greeter for EnglishGreeter.
func (g EnglishGreeter) Hello(name string) string {
	return g.Prefix + "Hello, " + name
}

// DefaultPrefix is the default greeting prefix.
const DefaultPrefix = ">> "

// NewEnglish builds an EnglishGreeter with the default prefix.
func NewEnglish() EnglishGreeter {
	return EnglishGreeter{base: base{Prefix: DefaultPrefix}}
}

// Run greets everyone, defaulting the greeter when nil.
func Run(g Greeter, names []string) []string {
	if g == nil {
		g = NewEnglish()
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, g.Hello(n))
	}
	return out
}
