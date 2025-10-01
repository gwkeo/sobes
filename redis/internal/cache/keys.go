package cache

import (
	"fmt"
	"net/url"
	"strings"
)

// Keys — единая точка именования ключей. Префикс и версия позволяют
// безопасно инвалидировать весь кэш при несовместимом изменении формата:
// достаточно поднять CACHE_KEY_VERSION — старые ключи просто доживут свой TTL.
type Keys struct {
	prefix  string
	version string
}

func NewKeys(prefix, version string) Keys {
	return Keys{prefix: prefix, version: version}
}

func (k Keys) Base() string {
	return k.prefix + ":" + k.version
}

func (k Keys) Product(id string) string {
	return k.Base() + ":product:" + id
}

func (k Keys) ProductList(category string, limit int, listVersion int64) string {
	if category == "" {
		category = "all"
	}
	return fmt.Sprintf("%s:list:%d:%s:%d", k.Base(), listVersion, safeSegment(category), limit)
}

// ListVersion — счётчик, который инкрементится при любом изменении каталога.
// Он входит в ключ списков, поэтому старые списки становятся недостижимы
// без SCAN/KEYS и удаления по шаблону.
func (k Keys) ListVersion() string {
	return k.Base() + ":listver"
}

func (k Keys) Lock(key string) string {
	return k.Base() + ":lock:" + key
}

func (k Keys) EventsChannel() string {
	return k.Base() + ":events"
}

// safeSegment кодирует категорию без потерь, чтобы разные категории
// не превращались в один и тот же ключ.
func safeSegment(s string) string {
	return url.QueryEscape(strings.ToLower(strings.TrimSpace(s)))
}
