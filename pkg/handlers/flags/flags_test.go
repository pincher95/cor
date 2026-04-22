/*
Copyright 2024 Cloud Orphaned Resources Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package flags

import (
	"testing"

	"github.com/spf13/viper"
)

type fakeRetriever struct {
	strings map[string]string
	bools   map[string]bool
	ints    map[string]int
	changed map[string]bool
}

func (f *fakeRetriever) GetInt(name string) (int, error) { return f.ints[name], nil }

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

	globals, _, err := GetFlags(r, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if globals.Region != "us-west-2" {
		t.Fatalf("expected region from viper, got %q", globals.Region)
	}
	if globals.Delete != true {
		t.Fatalf("expected delete from viper, got %v", globals.Delete)
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

	globals, _, err := GetFlags(r, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if globals.Profile != "from-cli" {
		t.Fatalf("expected profile from CLI, got %q", globals.Profile)
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
	_, extras, err := GetFlags(r, []Flag{{Name: "filter-by-name", Type: "string"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if (*extras)["filter-by-name"].(string) != "abc" {
		t.Fatalf("expected additional flag, got %q", (*extras)["filter-by-name"])
	}

	// unsupported type
	_, _, err = GetFlags(r, []Flag{{Name: "x", Type: "float"}})
	if err == nil {
		t.Fatalf("expected error for unsupported type")
	}
}

func TestGetFlags_ReturnsTypedGlobals(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	r := &fakeRetriever{
		strings: map[string]string{
			"region":         "us-west-2",
			"profile":        "prod",
			"auth-method":    "ENV_SECRET",
			"sort-by":        "Name",
			"filter-by-name": "x",
		},
		bools: map[string]bool{
			"delete":    true,
			"sort-desc": true,
		},
		changed: map[string]bool{
			"region": true, "profile": true, "auth-method": true,
			"delete": true, "sort-by": true, "sort-desc": true,
			"filter-by-name": true,
		},
	}

	globals, extras, err := GetFlags(r, []Flag{{Name: "filter-by-name", Type: "string"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if globals.Region != "us-west-2" {
		t.Errorf("Region: expected us-west-2, got %q", globals.Region)
	}
	if globals.Profile != "prod" {
		t.Errorf("Profile: expected prod, got %q", globals.Profile)
	}
	if globals.AuthMethod != "ENV_SECRET" {
		t.Errorf("AuthMethod: expected ENV_SECRET, got %q", globals.AuthMethod)
	}
	if !globals.Delete {
		t.Error("Delete: expected true")
	}
	if globals.SortBy != "Name" {
		t.Errorf("SortBy: expected Name, got %q", globals.SortBy)
	}
	if !globals.SortDesc {
		t.Error("SortDesc: expected true")
	}
	if _, ok := (*extras)["region"]; ok {
		t.Error("extras should not contain global 'region'")
	}
	if _, ok := (*extras)["filter-by-name"]; !ok {
		t.Error("extras should contain additional 'filter-by-name'")
	}
	if (*extras)["filter-by-name"].(string) != "x" {
		t.Errorf("extras[filter-by-name]: expected x, got %q", (*extras)["filter-by-name"])
	}
}
