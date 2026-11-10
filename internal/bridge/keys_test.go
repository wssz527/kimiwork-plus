package bridge

import "testing"

type countedKeys struct {
	values map[string]string
	reads  int
}

func (k *countedKeys) Get(ref string) (string, error) {
	k.reads++
	return k.values[ref], nil
}
func (k *countedKeys) Set(ref, value string) error {
	k.values[ref] = value
	return nil
}
func (k *countedKeys) Delete(ref string) error {
	delete(k.values, ref)
	return nil
}

func TestCredentialWriteReadsBackFromTheStore(t *testing.T) {
	store := &countedKeys{values: map[string]string{}}
	keys := CacheKeys(store)
	if err := keys.Set("ref", "new-value"); err != nil {
		t.Fatal(err)
	}
	if store.reads != 0 {
		t.Fatal("test setup read the store unexpectedly")
	}
	actual, err := keys.Get("ref")
	if err != nil || actual != "new-value" || store.reads != 1 {
		t.Fatalf("write was not read back from the store: %q %v reads=%d", actual, err, store.reads)
	}
	if _, err := keys.Get("ref"); err != nil || store.reads != 1 {
		t.Fatal("the verified value was not cached for subsequent reads")
	}
}
