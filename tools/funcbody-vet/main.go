// funcbody-vet is a go vet tool enforcing how this repo writes function bodies:
// no blank line and no comment anywhere inside one. What a comment would have
// explained belongs in the doc comment above the function instead.
//
//	go build -o /tmp/funcbody-vet ./tools/funcbody-vet/ && go vet -vettool=/tmp/funcbody-vet ./...
//
// A function literal is part of the body it sits in, so a closure's comment is
// reported once, and a literal outside any function — a package-level var —
// is a body of its own. A blank line inside a multi-line raw string belongs to
// the string, and one inside a comment to the comment, which is reported
// already. Each run of blank lines is reported once, at its first line, and
// generated files are skipped.
//
// Lines are counted unadjusted, so a //line directive cannot move a line away
// from the source text it is checked against.
package main

import "go/ast"
import "go/token"
import "strings"
import "golang.org/x/tools/go/analysis"
import "golang.org/x/tools/go/analysis/unitchecker"

var analyzer = &analysis.Analyzer{
    Name: "funcbody",
    Doc:  "check that function bodies hold no blank lines and no comments",
    Run:  run,
}

func main() {
    unitchecker.Main(analyzer)
}

func run(pass *analysis.Pass) (any, error) {
    for _, f := range pass.Files {
        if ast.IsGenerated(f) { continue }
        var file = pass.Fset.File(f.Pos())
        var line = func(p token.Pos) int { return file.PositionFor(p, false).Line }
        var src, err = pass.ReadFile(file.Name())
        if err != nil { return nil, err }
        var text = strings.Split(string(src), "\n")
        var bodies []*ast.BlockStmt
        var skip = map[int]bool{}
        ast.Inspect(f, func(n ast.Node) bool {
            var body *ast.BlockStmt
            switch x := n.(type) {
            case *ast.FuncDecl:
                body = x.Body
            case *ast.FuncLit:
                body = x.Body
            case *ast.BasicLit:
                for l := line(x.Pos()); l <= line(x.End()); l++ { skip[l] = true }
            }
            if body != nil && (len(bodies) == 0 || body.Lbrace > bodies[len(bodies)-1].Rbrace) {
                bodies = append(bodies, body)
            }
            return true
        })
        for _, g := range f.Comments {
            for _, b := range bodies {
                if g.Pos() < b.Lbrace || g.Pos() > b.Rbrace { continue }
                pass.Reportf(g.Pos(), "comment inside a function: move it to the doc comment")
                for l := line(g.Pos()); l <= line(g.End()); l++ { skip[l] = true }
                break
            }
        }
        for _, b := range bodies {
            var inRun = false
            for l := line(b.Lbrace) + 1; l < line(b.Rbrace); l++ {
                var blank = !skip[l] && strings.TrimSpace(text[l-1]) == ""
                if blank && !inRun { pass.Reportf(file.LineStart(l), "blank line inside a function") }
                inRun = blank
            }
        }
    }
    return nil, nil
}
