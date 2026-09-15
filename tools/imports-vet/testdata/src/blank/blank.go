package blank

import "fmt" // want +1 "blank line in the import block"

import "os" // want +3 "blank line in the import block"
// A comment does not excuse the blank lines after it, and a run of them is
// reported once.


import "strings"
import ( // want "grouped imports"
    "errors" // want +1 "blank line in the import block"

    "io"
)

var _ = fmt.Sprint
var _ = os.Args
var _ = strings.ToLower
var _ = errors.New
var _ = io.EOF
