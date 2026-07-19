package main

import (
	"flag"
	"fmt"
)

// flagSet wraps *flag.FlagSet to add a repeatable string flag (multiString).
type flagSet struct {
	*flag.FlagSet
}

func newFlagSet(name string) *flagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage of %s:\n", name)
		fs.PrintDefaults()
	}
	return &flagSet{fs}
}

// stringSliceValue is a flag.Value that accumulates repeated -flag values.
type stringSliceValue struct {
	values *[]string
}

func (s *stringSliceValue) String() string {
	if s.values == nil || len(*s.values) == 0 {
		return ""
	}
	return fmt.Sprint(*s.values)
}

func (s *stringSliceValue) Set(v string) error {
	*s.values = append(*s.values, v)
	return nil
}

// multiString defines a repeatable string flag and returns a pointer to
// the accumulated slice.
func (f *flagSet) multiString(name, usage string) *[]string {
	var out []string
	f.Var(&stringSliceValue{&out}, name, usage)
	return &out
}
