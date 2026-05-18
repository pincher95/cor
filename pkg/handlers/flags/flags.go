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
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// GlobalFlags holds the root-level flags that every command shares. Commands
// read them directly from this struct; the extras map returned alongside
// holds only per-command flags.
type GlobalFlags struct {
	Region       string
	Profile      string
	AuthMethod   string
	Delete       bool
	SortBy       string
	SortDesc     bool
	AssumeYes    bool
	OnError      string
	LogFormat    string
	MetricsFile  string
	DryRun       bool
	Format       string
	StateFile    string
	MinCost      float64
	TopN         int
	Rank         bool
	AllRegions   bool
	SaveBaseline string
	DiffBaseline string
}

// FlagRetriever defines an interface for retrieving flags.
type FlagRetriever interface {
	GetString(name string) (string, error)
	GetBool(name string) (bool, error)
	GetInt(name string) (int, error)
	GetFloat64(name string) (float64, error)
	// IsChanged returns true if a flag value was explicitly provided on the CLI.
	// This is used to ensure correct precedence: CLI > config file > env > defaults.
	IsChanged(name string) bool
}

// CommandFlagRetriever is a wrapper around *cobra.Command to implement FlagRetriever.
type CommandFlagRetriever struct {
	Cmd *cobra.Command
}

// Flag represents a flag with its name and type.
type Flag struct {
	Name string
	Type string // "string", "bool", or "int"
}

// GetString retrieves a string flag from the cobra command.
func (r *CommandFlagRetriever) GetString(name string) (string, error) {
	// Prefer local flags, then inherited (persistent from parents), then persistent on this cmd.
	if r.Cmd.Flags().Lookup(name) != nil {
		return r.Cmd.Flags().GetString(name)
	}
	if r.Cmd.InheritedFlags().Lookup(name) != nil {
		return r.Cmd.InheritedFlags().GetString(name)
	}
	return r.Cmd.PersistentFlags().GetString(name)
}

// GetBool retrieves a boolean flag from the cobra command.
func (r *CommandFlagRetriever) GetBool(name string) (bool, error) {
	if r.Cmd.Flags().Lookup(name) != nil {
		return r.Cmd.Flags().GetBool(name)
	}
	if r.Cmd.InheritedFlags().Lookup(name) != nil {
		return r.Cmd.InheritedFlags().GetBool(name)
	}
	return r.Cmd.PersistentFlags().GetBool(name)
}

// GetInt retrieves an integer flag from the cobra command.
func (r *CommandFlagRetriever) GetInt(name string) (int, error) {
	if r.Cmd.Flags().Lookup(name) != nil {
		return r.Cmd.Flags().GetInt(name)
	}
	if r.Cmd.InheritedFlags().Lookup(name) != nil {
		return r.Cmd.InheritedFlags().GetInt(name)
	}
	return r.Cmd.PersistentFlags().GetInt(name)
}

// GetFloat64 retrieves a float64 flag from the cobra command.
func (r *CommandFlagRetriever) GetFloat64(name string) (float64, error) {
	if r.Cmd.Flags().Lookup(name) != nil {
		return r.Cmd.Flags().GetFloat64(name)
	}
	if r.Cmd.InheritedFlags().Lookup(name) != nil {
		return r.Cmd.InheritedFlags().GetFloat64(name)
	}
	return r.Cmd.PersistentFlags().GetFloat64(name)
}

func (r *CommandFlagRetriever) IsChanged(name string) bool {
	// pflag.FlagSet.Changed(name) returns false if the flag is not defined in that set,
	// so we can safely check all relevant sets.
	return r.Cmd.Flags().Changed(name) || r.Cmd.InheritedFlags().Changed(name) || r.Cmd.PersistentFlags().Changed(name)
}

