// A package comment, then a blank line before the imports and one after them:
// neither is inside the import block.
package clean

// The first import's doc comment sits above the block.
import "fmt"
// A comment between two imports is not a blank line.
import "os"
import str "strings" // a trailing comment
/* a block comment
   spanning lines */
import _ "unsafe"

var _ = fmt.Sprint
var _ = os.Args
var _ = str.ToLower

func f() {

    _ = 1
}
