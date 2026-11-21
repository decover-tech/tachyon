package secrets

import (
	"errors"
	"fmt"
	"time"
)

// Config selects and configures a Store.
type Config struct {
	Backend string // "vault" or "mem"

	VaultAddr      string
	VaultMount     string
	VaultPrefix    string
	VaultNamespace string
	VaultToken     Redacted // from the environment, never a flag
	VaultTokenFile string

	JITTTL     time.Duration // zero means DefaultJITTTL
	DefaultTTL time.Duration // zero means DefaultTTL
}

// Open builds the Store cfg describes.
func Open(cfg Config) (Store, error) {
	p := DefaultPolicy()
	if cfg.JITTTL < 0 || cfg.DefaultTTL < 0 {
		return nil, errors.New("secret TTLs must be positive")
	}
	if cfg.JITTTL > 0 {
		p.Prefixes["jit/"] = cfg.JITTTL
	}
	if cfg.DefaultTTL > 0 {
		p.Default = cfg.DefaultTTL
	}
	switch cfg.Backend {
	case "mem":
		return &Mem{Policy: p}, nil
	case "vault":
		if cfg.VaultAddr == "" {
			return nil, errors.New("vault key store needs an address")
		}
		if len(cfg.VaultToken) == 0 && cfg.VaultTokenFile == "" {
			return nil, errors.New("vault key store needs a token or token file")
		}
		return &Vault{
			Addr:      cfg.VaultAddr,
			Mount:     cfg.VaultMount,
			Prefix:    cfg.VaultPrefix,
			Namespace: cfg.VaultNamespace,
			Token:     cfg.VaultToken,
			TokenFile: cfg.VaultTokenFile,
			Policy:    p,
		}, nil
	default:
		return nil, fmt.Errorf("unknown key store backend %q (want vault or mem)", cfg.Backend)
	}
}