// GetFlags resolves the six baseline global flags into a typed *GlobalFlags
// and the caller-supplied additional flags into a *map[string]any (extras).
// Precedence for every value is CLI > viper (config file + env) > defaults —
// unchanged from the pre-typed version.
func GetFlags(flagRetriever FlagRetriever, additionalFlags []Flag) (*GlobalFlags, *map[string]any, error) {
	getString := func(name string) (string, error) {
		if !flagRetriever.IsChanged(name) && viper.IsSet(name) {
			return viper.GetString(name), nil
		}
		return flagRetriever.GetString(name)
	}
	getBool := func(name string) (bool, error) {
		if !flagRetriever.IsChanged(name) && viper.IsSet(name) {
			return viper.GetBool(name), nil
		}
		return flagRetriever.GetBool(name)
	}
	getFloat64 := func(name string) (float64, error) {
		if !flagRetriever.IsChanged(name) && viper.IsSet(name) {
			return viper.GetFloat64(name), nil
		}
		return flagRetriever.GetFloat64(name)
	}
	getInt := func(name string) (int, error) {
		if !flagRetriever.IsChanged(name) && viper.IsSet(name) {
			return viper.GetInt(name), nil
		}
		return flagRetriever.GetInt(name)
	}

	globals := &GlobalFlags{}
	var err error
	if globals.Region, err = getString("region"); err != nil {
		return nil, nil, err
	}
	if globals.Profile, err = getString("profile"); err != nil {
		return nil, nil, err
	}
	if globals.AuthMethod, err = getString("auth-method"); err != nil {
		return nil, nil, err
	}
	if globals.Delete, err = getBool("delete"); err != nil {
		return nil, nil, err
	}
	if globals.SortBy, err = getString("sort-by"); err != nil {
		return nil, nil, err
	}
	if globals.SortDesc, err = getBool("sort-desc"); err != nil {
		return nil, nil, err
	}
	if globals.AssumeYes, err = getBool("yes"); err != nil {
		return nil, nil, err
	}
	if globals.OnError, err = getString("on-error"); err != nil {
		return nil, nil, err
	}
	if globals.LogFormat, err = getString("log-format"); err != nil {
		return nil, nil, err
	}
	if globals.MetricsFile, err = getString("metrics-file"); err != nil {
		return nil, nil, err
	}
	if globals.DryRun, err = getBool("dry-run"); err != nil {
		return nil, nil, err
	}
	if globals.Format, err = getString("format"); err != nil {
		return nil, nil, err
	}
	if globals.StateFile, err = getString("state-file"); err != nil {
		return nil, nil, err
	}
	if globals.MinCost, err = getFloat64("min-cost"); err != nil {
		return nil, nil, err
	}
	if globals.TopN, err = getInt("top-n"); err != nil {
		return nil, nil, err
	}
	if globals.Rank, err = getBool("rank"); err != nil {
		return nil, nil, err
	}
	if globals.AllRegions, err = getBool("all-regions"); err != nil {
		return nil, nil, err
	}
	if globals.SaveBaseline, err = getString("save-baseline"); err != nil {
		return nil, nil, err
	}
	if globals.DiffBaseline, err = getString("diff-baseline"); err != nil {
		return nil, nil, err
	}

	extras := make(map[string]any, len(additionalFlags))
	for _, f := range additionalFlags {
		switch f.Type {
		case "string":
			v, err := getString(f.Name)
			if err != nil {
				return nil, nil, err
			}
			extras[f.Name] = v
		case "bool":
			v, err := getBool(f.Name)
			if err != nil {
				return nil, nil, err
			}
			extras[f.Name] = v
		case "int":
			if !flagRetriever.IsChanged(f.Name) && viper.IsSet(f.Name) {
				extras[f.Name] = viper.GetInt(f.Name)
				continue
			}
			v, err := flagRetriever.GetInt(f.Name)
			if err != nil {
				return nil, nil, err
			}
			extras[f.Name] = v
		default:
			return nil, nil, fmt.Errorf("unsupported flag type: %s", f.Type)
		}
	}
	return globals, &extras, nil
}
