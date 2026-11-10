package injection

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestTranslationRendererIsEmbeddedAndUnmodified(t *testing.T) {
	data, asset, err := TranslationRenderer()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty payload")
	}
	// The digest must describe the bytes that are actually injected. A mismatch
	// means the descriptor and the payload drifted apart.
	digest := sha256.Sum256(data)
	if asset.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("digest mismatch: %s", asset.SHA256)
	}
	if asset.Bytes != len(data) {
		t.Fatalf("size mismatch: %d != %d", asset.Bytes, len(data))
	}
	if asset.Version != rendererVersion {
		t.Fatalf("version %d", asset.Version)
	}
}

func TestPayloadDeclaresTheHostProtocol(t *testing.T) {
	data, _, err := TranslationRenderer()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, marker := range requiredMarkers {
		if !contains(text, marker) {
			t.Fatalf("payload does not declare %s", marker)
		}
	}
}

func TestPayloadNeverReadsSecretsFromTheHost(t *testing.T) {
	data, _, err := TranslationRenderer()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	// The renderer runs in the official page. It must not be handed a key, and
	// it must not have a path to the credential store.
	for _, forbidden := range []string{"apiKey", "safeStorage", "require(", "process.env"} {
		if contains(text, forbidden) {
			t.Fatalf("payload references %q", forbidden)
		}
	}
}

func TestDescribeListsEveryAsset(t *testing.T) {
	assets, err := Describe()
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].Name != rendererFile {
		t.Fatalf("unexpected assets: %+v", assets)
	}
}

func contains(haystack, needle string) bool {
	for at := 0; at+len(needle) <= len(haystack); at++ {
		if haystack[at:at+len(needle)] == needle {
			return true
		}
	}
	return false
}
