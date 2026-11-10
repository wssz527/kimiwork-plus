package protocol

import (
	"bytes"
	"encoding/json"
	"io"
)

// Document is a JSON object that remembers its own key order and the exact
// bytes of every value it does not touch. The gateway forwards a client's
// payload after rewriting a single field, and the legacy implementation did
// that with `{...payload, model: x}` — which preserves key order and the
// original encoding of every other value. Re-marshalling through a Go map
// would reorder keys and reformat numbers, so the value bytes are kept verbatim
// and only the replaced field is re-encoded.
type Document struct {
	keys   []string
	values map[string]json.RawMessage
}

// ParseDocument decodes one JSON object, keeping the original byte range of
// each member. Anything that is not a plain object is refused: the gateway only
// ever forwards objects.
func ParseDocument(raw []byte) (*Document, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, ErrInvalidRequest
	}
	doc := &Document{values: make(map[string]json.RawMessage)}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := dec.Token()
	if err != nil {
		return nil, ErrInvalidRequest
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, ErrInvalidRequest
	}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return nil, ErrInvalidRequest
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, ErrInvalidRequest
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, ErrInvalidRequest
		}
		if _, seen := doc.values[key]; !seen {
			doc.keys = append(doc.keys, key)
		}
		// A repeated key keeps its first position but takes the last value,
		// which is what JSON.parse does for an object literal.
		doc.values[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, ErrInvalidRequest
	}
	// Trailing content after the closing brace is not a single document.
	if _, err := dec.Token(); err != io.EOF {
		return nil, ErrInvalidRequest
	}
	return doc, nil
}

// Keys returns the member names in their original order.
func (d *Document) Keys() []string {
	out := make([]string, len(d.keys))
	copy(out, d.keys)
	return out
}

// Get returns the raw value for a key.
func (d *Document) Get(key string) (json.RawMessage, bool) {
	value, ok := d.values[key]
	return value, ok
}

// SetString replaces a member with a JSON string, keeping its position, or
// appends it when the key was not present.
func (d *Document) SetString(key, value string) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	d.SetRaw(key, encoded)
}

// SetRaw replaces a member with an already-encoded value.
func (d *Document) SetRaw(key string, value json.RawMessage) {
	if _, ok := d.values[key]; !ok {
		d.keys = append(d.keys, key)
	}
	d.values[key] = value
}

// Delete removes a member.
func (d *Document) Delete(key string) {
	if _, ok := d.values[key]; !ok {
		return
	}
	delete(d.values, key)
	for i, name := range d.keys {
		if name == key {
			d.keys = append(d.keys[:i], d.keys[i+1:]...)
			break
		}
	}
}

// Bytes renders the document with every untouched value exactly as it arrived.
func (d *Document) Bytes() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, key := range d.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(quote(key))
		b.WriteByte(':')
		b.Write(d.values[key])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// Object decodes one raw value as a nested ordered document.
func Object(raw json.RawMessage) (*Document, error) {
	return ParseDocument(raw)
}
