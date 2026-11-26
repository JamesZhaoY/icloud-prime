package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
)

type pickupTarget struct {
	AccountID string `json:"account_id"`
	Alias     string `json:"alias"`
}

type pickupStore struct {
	mu       sync.RWMutex
	file     string
	bySecret map[string]pickupTarget
	byTarget map[pickupTarget]string
}

func (s *Server) OpenPickupStore(dir string) error {
	store := &pickupStore{
		file: filepath.Join(dir, "pickup_links.json"), bySecret: make(map[string]pickupTarget), byTarget: make(map[pickupTarget]string),
	}
	raw, err := os.ReadFile(store.file)
	if err == nil {
		if err := json.Unmarshal(raw, &store.bySecret); err != nil {
			return err
		}
		if store.bySecret == nil {
			return fmt.Errorf("invalid pickup link store")
		}
		for secret, target := range store.bySecret {
			if !validPickupSecret(secret) || target.AccountID == "" || target.Alias == "" || store.byTarget[target] != "" {
				return fmt.Errorf("invalid pickup link record")
			}
			store.byTarget[target] = secret
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	s.pickups = store
	return nil
}

func validPickupSecret(secret string) bool {
	if len(secret) != 64 {
		return false
	}
	_, err := hex.DecodeString(secret)
	return err == nil
}

func (store *pickupStore) lookup(secret string) (pickupTarget, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	target, exists := store.bySecret[secret]
	return target, exists
}

func (store *pickupStore) issue(targets []pickupTarget, rotate bool) (map[pickupTarget]string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	result := make(map[pickupTarget]string, len(targets))
	var updated map[string]pickupTarget
	for _, target := range targets {
		if result[target] != "" {
			continue
		}
		previous := store.byTarget[target]
		if previous != "" && !rotate {
			result[target] = previous
			continue
		}
		if updated == nil {
			updated = maps.Clone(store.bySecret)
		}
		var secret string
		for {
			var random [32]byte
			if _, err := rand.Read(random[:]); err != nil {
				return nil, err
			}
			secret = hex.EncodeToString(random[:])
			if _, exists := updated[secret]; !exists {
				break
			}
		}
		delete(updated, previous)
		updated[secret] = target
		result[target] = secret
	}
	if updated != nil {
		if err := store.save(updated); err != nil {
			return nil, err
		}
		store.bySecret = updated
		maps.Copy(store.byTarget, result)
	}
	return result, nil
}

func (store *pickupStore) save(items map[string]pickupTarget) error {
	raw, err := json.Marshal(items)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(store.file), ".pickup-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), store.file)
}
