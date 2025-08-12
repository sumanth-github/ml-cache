package store

import (
	"os"
	"testing"

	"github.com/sumanth-kadarla/ml-cache/pkg/wal"
)

func TestBasicSetGet(t *testing.T) {
	os.RemoveAll("./data")
	w, _ := wal.NewWAL("./data/test_wal.log")
	defer w.Close()
	st := NewInMemStore(2, w)
	st.Set("a", "1")
	st.Set("b", "2")
	st.Set("c", "3") // should evict one
	if v, ok := st.Get("c"); !ok || v != "3" {
		t.Fatalf("expected c=3")
	}
}
