package main

import "testing"
import "golang.org/x/tools/go/analysis/analysistest"

func TestImportsVet(t *testing.T) {
    analysistest.Run(t, analysistest.TestData(), analyzer, "clean", "grouped", "sameline", "blank", "generated")
}
