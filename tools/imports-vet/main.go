// imports-vet is a go vet tool enforcing how this repo writes its imports: each
// one with its own import keyword, on its own line, and no blank line anywhere
// between the first import and the last.
//
//	go build -o /tmp/imports-vet ./tools/imports-vet/ && go vet -vettool=/tmp/imports-vet ./...
//
// A -vettool replaces vet's own analyzers rather than adding to them, so this
// runs as a second go vet beside the plain one, not instead of it.
//
// A comment between two imports is not a blank line. Generated files are
// skipped: their imports are written by whatever generated them.
package main

import "go/ast"
import "go/token"
import "golang.org/x/tools/go/analysis"
import "golang.org/x/tools/go/analysis/unitchecker"

var analyzer = &analysis.Analyzer{
    Name: "imports",
    Doc:  "check that every import has its own import keyword and line, with no blank lines between imports",
    Run:  run,
}

func main() {
    unitchecker.Main(analyzer)
}

// Unadjusted, so a //line directive cannot move a line the blank-line check counts.
func run(pass *analysis.Pass) (any, error) {
    for _, f := range pass.Files {
        if ast.IsGenerated(f) { continue }
        var file = pass.Fset.File(f.Pos())
        var line = func(p token.Pos) int { return file.PositionFor(p, false).Line }
        var used = map[int]bool{}
        var first, last = 0, 0
        for _, d := range f.Decls {
            var g, ok = d.(*ast.GenDecl)
            if !ok || g.Tok != token.IMPORT { break }
            if line(g.Pos()) == last {
                pass.Reportf(g.Pos(), "import on the same line as another: put each import on its own line")
            }
            if g.Lparen.IsValid() {
                pass.Reportf(g.Pos(), "grouped imports: give each import its own import keyword and line")
                used[line(g.Pos())] = true
                used[line(g.Lparen)] = true
                used[line(g.Rparen)] = true
                for _, s := range g.Specs {
                    for l := line(s.Pos()); l <= line(s.End()); l++ { used[l] = true }
                }
            } else {
                for l := line(g.Pos()); l <= line(g.End()); l++ { used[l] = true }
            }
            if first == 0 { first = line(g.Pos()) }
            last = line(g.End())
        }
        for _, c := range f.Comments {
            for l := line(c.Pos()); l <= line(c.End()); l++ { used[l] = true }
        }
        for l := first + 1; l < last; l++ {
            if !used[l] && used[l-1] {
                pass.Reportf(file.LineStart(l), "blank line in the import block")
            }
        }
    }
    return nil, nil
}
