package grouped

import ( // want "grouped imports"
    "fmt"
    "os"
)
import ("strings") // want "grouped imports"

var _ = fmt.Sprint
var _ = os.Args
var _ = strings.ToLower
