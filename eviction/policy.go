package eviction

type EvictionPolicy interface {
	OnGet(key string)
	OnSet(key string)
	OnDelete(key string)
	ChooseEviction() (key string, ok bool)
}
