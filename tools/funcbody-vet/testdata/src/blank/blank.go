package blank

// want +3 "blank line inside a function"
// want +4 "blank line inside a function"
func f() {

    var a = 1


    _ = a
}

// want +3 "blank line inside a function"
func g() {
    var h = func() {

    }
    h()
}

// want +2 "blank line inside a function"
var k = func() {

}
