package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/knadh/koanf/maps"
	yaml "gopkg.in/yaml.v3"
)

type config struct {
	ListenPort        int                       `yaml:"listenPort"`
	ListenAddress     string                    `yaml:"listenAddress"`
	ServerConfigs     []*serverConfig           `yaml:"serverConfigs"`
	ServerConfigPath  string                    `yaml:"serverConfigPath"`
	UserDataTemplates map[string]map[string]any `yaml:"userDataTemplates"`

	configPath string
	mu         sync.RWMutex
}

type serverConfig struct {
	Name             string          `yaml:"name"`
	MatchPatterns    []string        `yaml:"matchPatterns"`
	InstanceConfig   *instanceConfig `yaml:"instanceConfig"`
	UserDataTemplate string          `yaml:"userDataTemplate"`
	Replacements     map[string]any  `yaml:"replacements"`

	compiledMatchers []*regexp.Regexp
	renderedUserData []byte
}

type instanceConfig struct {
	Hostname               string `yaml:"hostname"`
	EnableInstanceIDSuffix bool   `yaml:"enableInstanceIDSuffix"`
	EnableHostnameSuffix   bool   `yaml:"enableHostnameSuffix"`
	GeneratedSuffixSize    int    `yaml:"hostnameSuffixSize"`
}

type metaData struct {
	InstanceID    string `yaml:"instance-id"`
	LocalHostname string `yaml:"local-hostname"`
	Hostname      string `yaml:"hostname"`
}

const (
	defaultListenAddress = "0.0.0.0"
	defaultListenPort    = 8000
	defaultSuffixLength  = 4
	yamlHeader           = "#cloud-config\n"
)

func loadConfig(path string) (*config, error) {
	cfg := &config{configPath: path}
	if err := cfg.reload(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *config) validate() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.ServerConfigs) == 0 {
		return fmt.Errorf("config file %q has no serving configurations", c.configPath)
	}
	for _, sc := range c.ServerConfigs {
		if err := sc.loadMatchers(); err != nil {
			return fmt.Errorf("config %q has invalid matchers: %w", sc.Name, err)
		}
		if sc.InstanceConfig == nil {
			return fmt.Errorf("config %q does not have an instanceConfig set", sc.Name)
		}
		if err := sc.InstanceConfig.validate(); err != nil {
			return fmt.Errorf("invalid instance config: %w", err)
		}
		if sc.UserDataTemplate == "" && len(sc.Replacements) > 0 {
			return fmt.Errorf("replacers can only be configured when referencing a user data template")
		}
		userData, ok := c.UserDataTemplates[sc.UserDataTemplate]
		if ok {
			clone := maps.Copy(userData)
			if len(sc.Replacements) > 0 {
				maps.Merge(sc.Replacements, clone)
			}
			by, err := yaml.Marshal(clone)
			if err != nil {
				return fmt.Errorf("render user data after replacements: %w", err)
			}
			sc.renderedUserData = append([]byte(yamlHeader), by...)
		}
	}

	return nil
}

func (c *config) reload() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	by, err := os.ReadFile(c.configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var cfg config
	if err := yaml.Unmarshal(by, &cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	// if serverConfigPath is defined we also read all y(a)ml files from that directory into the serviceConfigs
	if cfg.ServerConfigPath != "" {
		files, err := os.ReadDir(cfg.ServerConfigPath)
		if err != nil {
			log.Printf("WARN: configured ServerConfigPath \"%s\" couldn't be loaded", cfg.ServerConfigPath)
		} else {
			for _, file := range files {
				if !file.IsDir() && (strings.Contains(file.Name(), "yaml") || strings.Contains(file.Name(), "yml")) {

					//serverConfig := &serverConfig{serverConfigPath, file}
					by, err := os.ReadFile(filepath.Join(cfg.ServerConfigPath, file.Name()))
					if err != nil {
						return fmt.Errorf("read serverConfig: %w", err)
					}
					var serverConfig serverConfig
					if err := yaml.Unmarshal(by, &serverConfig); err != nil {
						return fmt.Errorf("parse serverConfig: %w", err)
					}
					cfg.ServerConfigs = append(cfg.ServerConfigs, &serverConfig)
				}

			}
		}

	}
	if err := cfg.validate(); err != nil {
		return fmt.Errorf("validate config: %w", err)
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = defaultListenAddress
	}
	if cfg.ListenPort == 0 {
		cfg.ListenPort = defaultListenPort
	}
	c.UserDataTemplates = cfg.UserDataTemplates
	c.ServerConfigs = cfg.ServerConfigs
	c.ListenAddress = cfg.ListenAddress
	c.ListenPort = cfg.ListenPort

	return nil
}

func (c *config) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, s := range c.ServerConfigs {
		if s.Match(r.URL.Path) {
			log.Printf("%s: returning config %q for: %s", r.RemoteAddr, s.Name, r.URL.Path)
			s.ServeHTTP(w, r)
			return
		}
	}

	log.Printf("WARN: %s: no config found for: %s", r.RemoteAddr, r.URL.Path)
	http.NotFound(w, r)
}

func (c *serverConfig) loadMatchers() error {
	if len(c.MatchPatterns) == 0 {
		log.Printf("INFO:  No matcher configured, fallback to name for %s", c.Name)
		c.MatchPatterns = append(c.MatchPatterns, c.Name)
	}
	for _, m := range c.MatchPatterns {
		re, err := regexp.Compile(m)
		if err != nil {
			return fmt.Errorf("compile pattern %q: %w", m, err)
		}
		c.compiledMatchers = append(c.compiledMatchers, re)
	}
	return nil
}

func (c *serverConfig) Match(s string) bool {
	for _, re := range c.compiledMatchers {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func (c serverConfig) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	split := strings.Split(r.URL.Path, "/")
	switch suffix := split[len(split)-1]; suffix {
	case "meta-data":
		serial := split[len(split)-2]
		by, err := c.InstanceConfig.RenderMetaData(serial)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(by)
	case "user-data":
		w.Write(c.renderedUserData)
	case "vendor-data":
		break
	default:
		log.Printf("WARN: %s: invalid request path: %s", r.RemoteAddr, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (c *instanceConfig) RenderMetaData(serial string) ([]byte, error) {
	md := metaData{
		InstanceID:    "i-" + serial,
		Hostname:      c.Hostname,
		LocalHostname: c.Hostname,
	}
	var suffix string
	if c.EnableHostnameSuffix || c.EnableInstanceIDSuffix {
		s, err := genSuffix(c.GeneratedSuffixSize)
		if err != nil {
			return nil, fmt.Errorf("generate suffix: %w", err)
		}
		suffix = s
	}
	if c.EnableHostnameSuffix {
		md.Hostname += suffix
		md.LocalHostname += suffix
	}
	if c.EnableInstanceIDSuffix {
		md.InstanceID += suffix
	}
	by, err := yaml.Marshal(md)
	if err != nil {
		return nil, fmt.Errorf("render YAML: %w", err)
	}
	return append([]byte(yamlHeader), by...), nil
}

func genSuffix(n int) (string, error) {
	if n <= 0 {
		n = defaultSuffixLength
	}
	by := make([]byte, n)
	if _, err := rand.Read(by); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return "-" + hex.EncodeToString(by), nil
}

func (c *instanceConfig) validate() error {
	if c.Hostname == "" {
		return fmt.Errorf("hostname field must be set")
	}
	return nil
}
