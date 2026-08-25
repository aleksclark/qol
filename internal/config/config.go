package config

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func Bind(command *cobra.Command) (*viper.Viper, error) {
	settings := viper.New()
	settings.SetEnvPrefix("QOL")
	settings.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	settings.AutomaticEnv()
	if err := settings.BindPFlags(command.Flags()); err != nil {
		return nil, fmt.Errorf("bind flags: %w", err)
	}
	if inherited := command.InheritedFlags(); inherited.HasFlags() {
		if err := settings.BindPFlags(inherited); err != nil {
			return nil, fmt.Errorf("bind inherited flags: %w", err)
		}
	}
	return settings, nil
}
