package config

import (
	"fmt"
	"github.com/crazy-max/gonfig"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"path"
)

func (cfg *Config) Load(app App, configFile string) {
	// Load from file(s)
	if configFile == "" {
		configFile = app.Name + ".yml"
	} else {
		configFile = path.Clean(configFile)
	}
	fileLoader := gonfig.NewFileLoader(gonfig.FileLoaderConfig{
		Filename: configFile,
		Finder: gonfig.Finder{
			BasePaths: []string{
				fmt.Sprintf("/etc/bigbluebutton/%s", app.Name),
				fmt.Sprintf("/etc/%s/%s", app.Name, app.Name),
				fmt.Sprintf("$HOME/.config/%s", app.Name),
				fmt.Sprintf("./%s", app.Name),
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
	envPrefix := "BBBRECORDER_"
	envLoader := gonfig.NewEnvLoader(gonfig.EnvLoaderConfig{
		Prefix: envPrefix,
	})

	if found, err := envLoader.Load(cfg); err != nil {
		log.Fatal(errors.Wrap(err, "failed to decode configuration from environment variables"))
	} else if !found {
		log.Debugf("no %s* environment variables defined", envPrefix)
	} else {
		log.Printf("configuration loaded from %d environment variables", len(envLoader.GetVars()))
	}

	cfg.App = app
}
