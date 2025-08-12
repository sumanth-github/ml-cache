package store

// Store is the minimal key-value store interface used by the scaffold
type Store interface {
	Get(key string) (string, bool)
	Set(key string, value string)
}
