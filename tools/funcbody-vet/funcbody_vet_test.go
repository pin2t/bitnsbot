package main

import "testing"
import "golang.org/x/tools/go/analysis/analysistest"

func TestFuncbodyVet(t *testing.T) {
    analysistest.Run(t, analysistest.TestData(), analyzer, "clean", "blank", "comments", "generated")
}
