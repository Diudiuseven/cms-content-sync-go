// Package config holds runtime configuration for the sync engine.
package config

import "time"

// Config controls sync, retry and publish behavior.
type Config struct {
	// Retry settings for transient upstream failures.
	MaxRetries    int           `json:"max_retries"`
	RetryBase     time.Duration `json:"retry_base"`
	RetryMax      time.Duration `json:"retry_max"`
	RetryJitter   float64       `json:"retry_jitter"`

	// Sync behavior.
	AutoPublish   bool          `json:"auto_publish"`
	PublishInterval time.Duration `json:"publish_interval"`

	// Locale fallback chain: most specific first, ending with the default.
	Locales       []string      `json:"locales"`
	DefaultLocale string        `json:"default_locale"`
}

// Default returns a configuration with sensible defaults.
func Default() *Config {
	return &Config{
		MaxRetries:      5,
		RetryBase:       10 * time.Millisecond,
		RetryMax:        500 * time.Millisecond,
		RetryJitter:     0.2,
		AutoPublish:     true,
		PublishInterval: 0,
		Locales:         []string{"zh-CN", "zh", "en"},
		DefaultLocale:   "en",
	}
}

// FallbackChain returns the deterministic fallback chain for a locale:
// [locale, language, default]. For example "zh-CN" -> ["zh-CN", "zh", "en"].
func (c *Config) FallbackChain(locale string) []string {
	chain := []string{locale}
	if idx := indexByte(locale, '-'); idx > 0 {
		lang := locale[:idx]
		if lang != locale {
			chain = append(chain, lang)
		}
	}
	if c.DefaultLocale != "" && c.DefaultLocale != locale {
		chain = append(chain, c.DefaultLocale)
	}
	return chain
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
