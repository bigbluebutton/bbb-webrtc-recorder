package config

import (
	"fmt"
	"github.com/crazy-max/gonfig"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"path"
	"strings"
)

func (cfg *Config) Load(app, configFile string) {
	// Load from file(s)
	if configFile == "" {
		configFile = app + ".yml"
	} else {
		configFile = path.Clean(configFile)
	}
	fileLoader := gonfig.NewFileLoader(gonfig.FileLoaderConfig{
		Filename: configFile,
		Finder: gonfig.Finder{
			BasePaths: []string{
				fmt.Sprintf("/etc/%s/%s", app, app),
				fmt.Sprintf("$HOME/.config/%s", app),
				fmt.Sprintf("./%s", app),
			},
			Extensions: []string{"yaml", "yml"},
		},
	})
	if found, err := fileLoader.Load(cfg); err != nil {
		log.Fatal(errors.Wrap(err, fmt.Sprintf("failed to decode configuration from file: %s", fileLoader.GetFilename())))
	} else if !found {
		log.Debugf("no configuration file found: %s", fileLoader.GetFilename())
	} else {
		log.Printf("configuration loaded from file: %s", fileLoader.GetFilename())
	}

	// Load from environment variables
	envPrefix := strings.ReplaceAll(app, " ", "_")
	envPrefix = strings.ToUpper(strings.ReplaceAll(envPrefix, "-", "_")) + "_"
	envLoader := gonfig.NewEnvLoader(gonfig.EnvLoaderConfig{
		Prefix: envPrefix,
	})
	if found, err := envLoader.Load(cfg); err != nil {
		log.Fatal(errors.Wrap(err, "Failed to decode configuration from environment variables"))
	} else if !found {
		log.Debugf("No %s* environment variables defined", envPrefix)
	} else {
		log.Printf("Configuration loaded from %d environment variables\n", len(envLoader.GetVars()))
	}

}
