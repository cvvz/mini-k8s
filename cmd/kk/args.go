package main

import (
	"fmt"
	"strconv"
	"strings"
)

// flagSpec declares one command-line flag.
type flagSpec struct {
	long       string
	short      string
	takesValue bool
}

// args is the result of parsing a command line: positional arguments plus
// flags. Flags may appear anywhere, as in kubectl.
type args struct {
	positional []string
	values     map[string][]string
	bools      map[string]bool
	// rest holds everything after "--" (reserved for exec-style commands).
	rest []string
}

var globalFlags = []flagSpec{
	{long: "namespace", short: "n", takesValue: true},
	{long: "all-namespaces", short: "A"},
}

var commandFlags = map[string][]flagSpec{
	"get":      {{long: "output", short: "o", takesValue: true}, {long: "selector", short: "l", takesValue: true}, {long: "field-selector", takesValue: true}, {long: "watch", short: "w"}},
	"describe": {},
	"apply":    {{long: "filename", short: "f", takesValue: true}},
	"create": {{long: "filename", short: "f", takesValue: true}, {long: "from-literal", takesValue: true},
		{long: "from-file", takesValue: true}, {long: "type", takesValue: true}, {long: "cert", takesValue: true},
		{long: "key", takesValue: true}, {long: "image", takesValue: true}, {long: "from", takesValue: true}},
	"delete": {{long: "filename", short: "f", takesValue: true}, {long: "selector", short: "l", takesValue: true},
		{long: "grace-period", takesValue: true}, {long: "force"}, {long: "cascade", takesValue: true}, {long: "wait"},
		{long: "ignore-not-found"}, {long: "all"}},
	"logs": {{long: "container", short: "c", takesValue: true}, {long: "follow", short: "f"}, {long: "tail", takesValue: true},
		{long: "previous", short: "p"}},
	"exec":     {{long: "container", short: "c", takesValue: true}},
	"scale":    {{long: "replicas", takesValue: true}},
	"rollout":  {{long: "to-revision", takesValue: true}, {long: "timeout", takesValue: true}, {long: "revision", takesValue: true}},
	"cordon":   {},
	"uncordon": {},
	"drain": {{long: "force"}, {long: "delete-emptydir-data"}, {long: "ignore-daemonsets"},
		{long: "grace-period", takesValue: true}, {long: "timeout", takesValue: true}},
	"taint":         {{long: "overwrite"}},
	"label":         {{long: "overwrite"}},
	"api-resources": {},
	"version":       {},
	"cluster":       {{long: "volumes"}, {long: "workers", takesValue: true}},
	"image":         {},
	"help":          {},
}

// findCommand returns the first positional token, skipping global flags.
func findCommand(argv []string) string {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-n" || a == "--namespace":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	return ""
}

func parseArgs(argv []string, specs []flagSpec) (args, error) {
	out := args{values: map[string][]string{}, bools: map[string]bool{}}
	lookup := func(name string, short bool) (flagSpec, bool) {
		for _, s := range specs {
			if (short && s.short == name) || (!short && s.long == name) {
				return s, true
			}
		}
		return flagSpec{}, false
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			out.rest = argv[i+1:]
			break
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			out.positional = append(out.positional, a)
			continue
		}
		short := !strings.HasPrefix(a, "--")
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		spec, ok := lookup(name, short)
		if !ok {
			return out, fmt.Errorf("unknown flag %s", a)
		}
		if !spec.takesValue {
			b := true
			if hasValue {
				parsed, err := strconv.ParseBool(value)
				if err != nil {
					return out, fmt.Errorf("flag %s expects true or false", a)
				}
				b = parsed
			}
			out.bools[spec.long] = b
			continue
		}
		if !hasValue {
			if i+1 >= len(argv) {
				return out, fmt.Errorf("flag %s needs a value", a)
			}
			i++
			value = argv[i]
		}
		out.values[spec.long] = append(out.values[spec.long], value)
	}
	return out, nil
}

// value returns the last value of a flag, or fallback.
func (a args) value(name, fallback string) string {
	if v := a.values[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return fallback
}

// boolean returns a bool flag, or fallback when it was not given.
func (a args) boolean(name string, fallback bool) bool {
	if b, ok := a.bools[name]; ok {
		return b
	}
	return fallback
}
