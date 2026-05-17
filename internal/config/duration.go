package config

import "time"

// CaptureInterval returns the parsed capture interval duration.
func (c *Config) CaptureInterval() time.Duration {
	return parseDurationOrDefault(c.Health.CaptureInterval, 5*time.Second)
}

// IdleTimeout returns the parsed idle timeout duration.
func (c *Config) IdleTimeout() time.Duration {
	return parseDurationOrDefault(c.Health.IdleTimeout, 60*time.Second)
}

// StuckTimeout returns the parsed stuck timeout duration.
func (c *Config) StuckTimeout() time.Duration {
	return parseDurationOrDefault(c.Health.StuckTimeout, 5*time.Minute)
}

func parseDurationOrDefault(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}
