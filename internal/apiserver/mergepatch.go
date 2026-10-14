package apiserver

import "encoding/json"

// mergePatch applies an RFC 7386 JSON merge patch to doc.
func mergePatch(doc, patch []byte) ([]byte, error) {
	var d, p any
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(patch, &p); err != nil {
		return nil, err
	}
	return json.Marshal(mergeValue(d, p))
}

func mergeValue(doc, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	dm, ok := doc.(map[string]any)
	if !ok {
		dm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(dm, k)
			continue
		}
		dm[k] = mergeValue(dm[k], v)
	}
	return dm
}
