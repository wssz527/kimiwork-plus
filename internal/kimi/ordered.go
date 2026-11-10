package kimi

import (
	"bytes"
	"encoding/json"
)

// orderedObject marshals its members in insertion order.
//
// This is not cosmetic: Kimi's parser walks the catalog with Object.values and
// Object.entries, so the order the gateway emits becomes the order the model
// list is built in. Go's encoding/json sorts map keys, which would silently
// reorder every model the user sees. An explicit order keeps the Go output
// byte-comparable with the original gateway.
type orderedObject struct {
	keys   []string
	values map[string]any
}

func newOrdered() *orderedObject {
	return &orderedObject{values: map[string]any{}}
}

func (o *orderedObject) set(key string, value any) *orderedObject {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
	return o
}

func (o *orderedObject) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			buffer.WriteByte(',')
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		buffer.Write(encodedKey)
		buffer.WriteByte(':')
		encodedValue, err := json.Marshal(o.values[key])
		if err != nil {
			return nil, err
		}
		buffer.Write(encodedValue)
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}
