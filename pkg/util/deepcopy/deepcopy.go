// Package deepcopy copies API objects. Kubernetes generates DeepCopy
// methods; a JSON round trip is enough for this project's object sizes.
package deepcopy

import "encoding/json"

// Copy returns an independent copy of obj.
func Copy[T any](obj *T) *T {
	if obj == nil {
		return nil
	}
	data, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}
	out := new(T)
	if err := json.Unmarshal(data, out); err != nil {
		panic(err)
	}
	return out
}
