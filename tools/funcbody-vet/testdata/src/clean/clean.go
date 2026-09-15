// A package comment, and blank lines between declarations, are outside every
// function.
package clean

import "strings"

// A doc comment is outside the function it documents.
func f(a int) int {
    var s = `a raw string

keeps its blank line`
    return a + len(strings.TrimSpace(s))
} // a comment after the closing brace is outside too

var g = func() int {
    return f(1)
}

func h() {}

func i() { _ = g() }

type t struct{}

func (t) m() int {
    return 1
}

type u interface {
    // An interface method has no body.
    m() int
}

// A signature spread over lines is not the body.
func j(
    a int,

    b int,
) int {
    return a + b
}
