package main

import (
	"fmt"
	"strings"
)

// args holds one command's flags and positional arguments. Flags may come
// before or after the positional ones, since an agent writes them in either
// order; "--" ends the flags.
type args struct {
	pos    []string
	values map[string]string
	bools  map[string]bool
}

// parse reads argv against the flags a command takes: names in valued take
// a value ("--file F" or "--file=F"), names in switches take none.
func parse(argv []string, valued, switches []string) (*args, error) {
	a := &args{values: map[string]string{}, bools: map[string]bool{}}
	isValued := map[string]bool{}
	for _, v := range valued {
		isValued[v] = true
	}
	isSwitch := map[string]bool{}
	for _, s := range switches {
		isSwitch[s] = true
	}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			a.pos = append(a.pos, argv[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			a.pos = append(a.pos, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg[2:], "=")
		switch {
		case isSwitch[name]:
			if hasValue {
				return nil, fmt.Errorf("--%s takes no value", name)
			}
			a.bools[name] = true
		case isValued[name]:
			if !hasValue {
				if i+1 >= len(argv) {
					return nil, fmt.Errorf("--%s needs a value", name)
				}
				i++
				value = argv[i]
			}
			a.values[name] = value
		default:
			return nil, fmt.Errorf("unknown flag --%s", name)
		}
	}
	return a, nil
}

func (a *args) str(name string) string { return a.values[name] }
func (a *args) on(name string) bool    { return a.bools[name] }
