package protocol

import "testing"

// A provider prefix is stripped only when it is a known legacy alias or a
// well-formed UUID provider id. Anything else belongs to the model's own id.
func TestStripModelAliasPrefix(t *testing.T) {
	const uuid = "byok-3d8250d1-cbbc-4f1f-991e-be98b216f34f/"
	for _, test := range []struct{ name, in, want string }{
		{name: "no prefix", in: "kimi-plain-model", want: "kimi-plain-model"},
		{name: "legacy chat", in: "byok-kth-kimi/kimi-plain-model", want: "kimi-plain-model"},
		{name: "legacy responses", in: "byok-kth-resp/vendor/model", want: "vendor/model"},
		{name: "uuid", in: uuid + "kimi-plain-model", want: "kimi-plain-model"},
		{name: "uuid model with slash", in: uuid + "vendor/model", want: "vendor/model"},
		{name: "uppercase uuid", in: "byok-3D8250D1-CBBC-4F1F-991E-BE98B216F34F/kimi-plain-model",
			want: "kimi-plain-model"},
		{name: "unknown byok prefix", in: "byok-custom/kimi-plain-model", want: "byok-custom/kimi-plain-model"},
		{name: "unknown legacy style prefix", in: "byok-kth-other/kimi-plain-model",
			want: "byok-kth-other/kimi-plain-model"},
		{name: "uuid without slash", in: "byok-3d8250d1-cbbc-4f1f-991e-be98b216f34f",
			want: "byok-3d8250d1-cbbc-4f1f-991e-be98b216f34f"},
		{name: "uuid with short group", in: "byok-3d8250d1-cbbc-4f1f-991e-be98b216f34/kimi",
			want: "byok-3d8250d1-cbbc-4f1f-991e-be98b216f34/kimi"},
		{name: "uuid with non hex group", in: "byok-3d8250d1-cbbc-4f1f-991e-be98b216f34g/kimi",
			want: "byok-3d8250d1-cbbc-4f1f-991e-be98b216f34g/kimi"},
		{name: "bare byok", in: "byok-", want: "byok-"},
		{name: "empty", in: "", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := stripModelAliasPrefix(test.in); got != test.want {
				t.Fatalf("stripModelAliasPrefix(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

// The exported alias list is what a caller renders into the catalog, so its
// contents must not change when the UUID form is added: a client that still sends
// a legacy prefix keeps working.
func TestLegacyAliasesAreUnchanged(t *testing.T) {
	if len(Aliases) != 2 || Aliases[0] != "byok-kth-kimi/" || Aliases[1] != "byok-kth-resp/" {
		t.Fatalf("Aliases = %#v", Aliases)
	}
}
