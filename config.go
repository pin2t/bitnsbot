package main

import "flag"
import "fmt"
import "os"
import "strings"

func applyConfig(path string) error {
    var data, err = os.ReadFile(path)
    if err != nil { return err }
    var set = make(map[string]bool)
    flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
    for i, line := range strings.Split(string(data), "\n") {
        line = strings.TrimSpace(line)
        if line == "" || strings.HasPrefix(line, "#") { continue }
        var name, value, found = strings.Cut(line, "=")
        if !found { return fmt.Errorf("%s:%d: expected name=value, got %q", path, i+1, line) }
        name = strings.TrimSpace(name)
        value = strings.TrimSpace(value)
        if flag.Lookup(name) == nil { return fmt.Errorf("%s:%d: unknown option %q", path, i+1, name) }
        if set[name] { continue }
        if err := flag.Set(name, value); err != nil { return fmt.Errorf("%s:%d: %v", path, i+1, err) }
    }
    return nil
}
