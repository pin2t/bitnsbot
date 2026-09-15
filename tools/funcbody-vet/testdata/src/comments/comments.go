package comments

// want +3 "comment inside a function"
// want +4 "comment inside a function"
func f() int {
    // a comment group
    // of two lines is reported once
    var a = 1 /* a block comment */
    return a
}

// want +1 "comment inside a function"
func g() { // on the opening brace's line
    _ = f()
}

// want +2 "comment inside a function"
var h = func() {
    // inside a package-level literal
}

// want +3 "comment inside a function"
func i() {
    var j = func() {
        // reported once, not once per function around it
        _ = f()
    }
    j()
}

// want +2 "comment inside a function"
func k() {
    /* a block comment

       holding a blank line is a comment, not a blank line */
    _ = f()
}
