// Package expansion expands $(VAR) references in environment variable
// values, commands and arguments, like Kubernetes'
// third_party/forked/golang/expansion: a reference to an undefined variable
// stays as written and $$ escapes a dollar sign.
package expansion

import "strings"

// MappingFuncFor returns a mapping that looks a name up in each context in
// turn; unknown names map back to their $(NAME) reference.
func MappingFuncFor(contexts ...map[string]string) func(string) string {
	return func(name string) string {
		for _, vars := range contexts {
			if v, ok := vars[name]; ok {
				return v
			}
		}
		return "$(" + name + ")"
	}
}

// Expand replaces the $(NAME) references in input through mapping.
func Expand(input string, mapping func(string) string) string {
	var b strings.Builder
	for i := 0; i < len(input); i++ {
		if input[i] != '$' || i+1 == len(input) {
			b.WriteByte(input[i])
			continue
		}
		switch next := input[i+1]; next {
		case '$': // an escaped dollar sign
			b.WriteByte('$')
			i++
		case '(':
			end := strings.IndexByte(input[i+2:], ')')
			if end < 0 { // an unterminated reference stays as written
				b.WriteString("$(")
				i++
				continue
			}
			b.WriteString(mapping(input[i+2 : i+2+end]))
			i += 2 + end
		default:
			b.WriteByte('$')
			b.WriteByte(next)
			i++
		}
	}
	return b.String()
}
