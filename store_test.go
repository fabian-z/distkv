package distkv

import (
	"bytes"
	"io/ioutil"
	"os"
	"testing"
	"time"
)

func Test_StoreOpen(t *testing.T) {
	s := NewStore(true)
	tmpDir, _ := ioutil.TempDir("", "store_test")
	defer os.RemoveAll(tmpDir)

	s.RaftBind = "127.0.0.1:0"
	s.RaftDir = tmpDir
	if s == nil {
		t.Fatalf("failed to create store")
	}

	if err := s.Open(false); err != nil {
		t.Fatalf("failed to open store: %s", err)
	}
}

func Test_StoreOpenSingleNode(t *testing.T) {
	s := NewStore(true)
	tmpDir, _ := ioutil.TempDir("", "store_test")
	defer os.RemoveAll(tmpDir)

	s.RaftBind = "127.0.0.1:0"
	s.RaftDir = tmpDir
	if s == nil {
		t.Fatalf("failed to create store")
	}

	if err := s.Open(true); err != nil {
		t.Fatalf("failed to open store: %s", err)
	}

	// Simple way to ensure there is a leader.
	time.Sleep(3 * time.Second)

	if err := s.Set("foo", []byte("bar")); err != nil {
		t.Fatalf("failed to set key: %s", err.Error())
	}

	// Wait for committed log entry to be applied.
	time.Sleep(500 * time.Millisecond)
	value, err := s.Get("foo")
	if err != nil {
		t.Fatalf("failed to get key: %s", err.Error())
	}
	if !bytes.Equal(value, []byte("bar")) {
		t.Fatalf("key has wrong value: %s", string(value))
	}

	if err := s.Delete("foo"); err != nil {
		t.Fatalf("failed to delete key: %s", err.Error())
	}

	// Wait for committed log entry to be applied.
	time.Sleep(500 * time.Millisecond)
	value, err = s.Get("foo")
	if err != nil {
		t.Fatalf("failed to get key: %s", err.Error())
	}
	if !bytes.Equal(value, []byte{}) {
		t.Fatalf("key has wrong value: %s", string(value))
	}

}
