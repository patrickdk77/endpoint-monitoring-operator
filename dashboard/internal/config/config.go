package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	HTTPAddr             string     `yaml:"httpAddr"`
	RollupInterval       string     `yaml:"rollupInterval"`
	BackfillPace         string     `yaml:"backfillPace"`
	DefaultRetentionDays int        `yaml:"defaultRetentionDays"`
	Locations            []Location `yaml:"locations"`
}

type Location struct {
	Name     string `yaml:"name"`
	Addr     string `yaml:"addr"`
	TLS      bool   `yaml:"tls"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	Primary  bool   `yaml:"primary"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":8080"
	}
	if c.RollupInterval == "" {
		c.RollupInterval = "5m"
	}
	if c.BackfillPace == "" {
		c.BackfillPace = "10ms"
	}
	if c.DefaultRetentionDays == 0 {
		c.DefaultRetentionDays = 90
	}
	for i := range c.Locations {
		if c.Locations[i].Password == "" {
			if v := os.Getenv("VALKEY_PASSWORD_" + envKey(c.Locations[i].Name)); v != "" {
				c.Locations[i].Password = v
			}
		}
	}
}

func (c *Config) validate() error {
	if len(c.Locations) == 0 {
		return fmt.Errorf("at least one location is required")
	}
	primary := 0
	names := map[string]bool{}
	for _, loc := range c.Locations {
		if loc.Name == "" || loc.Addr == "" {
			return fmt.Errorf("each location requires name and addr")
		}
		if names[loc.Name] {
			return fmt.Errorf("location name %q is used twice", loc.Name)
		}
		names[loc.Name] = true
		if loc.Primary {
			primary++
		}
	}
	if primary != 1 {
		return fmt.Errorf("exactly one location must be marked primary")
	}
	if c.DefaultRetentionDays < 1 {
		return fmt.Errorf("defaultRetentionDays must be at least 1, got %d", c.DefaultRetentionDays)
	}
	interval, err := c.RollupDuration()
	if err != nil {
		return fmt.Errorf("rollupInterval: %w", err)
	}
	if interval <= 0 {
		return fmt.Errorf("rollupInterval must be a positive duration, got %s", c.RollupInterval)
	}
	pace, err := c.BackfillPaceDuration()
	if err != nil {
		return fmt.Errorf("backfillPace: %w", err)
	}
	if pace < 0 {
		return fmt.Errorf("backfillPace must not be negative, got %s", c.BackfillPace)
	}
	return nil
}

func (c *Config) RollupDuration() (time.Duration, error) {
	return time.ParseDuration(c.RollupInterval)
}

func (c *Config) BackfillPaceDuration() (time.Duration, error) {
	return time.ParseDuration(c.BackfillPace)
}

func (c *Config) Primary() *Location {
	for i := range c.Locations {
		if c.Locations[i].Primary {
			return &c.Locations[i]
		}
	}
	return nil
}

func envKey(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '-' {
			out = append(out, '_')
			continue
		}
		if c >= 'a' && c <= 'z' {
			out = append(out, c-'a'+'A')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
