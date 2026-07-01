package nbdkit

import (
	"fmt"
	"strings"
)

const (
	attrPrefix = "nbdkit.csi.k8s.io/"

	AttrPlugin       = attrPrefix + "plugin"
	AttrFilters      = attrPrefix + "filters"
	AttrSourcePV     = attrPrefix + "source-pv"
	AttrSourceDevice = attrPrefix + "source-device"
	AttrExtraArgs    = attrPrefix + "extra-args"

	// Prefixes for generic key=value pass-through
	PluginParamPrefix = attrPrefix + "param-"
	FilterParamPrefix = attrPrefix + "filter-param-"

	// Placeholder in param values that gets replaced with the source PVC's device path
	SourcePlaceholder = "{source}"
)

type Config struct {
	Plugin       string
	Filters      []string
	SourcePV     string
	SourceDevice string
	PluginArgs   []string
	FilterArgs   []string
	ExtraArgs    []string
}

func ParseVolumeAttributes(attrs map[string]string) (*Config, error) {
	cfg := &Config{}

	if v, ok := attrs[AttrPlugin]; ok {
		cfg.Plugin = v
	}
	if v, ok := attrs[AttrFilters]; ok && v != "" {
		cfg.Filters = strings.Split(v, ",")
	}
	if v, ok := attrs[AttrSourcePV]; ok {
		cfg.SourcePV = v
	}
	if v, ok := attrs[AttrSourceDevice]; ok {
		cfg.SourceDevice = v
	}
	if v, ok := attrs[AttrExtraArgs]; ok && v != "" {
		cfg.ExtraArgs = strings.Split(v, " ")
	}

	if cfg.SourcePV == "" && cfg.SourceDevice == "" && cfg.Plugin == "" {
		return nil, fmt.Errorf("one of plugin, source-pv, or source-device is required")
	}

	// Default plugin when using a source reference
	if (cfg.SourcePV != "" || cfg.SourceDevice != "") && cfg.Plugin == "" {
		cfg.Plugin = "file"
	}

	cfg.PluginArgs = buildArgs(attrs, PluginParamPrefix)
	cfg.FilterArgs = buildArgs(attrs, FilterParamPrefix)

	return cfg, nil
}

func IsOverlay(cfg *Config) bool {
	return cfg.SourcePV != "" || cfg.SourceDevice != ""
}

// ResolveSource replaces {source} placeholders in plugin args with the actual
// device path, or appends the device as a positional arg if no placeholders exist.
func ResolveSource(cfg *Config, devicePath string) {
	replaced := false
	for i, arg := range cfg.PluginArgs {
		if strings.Contains(arg, SourcePlaceholder) {
			cfg.PluginArgs[i] = strings.ReplaceAll(arg, SourcePlaceholder, devicePath)
			replaced = true
		}
	}
	if !replaced {
		cfg.PluginArgs = append(cfg.PluginArgs, devicePath)
	}
}

// buildArgs extracts all volume attributes with the given prefix and converts
// them into key=value arguments for nbdkit.
func buildArgs(attrs map[string]string, prefix string) []string {
	var args []string
	for k, v := range attrs {
		if strings.HasPrefix(k, prefix) {
			paramName := strings.TrimPrefix(k, prefix)
			args = append(args, fmt.Sprintf("%s=%s", paramName, v))
		}
	}
	return args
}
