package app

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

func initConfig() *config.Config {
	return (&config.Config{App: app}).GetDefaults()
}

func loadConfig() {
	newCfg := initConfig()
	newCfg.Load(app, flags.config)
	*cfg = *newCfg
}

func dumpConfig() {
	var v interface{}
	y, _ := yaml.Marshal(cfg)

	if err := yaml.Unmarshal(y, &v); err != nil {
		log.Fatalf("failed to unmarshal config: %s", err)
	}

	if flags.dump != "all" {
		for _, a := range strings.Split(flags.dump, ".") {
			var i *int
			if n, err := strconv.Atoi(a); err == nil {
				i = &n
			}
			switch v.(type) {
			case []interface{}:
				if i == nil || len(v.([]interface{})) < *i+1 {
					v = nil
					goto _break
				}
				v = v.([]interface{})[*i]
			case map[string]interface{}:
				var ok bool
				if v, ok = v.(map[string]interface{})[a]; !ok {
					v = nil
					goto _break
				}
			default:
				v = nil
				goto _break
			}
			switch v.(type) {
			case []interface{}:
				v = v.([]interface{})
			case map[string]interface{}:
				v = v.(map[string]interface{})
			case string:
				v = v.(string)
			case bool:
				v = v.(bool)
			case int:
				v = v.(int)
			default:
				v = nil
				goto _break
			}

		}
	}
_break:
	if v != nil {
		b, _ := yaml.Marshal(v)
		fmt.Print(string(b))
		os.Exit(0)
	}
	os.Exit(1)
}
