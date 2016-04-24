package main

import (
	"bytes"
	"net/rpc"
	"testing"
)

func Test_NewServer(t *testing.T) {
	store := newTestStore()
	s := &testServer{NewRPC(":0", store)}

	if s == nil {
		t.Fatal("failed to create RPC service")
	}

	if err := s.start(); err != nil {
		t.Fatalf("failed to start RPC service: %s", err)
	}

	b := doGet(t, s.Addr(), "k1")
	if !bytes.Equal(b, []byte{}) {
		t.Fatalf("wrong value received for key k1: %s", string(b))
	}

	doSet(t, s.Addr(), "k1", []byte("v1"))

	b = doGet(t, s.Addr(), "k1")
	if !bytes.Equal(b, []byte("v1")) {
		t.Fatalf("wrong value received for key k1: %s", string(b))
	}

	store.m["k2"] = []byte("v2")
	b = doGet(t, s.Addr(), "k2")
	if !bytes.Equal(b, []byte("v2")) {
		t.Fatalf("wrong value received for key k2: %s", string(b))
	}

	doDelete(t, s.Addr(), "k2")
	b = doGet(t, s.Addr(), "k2")
	if !bytes.Equal(b, []byte{}) {
		t.Fatalf("wrong value received for key k2: %s", string(b))
	}

}

type testServer struct {
	*Service
}

func (t *testServer) Addr() string {
	return t.ln.Addr().String()
}

type testStore struct {
	m map[string][]byte
}

func newTestStore() *testStore {
	return &testStore{
		m: make(map[string][]byte),
	}
}

func (t *testStore) Get(key string) ([]byte, error) {
	return t.m[key], nil
}

func (t *testStore) Set(key string, value []byte) error {
	t.m[key] = value
	return nil
}

func (t *testStore) Delete(key string) error {
	delete(t.m, key)
	return nil
}

func (t *testStore) Join(addr string) error {
	return nil
}

func doGet(t *testing.T, addr, key string) []byte {

	client, err := rpc.DialHTTP("tcp", addr)

	if err != nil {
		t.Fatalf("error (%s) dialing %s:\n", err, addr)
	}

	// Synchronous call
	var reply []byte
	err = client.Call("Service.Get", key, &reply)

	if err != nil {
		t.Fatalf("error invoking get %s:\n", err)
	}

	return reply

}

func doSet(t *testing.T, addr, key string, value []byte) {

	client, err := rpc.DialHTTP("tcp", addr)

	if err != nil {
		t.Fatalf("error (%s) dialing %s:\n", err, addr)
	}

	// Synchronous call
	var reply bool
	setRequest := &KeyValue{Key: key, Value: value}

	err = client.Call("Service.Set", setRequest, &reply)

	if err != nil {
		t.Fatalf("error invoking set %s:\n", err)
	}

	if !reply {
		t.Fatalf("received false reply for set request\n")
	}

}

func doDelete(t *testing.T, addr, key string) {

	client, err := rpc.DialHTTP("tcp", addr)

	if err != nil {
		t.Fatalf("error (%s) dialing %s:\n", err, addr)
	}

	// Synchronous call
	var reply bool
	err = client.Call("Service.Delete", key, &reply)

	if err != nil {
		t.Fatalf("error invoking delete %s:\n", err)
	}

	if !reply {
		t.Fatalf("received false reply for delete request\n")
	}

}
