package equality

import "testing"

func TestSemantic(t *testing.T) {
	type obj struct {
		Labels map[string]string `json:"labels,omitempty"`
		Items  []int             `json:"items,omitempty"`
		N      *int              `json:"n,omitempty"`
	}
	one := 1
	if !Semantic(obj{}, obj{Labels: map[string]string{}, Items: []int{}}) {
		t.Fatal("nil and empty collections are equal")
	}
	if Semantic(obj{}, obj{N: &one}) || Semantic(obj{Labels: map[string]string{"a": "b"}}, obj{}) {
		t.Fatal("different values are not equal")
	}
}
