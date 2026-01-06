package flags

import (
	"testing"

	"github.com/spf13/viper"
)

type fakeRetriever struct {
	strings map[string]string
	bools   map[string]bool
	changed map[string]bool
}

func (f *fakeRetriever) GetString(name string) (string, error) { return f.strings[name], nil }
func (f *fakeRetriever) GetBool(name string) (bool, error)     { return f.bools[name], nil }
func (f *fakeRetriever) IsChanged(name string) bool            { return f.changed[name] }

func TestGetFlags_ViperFallbackWhenNotChanged(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set("region", "us-west-2")
	viper.Set("delete", true)

	r := &fakeRetriever{
		strings: map[string]string{"region": "us-east-1"},
		bools:   map[string]bool{"delete": false},
		changed: map[string]bool{}, // nothing explicitly set on CLI
	}

	got, err := GetFlags(r, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if (*got)["region"].(string) != "us-west-2" {
		t.Fatalf("expected region from viper, got %q", (*got)["region"])
	}
	if (*got)["delete"].(bool) != true {
		t.Fatalf("expected delete from viper, got %v", (*got)["delete"])
	}
}

func TestGetFlags_CLIWinsOverViper(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set("profile", "from-config")

	r := &fakeRetriever{
		strings: map[string]string{"profile": "from-cli"},
		bools:   map[string]bool{},
		changed: map[string]bool{"profile": true}, // explicitly set on CLI
	}

	got, err := GetFlags(r, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if (*got)["profile"].(string) != "from-cli" {
		t.Fatalf("expected profile from CLI, got %q", (*got)["profile"])
	}
}

func TestGetFlags_AdditionalFlagsAndUnsupportedType(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	r := &fakeRetriever{
		strings: map[string]string{"filter-by-name": "abc"},
		bools:   map[string]bool{},
		changed: map[string]bool{"filter-by-name": true},
	}

	// additional string flag
	got, err := GetFlags(r, []Flag{{Name: "filter-by-name", Type: "string"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if (*got)["filter-by-name"].(string) != "abc" {
		t.Fatalf("expected additional flag, got %q", (*got)["filter-by-name"])
	}

	// unsupported type
	_, err = GetFlags(r, []Flag{{Name: "x", Type: "int"}})
	if err == nil {
		t.Fatalf("expected error for unsupported type")
	}
}
